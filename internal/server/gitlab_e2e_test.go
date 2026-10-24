package server

// End-to-end tests for the MCP tool surface against a real GitLab provider: an
// httptest server emulates the GitLab REST API, the real
// internal/provider/gitlab client talks to it, and the tools are driven through
// an in-memory MCP session. Unlike the fakeProvider-based tests, this exercises
// the whole chain: tool -> policy guard -> GitLab client (HTTP) -> mock API ->
// response mapping -> tool JSON, including the .noai marker read.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hvo/mcp-forj/internal/config"
	"github.com/hvo/mcp-forj/internal/policy"
	"github.com/hvo/mcp-forj/internal/provider"
	_ "github.com/hvo/mcp-forj/internal/provider/gitlab" // register the factory
)

// mockGitLab emulates the subset of the GitLab REST API v4 the provider uses.
type mockGitLab struct {
	repo      string
	project   map[string]any
	files     map[string][]byte
	mrs       []map[string]any
	mr        map[string]any
	notes     []map[string]any
	diffs     []map[string]any
	group     map[string][]map[string]any // group path -> projects
	projects  []map[string]any
	branches  map[string]string
	base      string
	forbidden bool
	user      map[string]any
	commits   []map[string]any

	mu       sync.Mutex
	requests []string
	authSeen []string
}

func (m *mockGitLab) record(r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, r.Method+" "+r.URL.Path)
	m.authSeen = append(m.authSeen, r.Header.Get("PRIVATE-TOKEN"))
}

func (m *mockGitLab) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *mockGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.record(r)
	p := strings.TrimPrefix(r.URL.Path, "/api/v4/")
	base := "projects/" + m.repo

	switch {
	case p == "projects":
		m.writeJSON(w, http.StatusOK, m.projects)
	case p == "user":
		m.writeJSON(w, http.StatusOK, m.user)
	case strings.HasPrefix(p, "groups/") && strings.HasSuffix(p, "/projects"):
		group := strings.TrimSuffix(strings.TrimPrefix(p, "groups/"), "/projects")
		m.writeJSON(w, http.StatusOK, m.group[group])
	case p == base:
		m.writeJSON(w, http.StatusOK, m.project)
	case p == base+"/merge_requests":
		if m.forbidden {
			m.writeJSON(w, http.StatusForbidden, map[string]any{"message": "403 Forbidden"})
			return
		}
		m.writeJSON(w, http.StatusOK, m.mrs)
	case strings.HasPrefix(p, base+"/merge_requests/"):
		m.serveMergeRequest(w, r, strings.TrimPrefix(p, base+"/merge_requests/"))
	case strings.HasPrefix(p, base+"/repository/files/"):
		path := strings.TrimPrefix(p, base+"/repository/files/")
		data, ok := m.files[path]
		if !ok {
			m.writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 File Not Found"})
			return
		}
		m.writeJSON(w, http.StatusOK, map[string]any{
			"file_path": path,
			"encoding":  "base64",
			"content":   base64.StdEncoding.EncodeToString(data),
		})
	case p == base+"/repository/commits":
		m.writeJSON(w, http.StatusOK, m.commits)
	case strings.HasPrefix(p, base+"/repository/branches/"):
		branch := strings.TrimPrefix(p, base+"/repository/branches/")
		sha, ok := m.branches[branch]
		if !ok {
			m.writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Branch Not Found"})
			return
		}
		m.writeJSON(w, http.StatusOK, map[string]any{
			"name": branch, "commit": map[string]any{"id": sha},
		})
	case p == base+"/repository/merge_base":
		m.writeJSON(w, http.StatusOK, map[string]any{"id": m.base})
	default:
		m.writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
	}
}

func (m *mockGitLab) serveMergeRequest(w http.ResponseWriter, r *http.Request, rest string) {
	if m.forbidden {
		m.writeJSON(w, http.StatusForbidden, map[string]any{"message": "403 Forbidden"})
		return
	}
	mr := m.mr
	if mr == nil {
		mr = map[string]any{"iid": 1, "title": "t", "state": "opened", "labels": []string{}}
	}
	switch {
	case strings.HasSuffix(rest, "/notes"):
		if r.Method == http.MethodPost {
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.writeJSON(w, http.StatusCreated, map[string]any{
				"id": 99, "body": body.Body, "author": map[string]any{"username": "me"},
			})
			return
		}
		m.writeJSON(w, http.StatusOK, m.notes)
	case strings.HasSuffix(rest, "/diffs"):
		m.writeJSON(w, http.StatusOK, m.diffs)
	case strings.HasSuffix(rest, "/rebase"):
		m.writeJSON(w, http.StatusAccepted, map[string]any{})
	case strings.HasSuffix(rest, "/merge"):
		m.writeJSON(w, http.StatusOK, mr)
	default: // /merge_requests/:iid
		m.writeJSON(w, http.StatusOK, mr)
	}
}

// newGitLabEnv wires a real gitlab provider (through the factory) to the mock
// API and returns an MCP session over the server.
func newGitLabEnv(t *testing.T, rules []policy.RuleSpec, mock *mockGitLab) (*testEnv, *mockGitLab) {
	t.Helper()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	prov, err := provider.New(config.ProviderConfig{
		Name:           "gl",
		Type:           "gitlab",
		BaseURL:        srv.URL,
		Token:          config.Secret("test-token"),
		RequestTimeout: config.Duration(5 * time.Second),
		ProjectScope:   "accessible",
	})
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	pol, err := policy.Build(rules)
	if err != nil {
		t.Fatalf("policy.Build: %v", err)
	}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	guard := policy.NewGuard(
		map[string]*policy.Policy{"gl": pol},
		map[string]policy.FileChecker{"gl": prov},
		".noai",
		logger,
	)
	registry := provider.NewRegistry()
	registry.Register(prov)
	srv2 := New(guard, registry, logger)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv2.MCPServer("test").Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return &testEnv{session: session}, mock
}

func baseProject() map[string]any {
	return map[string]any{
		"id":                  1,
		"path_with_namespace": "team/app",
		"web_url":             "https://gitlab.example.com/team/app",
		"default_branch":      "main",
		"topics":              []string{"ai-ok"},
	}
}

func TestE2EGitLabReadFile(t *testing.T) {
	mock := &mockGitLab{
		repo:    "team/app",
		project: baseProject(),
		files:   map[string][]byte{"README.md": []byte("hello from gitlab\n")},
	}
	env, _ := newGitLabEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{Name: policy.CapRepoRead}},
	}}, mock)

	res := env.call(t, "read_file", map[string]any{"provider": "gl", "repo": "team/app", "path": "README.md"})
	if res.IsError {
		t.Fatalf("read_file denied: %s", resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "hello from gitlab") {
		t.Fatalf("read_file result = %q, want the file content", resultText(t, res))
	}
	if strings.Contains(resultText(t, res), "test-token") {
		t.Error("read_file leaked the provider token")
	}
}

func TestE2EGitLabNoAIMarkerDeniesRead(t *testing.T) {
	mock := &mockGitLab{
		repo:    "team/app",
		project: baseProject(),
		files: map[string][]byte{
			"README.md": []byte("x\n"),
			".noai":     {},
		},
	}
	env, _ := newGitLabEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{Name: policy.CapRepoRead}},
	}}, mock)

	res := env.call(t, "read_file", map[string]any{"provider": "gl", "repo": "team/app", "path": "README.md"})
	if !res.IsError || !strings.Contains(resultText(t, res), "unknown repository") {
		t.Fatalf("read_file = %q (isError=%v), want an indistinguishable unknown-repository denial",
			resultText(t, res), res.IsError)
	}
}

func TestE2EGitLabListMergeRequestsLabelFilter(t *testing.T) {
	mock := &mockGitLab{
		repo:    "team/app",
		project: baseProject(),
		mrs: []map[string]any{
			{"iid": 1, "title": "approved", "state": "opened", "labels": []string{"ai-reviewed"}},
			{"iid": 2, "title": "other", "state": "opened", "labels": []string{"other"}},
			{"iid": 3, "title": "unknown labels", "state": "opened"}, // labels absent -> fail closed
		},
	}
	env, _ := newGitLabEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{
			{Name: policy.CapMRRead, Filter: policy.CapabilityFilter{Require: []string{"ai-reviewed"}}},
		},
	}}, mock)

	res := env.call(t, "list_merge_requests", map[string]any{"provider": "gl", "repo": "team/app"})
	if res.IsError {
		t.Fatalf("list_merge_requests denied: %s", resultText(t, res))
	}
	text := resultText(t, res)
	if !strings.Contains(text, "approved") {
		t.Errorf("result = %q, want the ai-reviewed MR", text)
	}
	if strings.Contains(text, "other") || strings.Contains(text, "unknown labels") {
		t.Errorf("result = %q, want non-matching and unknown-label MRs omitted", text)
	}
}

func TestE2EGitLabGetMergeRequestForbidden(t *testing.T) {
	mock := &mockGitLab{repo: "team/app", project: baseProject(), forbidden: true}
	env, _ := newGitLabEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{Name: policy.CapMRRead}},
	}}, mock)

	res := env.call(t, "get_merge_request", map[string]any{"provider": "gl", "repo": "team/app", "number": 1})
	if !res.IsError {
		t.Fatal("get_merge_request succeeded despite a 403, want a forbidden error")
	}
	if !strings.Contains(resultText(t, res), "forbidden") {
		t.Errorf("error = %q, want it to mention forbidden", resultText(t, res))
	}
}

func TestE2EGitLabListRepositoriesDiscoveryWithTopics(t *testing.T) {
	mock := &mockGitLab{
		repo: "team/app",
		group: map[string][]map[string]any{
			"team": {
				{"id": 1, "path_with_namespace": "team/app", "web_url": "https://x/team/app", "topics": []string{"ai-ok"}},
				{"id": 2, "path_with_namespace": "team/other", "web_url": "https://x/team/other", "topics": []string{}},
			},
		},
	}
	env, _ := newGitLabEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{
			{Name: policy.CapRepoList, Filter: policy.CapabilityFilter{Require: []string{"ai-ok"}}},
		},
	}}, mock)

	res := env.call(t, "list_repositories", map[string]any{"provider": "gl"})
	if res.IsError {
		t.Fatalf("list_repositories denied: %s", resultText(t, res))
	}
	text := resultText(t, res)
	if !strings.Contains(text, "team/app") {
		t.Errorf("result = %q, want team/app (topic ai-ok)", text)
	}
	if strings.Contains(text, "team/other") {
		t.Errorf("result = %q, want team/other omitted (missing topic)", text)
	}
}

func TestE2EGitLabAddNote(t *testing.T) {
	mock := &mockGitLab{repo: "team/app", project: baseProject()}
	env, _ := newGitLabEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{Name: policy.CapMRComment}},
	}}, mock)

	res := env.call(t, "add_merge_request_note", map[string]any{
		"provider": "gl", "repo": "team/app", "number": 1, "body": "looks good",
	})
	if res.IsError {
		t.Fatalf("add_merge_request_note denied: %s", resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "looks good") {
		t.Errorf("result = %q, want the created note body", resultText(t, res))
	}
}

func TestE2EGitLabSearchCommits(t *testing.T) {
	committed := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mock := &mockGitLab{
		repo:    "team/app",
		project: baseProject(),
		user:    map[string]any{"username": "alice", "name": "Alice Example", "email": "alice@example.com"},
		commits: []map[string]any{
			{
				"id": "aaa", "short_id": "aaa", "title": "Fix", "message": "Fix BUG-9",
				"author_name": "Alice", "author_email": "alice@example.com",
				"committed_date": committed.Format(time.RFC3339),
			},
		},
	}
	env, _ := newGitLabEnv(t, []policy.RuleSpec{{
		Repositories: []string{"team/app"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{Name: policy.CapCommitRead}},
	}}, mock)

	res := env.call(t, "search_commits", map[string]any{"provider": "gl"})
	if res.IsError {
		t.Fatalf("search_commits denied: %s", resultText(t, res))
	}
	text := resultText(t, res)
	if !strings.Contains(text, "Fix BUG-9") {
		t.Errorf("result = %q, want the commit message", text)
	}
	if strings.Contains(text, "alice@example.com") {
		t.Errorf("result = %q, must not contain the author email", text)
	}
}
