package gitproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hvo/mcp-forj/internal/policy"
	"github.com/hvo/mcp-forj/internal/provider"
)

const (
	proxyToken   = "s3cret-proxy-token"
	fakeGitAuth  = "Basic dXNlcjpzdHJlYW0tdG9rZW4=" // the provider's own git credential
	providerName = "gl"
	repo         = "team/app"
)

var (
	branchTip  = sha1Old
	notFastFwd = strings.Repeat("e", 40)
)

var packBody = []byte("PACKDATA-BYTES-0123456789\n")

// fakeProvider is a configurable provider.Provider stand-in for proxy tests.
type fakeProvider struct {
	name string
	// marker is what FileExists reports for the .noai check.
	marker bool
	// gitAuth is returned by GitAuthHeader; authCalls counts the lookups.
	gitAuth string
	// remoteBase is the prefix returned by GitRemoteURL (the upstream URL).
	remoteBase string
	// defaultBranch is returned by DefaultBranch.
	defaultBranch string
	// tips maps branch names to resolved SHAs; tipsErr forces an error.
	tips    map[string]string
	tipsErr map[string]error
	// mergeBase is returned by MergeBase; mergeBaseRefs records its arguments.
	mergeBase     string
	mergeBaseErr  error
	mergeBaseRefs []string
	// call counters for fail-closed assertions.
	authCalls      int
	remoteURLCalls int
	mergeBaseCalls int
	resolveCalls   int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Type() string { return "fake" }

func (f *fakeProvider) ListRepositories(context.Context, provider.RepoListOptions) ([]provider.Repository, error) {
	return nil, nil
}
func (f *fakeProvider) GetRepositoryTopics(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeProvider) ListMergeRequests(context.Context, string, provider.ListOptions) ([]provider.MergeRequest, error) {
	return nil, nil
}
func (f *fakeProvider) GetMergeRequest(context.Context, string, int64) (*provider.MergeRequest, error) {
	return nil, provider.ErrNotFound
}
func (f *fakeProvider) ListMergeRequestNotes(context.Context, string, int64, provider.ListOptions) ([]provider.Note, error) {
	return nil, nil
}
func (f *fakeProvider) AddMergeRequestNote(context.Context, string, int64, string) (*provider.Note, error) {
	return nil, nil
}
func (f *fakeProvider) ListMergeRequestDiffs(context.Context, string, int64) ([]provider.DiffFile, error) {
	return nil, nil
}
func (f *fakeProvider) RebaseMergeRequest(context.Context, string, int64) error { return nil }
func (f *fakeProvider) MergeMergeRequest(context.Context, string, int64) (*provider.MergeRequest, error) {
	return nil, nil
}
func (f *fakeProvider) ReadFile(context.Context, string, string, string) ([]byte, error) {
	return nil, provider.ErrNotFound
}
func (f *fakeProvider) FileExists(context.Context, string, string, string) (bool, error) {
	return f.marker, nil
}

func (f *fakeProvider) GitAuthHeader(context.Context, string) (string, error) {
	f.authCalls++
	return f.gitAuth, nil
}

func (f *fakeProvider) GitRemoteURL(context.Context, string) (string, error) {
	f.remoteURLCalls++
	return strings.TrimSuffix(f.remoteBase, "/") + "/" + repo + ".git", nil
}

func (f *fakeProvider) DefaultBranch(context.Context, string) (string, error) {
	return f.defaultBranch, nil
}

func (f *fakeProvider) ResolveRef(_ context.Context, _, ref string) (string, error) {
	f.resolveCalls++
	if err, ok := f.tipsErr[ref]; ok {
		return "", err
	}
	if sha, ok := f.tips[ref]; ok {
		return sha, nil
	}
	return "", provider.ErrNotFound
}

func (f *fakeProvider) MergeBase(_ context.Context, _ string, refs ...string) (string, error) {
	f.mergeBaseCalls++
	f.mergeBaseRefs = append(f.mergeBaseRefs, refs...)
	if f.mergeBaseErr != nil {
		return "", f.mergeBaseErr
	}
	return f.mergeBase, nil
}

// recorded is one request observed by the fake upstream git host.
type recorded struct {
	method        string
	path          string
	rawQuery      string
	authorization string
	contentType   string
	body          []byte
}

// upstream is an httptest server standing in for the provider's git endpoint.
type upstream struct {
	t           *testing.T
	serverURL   string
	mu          sync.Mutex
	requests    []recorded
	status      int
	body        []byte
	contentType string // when non-empty, served as the response Content-Type
	location    string // when set, the response carries this Location header
}

func newUpstream(t *testing.T, body []byte) *upstream {
	t.Helper()
	u := &upstream{t: t, status: http.StatusOK, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read body: %v", err)
		}
		u.mu.Lock()
		u.requests = append(u.requests, recorded{
			method:        r.Method,
			path:          r.URL.Path,
			rawQuery:      r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
			body:          data,
		})
		u.mu.Unlock()
		if u.contentType != "" {
			w.Header().Set("Content-Type", u.contentType)
		}
		if u.location != "" {
			w.Header().Set("Location", u.location)
		}
		w.WriteHeader(u.status)
		_, _ = w.Write(u.body)
	}))
	t.Cleanup(srv.Close)
	u.serverURL = srv.URL
	return u
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *upstream) last() recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		u.t.Fatal("upstream received no request")
	}
	return u.requests[len(u.requests)-1]
}

func newTestServer(t *testing.T, p *fakeProvider, grants []policy.Capability, branches []string) *Server {
	t.Helper()
	caps := make([]policy.CapabilityGrant, len(grants))
	for i, c := range grants {
		caps[i] = policy.CapabilityGrant{Name: c}
	}
	pol, err := policy.Build([]policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       string(policy.EffectAllow),
		Capabilities: caps,
	}})
	if err != nil {
		t.Fatalf("policy.Build: %v", err)
	}
	registry := provider.NewRegistry()
	registry.Register(p)
	guard := policy.NewGuard(
		map[string]*policy.Policy{p.name: pol},
		map[string]policy.FileChecker{p.name: p},
		".noai", nil,
	)
	s, err := New(Config{
		Listen:    "127.0.0.1:0",
		PublicURL: "https://gitproxy.example.com/",
		Token:     proxyToken,
		Branches:  branches,
	}, registry, guard, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func newFetchProvider(remoteBase string) *fakeProvider {
	return &fakeProvider{
		name:          providerName,
		gitAuth:       fakeGitAuth,
		remoteBase:    remoteBase,
		defaultBranch: "main",
		tips:          map[string]string{},
		tipsErr:       map[string]error{},
	}
}

func doRequest(t *testing.T, s *Server, method, target string, body []byte, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func pushBody(oldSHA, newSHA, ref string) []byte {
	return append(pkts(oldSHA+" "+newSHA+" "+ref+"\x00report-status side-band-64k\n"), packBody...)
}

func TestAuthRequired(t *testing.T) {
	u := newUpstream(t, []byte(" advertisement "))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead}, []string{"ai/**"})

	target := "/git/" + providerName + "/" + repo + ".git/info/refs?service=git-upload-pack"

	rec := doRequest(t, s, http.MethodGet, target, nil, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", rec.Code)
	}
	if ch := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(ch, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", ch)
	}

	rec = doRequest(t, s, http.MethodGet, target, nil, "git", "wrong-password")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", rec.Code)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0 on authentication failure", u.count())
	}

	rec = doRequest(t, s, http.MethodGet, target, nil, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Errorf("correct password = %d, want 200", rec.Code)
	}
}

func TestFetchInfoRefsForwardsWithProviderAuth(t *testing.T) {
	ad := []byte("001e# service=git-upload-pack\n0000")
	u := newUpstream(t, ad)
	u.contentType = "application/x-git-upload-pack-advertisement"
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead}, []string{"ai/**"})

	rec := doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/"+repo+".git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), ad) {
		t.Errorf("body = %q, want the advertisement verbatim", rec.Body.Bytes())
	}
	if got := rec.Header().Get("Content-Type"); got != u.contentType {
		t.Errorf("Content-Type = %q, want %q", got, u.contentType)
	}

	got := u.last()
	if got.method != http.MethodGet {
		t.Errorf("upstream method = %q, want GET", got.method)
	}
	if got.path != "/team/app.git/info/refs" {
		t.Errorf("upstream path = %q, want /team/app.git/info/refs", got.path)
	}
	if got.rawQuery != "service=git-upload-pack" {
		t.Errorf("upstream query = %q, want service=git-upload-pack", got.rawQuery)
	}
	// The upstream must see the provider credential, never the proxy token.
	if got.authorization != fakeGitAuth {
		t.Errorf("upstream Authorization = %q, want the provider credential", got.authorization)
	}
	if strings.Contains(got.authorization, proxyToken) {
		t.Error("upstream Authorization contains the proxy token")
	}
	if p.remoteURLCalls == 0 || p.authCalls == 0 {
		t.Error("provider credential/remote lookups were not used for forwarding")
	}
}

func TestFetchUploadPackPostForwardsBody(t *testing.T) {
	u := newUpstream(t, []byte("pack-negotiation-response"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead}, []string{"ai/**"})

	body := []byte("want " + sha1New + "\n")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-upload-pack", body, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := u.last()
	if got.path != "/team/app.git/git-upload-pack" {
		t.Errorf("upstream path = %q", got.path)
	}
	if !bytes.Equal(got.body, body) {
		t.Errorf("upstream body = %q, want %q", got.body, body)
	}
	if got.authorization != fakeGitAuth {
		t.Errorf("upstream Authorization = %q, want the provider credential", got.authorization)
	}
}

func TestRepositoryDeniedByPolicy(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	// repo:read only: every write path must be denied by policy.
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead}, []string{"ai/**"})

	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", pushBody(sha1Zero, sha1New, "refs/heads/ai/fix"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("push without repo:write = %d, want 403", rec.Code)
	}

	// Unmatched repository: denied by the default-deny policy.
	rec = doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/other/secret.git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unlisted repo = %d, want 403", rec.Code)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0 for denied repositories", u.count())
	}
}

func TestNoAIMarkerDenies(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	p.marker = true
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead, policy.CapRepoWrite}, []string{"ai/**"})

	rec := doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/"+repo+".git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf(".noai repo = %d, want 403", rec.Code)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0 for a .noai repository", u.count())
	}
	// The .noai denial is byte-identical to the unknown-repository denial.
	recUnknown := doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/other/secret.git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Body.String() != recUnknown.Body.String() {
		t.Error(".noai denial differs from the unknown-repository denial")
	}
}

func TestReceivePackDiscoveryRequiresWrite(t *testing.T) {
	u := newUpstream(t, []byte("advertisement"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead}, []string{"ai/**"})

	target := "/git/" + providerName + "/" + repo + ".git/info/refs?service=git-receive-pack"
	rec := doRequest(t, s, http.MethodGet, target, nil, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("receive-pack discovery without repo:write = %d, want 403", rec.Code)
	}

	sWrite := newTestServer(t, newFetchProvider(u.serverURL),
		[]policy.Capability{policy.CapRepoWrite}, []string{"ai/**"})
	rec = doRequest(t, sWrite, http.MethodGet, target, nil, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Errorf("receive-pack discovery with repo:write = %d, want 200", rec.Code)
	}
	if got := u.last(); got.rawQuery != "service=git-receive-pack" {
		t.Errorf("upstream query = %q, want service=git-receive-pack", got.rawQuery)
	}
}

func TestPushAllowedCreateForwardsBodyByteExact(t *testing.T) {
	u := newUpstream(t, []byte("000cunpack ok\n0009"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoWrite}, []string{"ai/**"})

	body := pushBody(sha1Zero, sha1New, "refs/heads/ai/fix")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body)
	}
	got := u.last()
	// The command section is parsed for the policy check and replayed
	// verbatim: the upstream must receive the original bytes exactly.
	if !bytes.Equal(got.body, body) {
		t.Errorf("upstream body = %q, want the original %q", got.body, body)
	}
	if got.path != "/team/app.git/git-receive-pack" {
		t.Errorf("upstream path = %q", got.path)
	}
	// A create (all-zero old id) needs no merge-base check.
	if p.mergeBaseCalls != 0 {
		t.Errorf("merge base calls = %d, want 0 for a new branch", p.mergeBaseCalls)
	}
}

func TestPushDeniedCases(t *testing.T) {
	tests := []struct {
		name   string
		old    string
		new    string
		ref    string
		branch string
		tips   map[string]string
		mb     string
		def    string
	}{
		{
			// "main" is allowlisted here so the default-branch rule rejects it,
			// not the allowlist.
			name: "default branch", old: sha1Old, new: sha1New,
			ref: "refs/heads/main", branch: "main", def: "main",
		},
		{
			name: "branch outside allowlist", old: sha1Zero, new: sha1New,
			ref: "refs/heads/feature/x", branch: "ai/**", def: "main",
		},
		{
			name: "delete", old: sha1Old, new: sha1Zero,
			ref: "refs/heads/ai/fix", branch: "ai/**", def: "main",
		},
		{
			name: "non fast-forward", old: sha1Old, new: sha1New,
			ref: "refs/heads/ai/fix", branch: "ai/**",
			tips: map[string]string{"ai/fix": sha1Old}, mb: notFastFwd, def: "main",
		},
		{
			name: "tag ref", old: sha1Zero, new: sha1New,
			ref: "refs/tags/v1", branch: "ai/**", def: "main",
		},
		{
			name: "note ref", old: sha1Zero, new: sha1New,
			ref: "refs/notes/ai", branch: "ai/**", def: "main",
		},
		{
			name: "invalid branch name", old: sha1Zero, new: sha1New,
			ref: "refs/heads/ai/bad..name", branch: "ai/**", def: "main",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, []byte("should not be called"))
			p := newFetchProvider(u.serverURL)
			p.defaultBranch = tc.def
			p.mergeBase = tc.mb
			for k, v := range tc.tips {
				p.tips[k] = v
			}
			s := newTestServer(t, p, []policy.Capability{policy.CapRepoWrite}, []string{tc.branch})

			rec := doRequest(t, s, http.MethodPost,
				"/git/"+providerName+"/"+repo+".git/git-receive-pack",
				pushBody(tc.old, tc.new, tc.ref), "git", proxyToken)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403, body %q", rec.Code, rec.Body)
			}
			if u.count() != 0 {
				t.Errorf("upstream calls = %d, want 0 for a denied push", u.count())
			}
			// The safe message repeats no branch or ref detail.
			if strings.Contains(rec.Body.String(), "refs/") {
				t.Errorf("denial body leaks the ref: %q", rec.Body.String())
			}
		})
	}
}

func TestPushFastForwardAllowed(t *testing.T) {
	u := newUpstream(t, []byte("ok"))
	p := newFetchProvider(u.serverURL)
	p.tips["ai/fix"] = branchTip
	p.mergeBase = branchTip // merge base equals the tip: fast-forward
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoWrite}, []string{"ai/**"})

	body := pushBody(branchTip, sha1New, "refs/heads/ai/fix")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body)
	}
	if !bytes.Equal(u.last().body, body) {
		t.Error("upstream body differs from the original request body")
	}
	want := []string{branchTip, sha1New}
	if !slices.Equal(p.mergeBaseRefs, want) {
		t.Errorf("merge base refs = %v, want %v", p.mergeBaseRefs, want)
	}
}

func TestPushUnresolvableBranchFailsClosed(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	p.tipsErr["ai/fix"] = &provider.HTTPError{Status: http.StatusNotFound, Err: provider.ErrNotFound}
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoWrite}, []string{"ai/**"})

	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Old, sha1New, "refs/heads/ai/fix"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when the branch tip cannot be resolved", rec.Code)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", u.count())
	}
}

func TestPushMergeBaseFailureDenies(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	p.tips["ai/fix"] = sha1Old
	p.mergeBaseErr = errors.New("provider unavailable")
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoWrite}, []string{"ai/**"})

	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Old, sha1New, "refs/heads/ai/fix"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when the merge base is unknown", rec.Code)
	}
	if u.count() != 0 {
		t.Error("upstream was called despite the failed merge-base check")
	}
}

func TestPushMalformedCommandSection(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoWrite}, []string{"ai/**"})

	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		[]byte("this is not a pkt-line stream"), "git", proxyToken)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0 for a malformed push", u.count())
	}
}

func TestRoutingUnsupportedEndpoints(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p,
		[]policy.Capability{policy.CapRepoRead, policy.CapRepoWrite}, []string{"ai/**"})

	tests := []struct {
		name   string
		method string
		target string
	}{
		{"unknown provider", http.MethodGet, "/git/nope/team/app.git/info/refs?service=git-upload-pack"},
		{"dumb protocol objects path", http.MethodGet, "/git/gl/team/app.git/objects/info/packs"},
		{"HEAD request", http.MethodHead, "/git/gl/team/app.git/info/refs?service=git-upload-pack"},
		{"info/refs without service", http.MethodGet, "/git/gl/team/app.git/info/refs"},
		{"info/refs with bogus service", http.MethodGet, "/git/gl/team/app.git/info/refs?service=git-whatever"},
		{"upload-pack via GET", http.MethodGet, "/git/gl/team/app.git/git-upload-pack"},
		{"receive-pack via GET", http.MethodGet, "/git/gl/team/app.git/git-receive-pack"},
		{"missing .git marker", http.MethodGet, "/git/gl/team/app/info/refs?service=git-upload-pack"},
		{"empty repo", http.MethodGet, "/git/gl/.git/info/refs?service=git-upload-pack"},
		{"outside the git prefix", http.MethodGet, "/healthz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, s, tc.method, tc.target, nil, "git", proxyToken)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", tc.method, tc.target, rec.Code)
			}
		})
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0 for unsupported endpoints", u.count())
	}
}

// TestRoutingRejectsURLDelimiters guards the authorization bypass: a
// percent-encoded URL-reserved character decodes into r.URL.Path before the
// route is parsed, and the provider builds the upstream URL from the
// repository name, where a delimiter such as "?" or "#" would truncate the
// path. Policy matching and upstream call must never diverge, so every such
// route is a routing miss (404) and the upstream is never contacted.
func TestRoutingRejectsURLDelimiters(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p,
		[]policy.Capability{policy.CapRepoRead, policy.CapRepoWrite}, []string{"ai/**"})

	delimiters := []struct {
		char string
		enc  string
	}{
		{"?", "%3F"},
		{"#", "%23"},
		{"@", "%40"},
		{":", "%3A"},
		{";", "%3B"},
		{"&", "%26"},
		{"=", "%3D"},
		{"$", "%24"},
		{"+", "%2B"},
		{",", "%2C"},
		{"[", "%5B"},
		{"]", "%5D"},
		{"!", "%21"},
		{"'", "%27"},
		{"(", "%28"},
		{")", "%29"},
		{"*", "%2A"},
	}
	for _, tc := range delimiters {
		t.Run(tc.char, func(t *testing.T) {
			target := "/git/" + providerName + "/team/secret" + tc.enc + "x.git/info/refs?service=git-upload-pack"
			rec := doRequest(t, s, http.MethodGet, target, nil, "git", proxyToken)
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", target, rec.Code)
			}
			if u.count() != 0 {
				t.Errorf("upstream calls = %d, want 0: the route must never reach the provider", u.count())
			}
			if p.remoteURLCalls != 0 || p.authCalls != 0 {
				t.Errorf("provider lookups = remote %d auth %d, want 0/0", p.remoteURLCalls, p.authCalls)
			}
		})
	}
}

func TestUpstreamRedirectIsNotForwarded(t *testing.T) {
	u := newUpstream(t, []byte("moved"))
	u.status = http.StatusFound
	u.location = "http://redirect-target.invalid/"
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead}, []string{"ai/**"})

	rec := doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/"+repo+".git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for an upstream redirect", rec.Code)
	}
	// The client must never see the Location header: following it would send
	// the proxy token to the redirect target.
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want it dropped", loc)
	}
	if strings.Contains(rec.Body.String(), "moved") {
		t.Errorf("body = %q, want the safe 502 text, not the upstream body", rec.Body.String())
	}
}

func TestRemoteURL(t *testing.T) {
	p := newFetchProvider("http://upstream.invalid")
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead}, []string{"ai/**"})

	got, err := s.RemoteURL(providerName, repo)
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	if want := "https://gitproxy.example.com/git/gl/team/app.git"; got != want {
		t.Errorf("RemoteURL = %q, want %q", got, want)
	}
	if strings.Contains(got, proxyToken) || strings.Contains(got, "test-token") {
		t.Errorf("RemoteURL = %q, must be credential-free", got)
	}

	if _, err := s.RemoteURL("nope", repo); err == nil {
		t.Error("RemoteURL accepted an unknown provider")
	}
	bad := []string{
		"", "..", "team/../x", "team//x", "team/app.git", "team/ap p",
		"team/a?b", "team/a#b", "team/a@b", "team/a:b", "team/a%40b",
	}
	for _, badRepo := range bad {
		if _, err := s.RemoteURL(providerName, badRepo); err == nil {
			t.Errorf("RemoteURL accepted invalid repo %q", badRepo)
		}
	}
}

func TestNewRejectsInsecureNonLoopback(t *testing.T) {
	registry := provider.NewRegistry()
	guard := policy.NewGuard(nil, nil, ".noai", nil)
	base := Config{Listen: "0.0.0.0:8417", PublicURL: "https://git.example.com", Token: proxyToken}

	if _, err := New(base, registry, guard, nil); err == nil {
		t.Error("New accepted plain HTTP on a non-loopback listen address")
	}
	// An unparsable listen address counts as non-loopback (fail-safe).
	unparsable := base
	unparsable.Listen = "8417"
	if _, err := New(unparsable, registry, guard, nil); err == nil {
		t.Error("New accepted plain HTTP on an unparsable listen address")
	}
	allowed := base
	allowed.AllowInsecure = true
	if _, err := New(allowed, registry, guard, nil); err != nil {
		t.Errorf("New rejected an explicit allow_insecure: %v", err)
	}
	tls := base
	tls.TLSCert, tls.TLSKey = "/etc/tls/cert.pem", "/etc/tls/key.pem"
	if _, err := New(tls, registry, guard, nil); err != nil {
		t.Errorf("New rejected TLS on a non-loopback address: %v", err)
	}
	loop := base
	loop.Listen = "127.0.0.1:0"
	if _, err := New(loop, registry, guard, nil); err != nil {
		t.Errorf("New rejected plain HTTP on loopback: %v", err)
	}
	halfTLS := loop
	halfTLS.TLSCert = "/etc/tls/cert.pem"
	if _, err := New(halfTLS, registry, guard, nil); err == nil {
		t.Error("New accepted tls_cert without tls_key")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	registry := provider.NewRegistry()
	guard := policy.NewGuard(nil, nil, ".noai", nil)
	base := Config{Listen: "127.0.0.1:0", PublicURL: "https://git.example.com", Token: proxyToken}

	if _, err := New(Config{PublicURL: base.PublicURL, Token: base.Token}, registry, guard, nil); err == nil {
		t.Error("New accepted an empty listen address")
	}
	if _, err := New(Config{Listen: base.Listen, PublicURL: base.PublicURL}, registry, guard, nil); err == nil {
		t.Error("New accepted an empty token")
	}
	if _, err := New(Config{Listen: base.Listen, PublicURL: "git.example.com", Token: base.Token}, registry, guard, nil); err == nil {
		t.Error("New accepted a relative public URL")
	}
	if _, err := New(Config{Listen: base.Listen, PublicURL: base.PublicURL, Token: base.Token,
		Branches: []string{"ai/["}}, registry, guard, nil); err == nil {
		t.Error("New accepted an invalid branch pattern")
	}
	if _, err := New(base, nil, guard, nil); err == nil {
		t.Error("New accepted a nil registry")
	}
	if _, err := New(base, registry, nil, nil); err == nil {
		t.Error("New accepted a nil guard")
	}
	if _, err := New(base, registry, guard, nil); err != nil {
		t.Errorf("New rejected a minimal valid config: %v", err)
	}
}

// TestValidateNameSegmentRejectsReservedChars pins the F1 fix at its source:
// the URL-reserved characters (which percent-escapes decode into r.URL.Path
// before routing) must never pass validation, so policy matching and the
// provider call cannot diverge at a delimiter.
func TestValidateNameSegmentRejectsReservedChars(t *testing.T) {
	for _, seg := range []string{"a?b", "a#b", "a@b", "a:b", "a;b", "a&b", "a=b", "a$b", "a+b", "a,b", "a[b", "a]b", "a'b", "a(b", "a)b", "a*b", "a!b"} {
		if err := validateNameSegment(seg, false); err == nil {
			t.Errorf("validateNameSegment(%q) accepted a URL-reserved character", seg)
		}
		if err := validateNameSegment("team/"+seg, true); err == nil {
			t.Errorf("validateNameSegment(%q) accepted a URL-reserved character in a path", "team/"+seg)
		}
	}
	for _, seg := range []string{"app", "app-1", "app_2", "a.b", "team/sub/app"} {
		if err := validateNameSegment(seg, true); err != nil {
			t.Errorf("validateNameSegment(%q) rejected a valid path: %v", seg, err)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8417", "localhost:8417", "[::1]:8417"} {
		if !isLoopbackAddr(addr) {
			t.Errorf("isLoopbackAddr(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0:8417", ":8417", "192.168.1.10:8417", "[::]:8417"} {
		if isLoopbackAddr(addr) {
			t.Errorf("isLoopbackAddr(%q) = true, want false", addr)
		}
	}
}

func TestCheckRefUpdateBranchNameValidation(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	// A broad allowlist so only the name validation can reject.
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoWrite}, []string{"**"})

	for _, branch := range []string{"-lead", ".dot", "trail.", "a//b", "seg/../x", "with space", "with~tilde"} {
		if err := s.checkRefUpdate(context.Background(), p, repo,
			RefUpdate{OldSHA: sha1Zero, NewSHA: sha1New, Ref: "refs/heads/" + branch}); err == nil {
			t.Errorf("branch %q accepted, want rejection", branch)
		}
	}
	for _, branch := range []string{"ai/fix", "feature/x-1", "rel-1.2"} {
		if err := s.checkRefUpdate(context.Background(), p, repo,
			RefUpdate{OldSHA: sha1Zero, NewSHA: sha1New, Ref: "refs/heads/" + branch}); err != nil {
			t.Errorf("branch %q rejected: %v, want acceptance", branch, err)
		}
	}
}

// freePort reserves an ephemeral loopback port and releases it for reuse.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	return addr
}

func TestListenAndServeGracefulShutdown(t *testing.T) {
	u := newUpstream(t, []byte("advertisement"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead}, []string{"ai/**"})
	s.cfg.Listen = freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()

	target := "http://" + s.cfg.Listen + "/git/" + providerName + "/" + repo + ".git/info/refs?service=git-upload-pack"
	client := &http.Client{Timeout: 2 * time.Second}
	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.SetBasicAuth("git", proxyToken)
		resp, err = client.Do(req)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp == nil {
		cancel()
		t.Fatal("proxy never answered on the listen address")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("served status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ListenAndServe returned %v, want nil after cancellation", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ListenAndServe did not shut down after cancellation")
	}
}
