//go:build e2e

package gitproxy

// End-to-end tests for the git smart-HTTP proxy. Unlike proxy_test.go, which
// hand-crafts pkt-line bodies against a request-recording httptest upstream,
// these tests run a real git client against the proxy and a real git backend:
// a local bare repository served over git smart-HTTP by `git http-backend`
// (in-process CGI). That exercises the protocol paths the unit tests cannot:
// ref discovery, packfile streaming, the client auth challenge, and the real
// push negotiation the proxy parses.
//
// They are behind the "e2e" build tag (run with `go test -tags e2e ./...` or
// `make test-e2e`) and skip when the git binary is unavailable or -short is set.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haininghu/mcp-forj/internal/policy"
	"github.com/haininghu/mcp-forj/internal/provider"
)

const e2eToken = "e2e-proxy-token"

// gitProvider is a provider.Provider backed by a local bare repository. It
// answers the metadata calls the proxy needs (default branch, refs, merge base,
// marker file) by shelling out to git, so the push guardrails run against real
// history instead of canned values.
type gitProvider struct {
	name     string
	bareRoot string // GIT_PROJECT_ROOT of the http-backend server
	remote   string // base URL of the http-backend server
}

func (p *gitProvider) Name() string { return p.name }
func (p *gitProvider) Type() string { return "git" }

func (p *gitProvider) bare(repo string) string {
	return filepath.Join(p.bareRoot, filepath.FromSlash(repo)+".git")
}

func (p *gitProvider) git(repo string, args ...string) (string, error) {
	full := append([]string{"-C", p.bare(repo)}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (p *gitProvider) CanonicalRepository(repo string) (string, error) {
	if !strings.Contains(repo, "/") {
		return "", errors.New("must be a namespaced path")
	}
	return strings.ToLower(repo), nil
}

func (p *gitProvider) GitRemoteURL(_ context.Context, repo string) (string, error) {
	return p.remote + "/" + strings.ToLower(repo) + ".git", nil
}

func (p *gitProvider) GitAuthHeader(context.Context, string) (string, error) {
	return "Basic dXNlcjpwYXNz", nil
}

func (p *gitProvider) DefaultBranch(_ context.Context, repo string) (string, error) {
	return p.git(repo, "symbolic-ref", "--short", "HEAD")
}

func (p *gitProvider) ResolveRef(_ context.Context, repo, branch string) (string, error) {
	sha, err := p.git(repo, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return "", provider.ErrNotFound
	}
	return sha, nil
}

func (p *gitProvider) MergeBase(_ context.Context, repo string, refs ...string) (string, error) {
	return p.git(repo, append([]string{"merge-base"}, refs...)...)
}

func (p *gitProvider) FileExists(_ context.Context, repo, path, ref string) (bool, error) {
	if ref == "" {
		ref = "HEAD"
	}
	if _, err := p.git(repo, "rev-parse", "--verify", ref); err != nil {
		// An unknown ref has no marker; the proxy denies on a present marker, so
		// treating an unresolvable ref as "absent" keeps the test provider
		// simple. Production providers return a fail-closed error here.
		return false, nil
	}
	if _, err := p.git(repo, "cat-file", "-e", ref+":"+path); err != nil {
		return false, nil
	}
	return true, nil
}

func (p *gitProvider) ReadFile(_ context.Context, repo, path, ref string) ([]byte, error) {
	if ref == "" {
		ref = "HEAD"
	}
	out, err := p.git(repo, "cat-file", "-p", ref+":"+path)
	if err != nil {
		return nil, provider.ErrNotFound
	}
	return []byte(out), nil
}

func (p *gitProvider) ListRepositories(context.Context, provider.RepoListOptions) ([]provider.Repository, error) {
	return nil, nil
}
func (p *gitProvider) GetRepositoryTopics(context.Context, string) ([]string, error) { return nil, nil }
func (p *gitProvider) ListCommits(context.Context, string, provider.CommitListOptions) ([]provider.Commit, bool, error) {
	return nil, false, nil
}
func (p *gitProvider) CurrentUser(context.Context) (provider.User, error) {
	return provider.User{}, nil
}
func (p *gitProvider) ListMergeRequests(context.Context, string, provider.ListOptions) ([]provider.MergeRequest, error) {
	return nil, nil
}
func (p *gitProvider) GetMergeRequest(context.Context, string, int64) (*provider.MergeRequest, error) {
	return nil, provider.ErrNotFound
}
func (p *gitProvider) ListMergeRequestNotes(context.Context, string, int64, provider.ListOptions) ([]provider.Note, error) {
	return nil, nil
}
func (p *gitProvider) AddMergeRequestNote(context.Context, string, int64, string) (*provider.Note, error) {
	return nil, nil
}
func (p *gitProvider) ListMergeRequestDiffs(context.Context, string, int64) ([]provider.DiffFile, error) {
	return nil, nil
}
func (p *gitProvider) RebaseMergeRequest(context.Context, string, int64) error { return nil }
func (p *gitProvider) MergeMergeRequest(context.Context, string, int64) (*provider.MergeRequest, error) {
	return nil, nil
}
func (p *gitProvider) UpdateMergeRequestLabels(context.Context, string, int64, []string, []string) (*provider.MergeRequest, error) {
	return nil, nil
}

// e2eEnv is one proxy under test plus its git backend and bare-repo root.
type e2eEnv struct {
	root     string
	provider *gitProvider
	proxy    *httptest.Server
}

// newE2EEnv creates a bare-repo root, runs seed against it, then starts the
// git http-backend and the proxy. It skips when git is unavailable.
func newE2EEnv(t *testing.T, rules []policy.RuleSpec, seed func(root string)) *e2eEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping git e2e test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	root := t.TempDir()
	seed(root)
	backend := newGitBackend(t, root)
	p := &gitProvider{name: "gl", bareRoot: root, remote: backend.URL}

	pol, err := policy.Build(rules)
	if err != nil {
		t.Fatalf("policy.Build: %v", err)
	}
	guard := policy.NewGuard(
		map[string]*policy.Policy{p.name: pol},
		map[string]policy.FileChecker{p.name: p},
		".noai",
		nil,
	)
	registry := provider.NewRegistry()
	registry.Register(p)
	ps, err := New(Config{
		Listen:    "127.0.0.1:0",
		PublicURL: "http://proxy.invalid",
		Token:     e2eToken,
	}, registry, guard, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	proxy := httptest.NewServer(ps.Handler())
	t.Cleanup(proxy.Close)
	return &e2eEnv{root: root, provider: p, proxy: proxy}
}

// newGitBackend serves git smart-HTTP for the bare repositories under root.
func newGitBackend(t *testing.T, root string) *httptest.Server {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not available")
	}
	h := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env: append(os.Environ(),
			"GIT_PROJECT_ROOT="+root,
			"GIT_HTTP_EXPORT_ALL=1",
		),
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// cloneURL builds the credential-carrying proxy URL for provider/repo.
func (e *e2eEnv) cloneURL(t *testing.T, provider, repo string) string {
	t.Helper()
	u, err := url.Parse(e.proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	u.User = url.UserPassword("any", e2eToken)
	u.Path = "/git/" + provider + "/" + repo + ".git"
	return u.String()
}

// branchSHA returns the SHA of a branch in the bare repo, or ok=false.
func (e *e2eEnv) branchSHA(t *testing.T, repo, branch string) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "-C", e.provider.bare(repo), "rev-parse", "--verify", "refs/heads/"+branch).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// --- git helpers -----------------------------------------------------------

func gitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=e2e",
		"GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=e2e",
		"GIT_COMMITTER_EMAIL=e2e@example.com",
	)
}

// git runs git in dir and returns its combined output and error.
func git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(t)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustGit runs git and fails the test on a non-zero exit.
func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// seedRepo creates <root>/<repo>.git with an initial commit on main carrying
// files, and enables receive-pack over http.
func seedRepo(t *testing.T, root, repo string, files map[string]string) {
	t.Helper()
	bare := filepath.Join(root, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGit(t, root, "init", "--bare", "--initial-branch=main", bare)
	mustGit(t, bare, "config", "http.receivepack", "true")
	mustGit(t, bare, "config", "receive.advertisePushOptions", "true")

	work := t.TempDir()
	mustGit(t, work, "init", "--initial-branch=main")
	for name, content := range files {
		writeFile(t, work, name, content)
	}
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-m", "init")
	mustGit(t, work, "remote", "add", "origin", bare)
	mustGit(t, work, "push", "origin", "main")
}

// seedBranch clones the bare repo, creates branch with files and pushes it.
func seedBranch(t *testing.T, root, repo, branch string, files map[string]string) {
	t.Helper()
	bare := filepath.Join(root, filepath.FromSlash(repo)+".git")
	work := t.TempDir()
	mustGit(t, work, "clone", bare, ".")
	mustGit(t, work, "checkout", "-b", branch)
	for name, content := range files {
		writeFile(t, work, name, content)
	}
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-m", "seed "+branch)
	mustGit(t, work, "push", "origin", branch)
}

// cloneThroughProxy clones provider/repo into a fresh directory and returns it.
func (e *e2eEnv) cloneThroughProxy(t *testing.T, provider, repo string) (string, error) {
	t.Helper()
	work := t.TempDir()
	out, err := git(t, work, "clone", e.cloneURL(t, provider, repo), ".")
	return out, err
}

// --- tests -----------------------------------------------------------------

func TestE2ECloneRequiresRepoRead(t *testing.T) {
	env := newE2EEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{Name: policy.CapRepoRead}},
	}}, func(root string) {
		seedRepo(t, root, "team/app", map[string]string{"README.md": "hello e2e\n"})
	})

	work := t.TempDir()
	if out, err := git(t, work, "clone", env.cloneURL(t, "gl", "team/app"), "."); err != nil {
		t.Fatalf("clone with repo:read failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(work, "README.md"))
	if err != nil || string(data) != "hello e2e\n" {
		t.Fatalf("cloned README = %q (%v), want %q", data, err, "hello e2e\n")
	}
}

func TestE2ECloneDeniedWithoutRepoRead(t *testing.T) {
	env := newE2EEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{Name: policy.CapMRRead}},
	}}, func(root string) {
		seedRepo(t, root, "team/app", map[string]string{"README.md": "secret\n"})
	})

	work := t.TempDir()
	out, err := git(t, work, "clone", env.cloneURL(t, "gl", "team/app"), ".")
	if err == nil {
		t.Fatal("clone succeeded without repo:read, want a denial")
	}
	if !strings.Contains(out, "403") {
		t.Errorf("clone output = %q, want it to mention HTTP 403", out)
	}
}

func TestE2EPushBranchPolicy(t *testing.T) {
	env := newE2EEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{
			{Name: policy.CapRepoRead},
			{Name: policy.CapRepoWrite, Filter: policy.CapabilityFilter{
				Branches: policy.PathFilter{Include: []string{"ai/**"}},
			}},
		},
	}}, func(root string) {
		seedRepo(t, root, "team/app", map[string]string{"README.md": "base\n"})
	})

	work := t.TempDir()
	if out, err := git(t, work, "clone", env.cloneURL(t, "gl", "team/app"), "."); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	writeFile(t, work, "feature.txt", "one\n")
	mustGit(t, work, "add", "-A")
	mustGit(t, work, "commit", "-m", "feat")

	t.Run("allowed branch", func(t *testing.T) {
		if out, err := git(t, work, "push", "origin", "HEAD:refs/heads/ai/x"); err != nil {
			t.Fatalf("push to ai/x denied: %v\n%s", err, out)
		}
		if _, ok := env.branchSHA(t, "team/app", "ai/x"); !ok {
			t.Error("ai/x was not created in the bare repo")
		}
	})

	t.Run("branch outside the filter", func(t *testing.T) {
		out, err := git(t, work, "push", "origin", "HEAD:refs/heads/feature/x")
		if err == nil {
			t.Fatal("push to feature/x succeeded, want a denial")
		}
		if !strings.Contains(out, "403") {
			t.Errorf("push output = %q, want HTTP 403", out)
		}
	})

	t.Run("default branch is hard-blocked", func(t *testing.T) {
		// Even a filter that would allow main never authorizes the default branch.
		out, err := git(t, work, "push", "origin", "HEAD:refs/heads/main")
		if err == nil {
			t.Fatal("push to the default branch succeeded, want a denial")
		}
		if !strings.Contains(out, "403") {
			t.Errorf("push output = %q, want HTTP 403", out)
		}
	})

	t.Run("delete is denied", func(t *testing.T) {
		out, err := git(t, work, "push", "origin", "--delete", "ai/x")
		if err == nil {
			t.Fatal("branch delete succeeded, want a denial")
		}
		if !strings.Contains(out, "403") {
			t.Errorf("delete output = %q, want HTTP 403", out)
		}
	})

	t.Run("force push is denied", func(t *testing.T) {
		// ai/x exists from the allowed push above; rewrite its single commit.
		mustGit(t, work, "checkout", "-B", "ai/x", "origin/ai/x")
		writeFile(t, work, "feature.txt", "rewritten\n")
		mustGit(t, work, "add", "-A")
		mustGit(t, work, "commit", "--amend", "--no-edit")
		out, err := git(t, work, "push", "--force", "origin", "HEAD:refs/heads/ai/x")
		if err == nil {
			t.Fatal("force push succeeded, want a denial")
		}
		if !strings.Contains(out, "403") {
			t.Errorf("force push output = %q, want HTTP 403", out)
		}
	})
}

func TestE2ENoAIMarker(t *testing.T) {
	t.Run("default branch denies clone", func(t *testing.T) {
		env := newE2EEnv(t, []policy.RuleSpec{{
			Repositories: []string{"team/*"},
			Effect:       "allow",
			Capabilities: []policy.CapabilityGrant{{Name: policy.CapRepoRead}},
		}}, func(root string) {
			seedRepo(t, root, "team/app", map[string]string{"README.md": "x\n", ".noai": ""})
		})
		work := t.TempDir()
		out, err := git(t, work, "clone", env.cloneURL(t, "gl", "team/app"), ".")
		if err == nil {
			t.Fatal("clone of a .noai repository succeeded, want a denial")
		}
		if !strings.Contains(out, "403") {
			t.Errorf("clone output = %q, want HTTP 403", out)
		}
	})

	t.Run("target branch marker denies push", func(t *testing.T) {
		env := newE2EEnv(t, []policy.RuleSpec{{
			Repositories: []string{"team/*"},
			Effect:       "allow",
			Capabilities: []policy.CapabilityGrant{
				{Name: policy.CapRepoRead},
				{Name: policy.CapRepoWrite, Filter: policy.CapabilityFilter{
					Branches: policy.PathFilter{Include: []string{"ai/**"}},
				}},
			},
		}}, func(root string) {
			seedRepo(t, root, "team/app", map[string]string{"README.md": "x\n"})
			seedBranch(t, root, "team/app", "ai/noai", map[string]string{".noai": ""})
		})
		work := t.TempDir()
		if out, err := git(t, work, "clone", env.cloneURL(t, "gl", "team/app"), "."); err != nil {
			t.Fatalf("clone: %v\n%s", err, out)
		}
		// Base the commit on the target branch so the push is a fast-forward and
		// actually reaches the proxy's marker check (a non-fast-forward would be
		// refused by the git client before any request).
		mustGit(t, work, "checkout", "ai/noai")
		writeFile(t, work, "change.txt", "y\n")
		mustGit(t, work, "add", "-A")
		mustGit(t, work, "commit", "-m", "change")
		out, err := git(t, work, "push", "origin", "HEAD:refs/heads/ai/noai")
		if err == nil {
			t.Fatal("push to a .noai target branch succeeded, want a denial")
		}
		if !strings.Contains(out, "403") {
			t.Errorf("push output = %q, want HTTP 403", out)
		}
	})
}

func TestE2EPushOptions(t *testing.T) {
	rules := func(caps ...policy.CapabilityGrant) []policy.RuleSpec {
		return []policy.RuleSpec{{
			Repositories: []string{"team/*"},
			Effect:       "allow",
			Capabilities: append([]policy.CapabilityGrant{{Name: policy.CapRepoRead}}, caps...),
		}}
	}
	write := policy.CapabilityGrant{Name: policy.CapRepoWrite, Filter: policy.CapabilityFilter{
		Branches: policy.PathFilter{Include: []string{"ai/**"}},
	}}

	tests := []struct {
		name    string
		caps    []policy.CapabilityGrant
		option  string
		wantErr bool
	}{
		{"mr:write gates merge_request.create", []policy.CapabilityGrant{write, {Name: policy.CapMRWrite}}, "merge_request.create", false},
		{"merge_request.create without mr:write", []policy.CapabilityGrant{write}, "merge_request.create", true},
		{"auto_merge needs mr:merge", []policy.CapabilityGrant{write, {Name: policy.CapMRWrite}}, "merge_request.auto_merge", true},
		{"auto_merge with mr:merge", []policy.CapabilityGrant{write, {Name: policy.CapMRWrite}, {Name: policy.CapMRMerge}}, "merge_request.auto_merge", false},
		{"ci option is always denied", []policy.CapabilityGrant{write, {Name: policy.CapMRWrite}}, "ci.skip", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newE2EEnv(t, rules(tt.caps...), func(root string) {
				seedRepo(t, root, "team/app", map[string]string{"README.md": "x\n"})
			})
			work := t.TempDir()
			if out, err := git(t, work, "clone", env.cloneURL(t, "gl", "team/app"), "."); err != nil {
				t.Fatalf("clone: %v\n%s", err, out)
			}
			writeFile(t, work, "change.txt", tt.name+"\n")
			mustGit(t, work, "add", "-A")
			mustGit(t, work, "commit", "-m", "change")
			out, err := git(t, work, "push", "-o", tt.option, "origin", "HEAD:refs/heads/ai/x")
			if tt.wantErr && err == nil {
				t.Fatalf("push with -o %s succeeded, want a denial\n%s", tt.option, out)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("push with -o %s denied: %v\n%s", tt.option, err, out)
			}
		})
	}
}
