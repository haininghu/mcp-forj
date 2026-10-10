package gitproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
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
	// marker is what FileExists reports for the .noai check, regardless of ref
	// (the blunt switch used by the existing tests).
	marker bool
	// markersByRef makes FileExists ref-accurate: a present key returns its value
	// for that ref ("" is the default branch), overriding nothing when marker is
	// already true. It lets a test place the marker only on a push target branch.
	markersByRef map[string]bool
	// fileExistsRefs records every ref FileExists was asked about, in call order,
	// so tests can pin that a marker is checked on both the default branch and the
	// target branch (and that duplicate targets are deduplicated).
	fileExistsRefs []string
	// gitAuth is returned by GitAuthHeader; authCalls counts the lookups.
	gitAuth string
	// remoteBase is the prefix returned by GitRemoteURL (the upstream URL).
	remoteBase string
	// defaultBranch is returned by DefaultBranch; defaultBranchCalls counts it so a
	// test can prove authorization precedes the provider metadata reads.
	defaultBranch      string
	defaultBranchCalls int
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
func (f *fakeProvider) FileExists(_ context.Context, _, _, ref string) (bool, error) {
	f.fileExistsRefs = append(f.fileExistsRefs, ref)
	if f.marker {
		return true, nil
	}
	// Ref-accurate markers: a per-ref value denies only that ref, so a marker on a
	// push target branch is invisible on the (clean) default branch and vice versa.
	return f.markersByRef[ref], nil
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
	f.defaultBranchCalls++
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
	header        http.Header // full clone, for absence assertions
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
			header:        r.Header.Clone(),
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

// newTestServer builds a Server whose team/* allow rule grants exactly the given
// capabilities with no filter (no branch, no path, no tag, not .noai-exempt).
func newTestServer(t *testing.T, p *fakeProvider, grants []policy.Capability) *Server {
	t.Helper()
	caps := make([]policy.CapabilityGrant, len(grants))
	for i, c := range grants {
		caps[i] = policy.CapabilityGrant{Name: c}
	}
	return newTestServerGrants(t, p, caps)
}

// newTestServerGrants builds a Server from explicit capability grants so a test
// can attach a filter (the repo:write branches filter, the noai: allow
// exemption, or a path/tag constraint whose behavior in the proxy is
// fail-closed).
func newTestServerGrants(t *testing.T, p *fakeProvider, grants []policy.CapabilityGrant) *Server {
	t.Helper()
	return newTestServerGrantsToken(t, p, grants, proxyToken)
}

// newTestServerGrantsToken is newTestServerGrants with an explicit proxy token;
// an empty token selects the token-less loopback-only mode.
func newTestServerGrantsToken(t *testing.T, p *fakeProvider, grants []policy.CapabilityGrant, token string) *Server {
	t.Helper()
	pol, err := policy.Build([]policy.RuleSpec{{
		Repositories: []string{"team/*"},
		Effect:       string(policy.EffectAllow),
		Capabilities: grants,
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
		Token:     token,
	}, registry, guard, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// branchFilter builds a repo:write grant whose branches filter carries the
// given include and exclude globs: pushable branches are policy configuration
// now, not a proxy setting.
func branchFilter(include, exclude []string) []policy.CapabilityGrant {
	return []policy.CapabilityGrant{{
		Name: policy.CapRepoWrite,
		Filter: policy.CapabilityFilter{
			Branches: policy.PathFilter{Include: include, Exclude: exclude},
		},
	}}
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

// pushBodyMulti builds a command section from fully framed command lines (the
// first must carry the capability list after a NUL) followed by the pack body.
func pushBodyMulti(lines ...string) []byte {
	return append(pkts(lines...), packBody...)
}

// pushBodyWithOptions builds a receive-pack body whose first command line
// negotiates the push-options capability and whose options section carries the
// given options (empty options send the section flush only), followed by the
// pack body.
func pushBodyWithOptions(oldSHA, newSHA, ref string, options ...string) []byte {
	body := pkts(oldSHA + " " + newSHA + " " + ref + "\x00report-status side-band-64k push-options\n")
	body = append(body, optionPkts(options...)...)
	return append(body, packBody...)
}

// pushOptionGrants is the repo:write branch filter plus the capabilities that
// gate push options, so a test grants exactly what an option needs — or withholds
// it.
func pushOptionGrants(include []string, mr ...policy.Capability) []policy.CapabilityGrant {
	grants := branchFilter(include, nil)
	for _, c := range mr {
		grants = append(grants, policy.CapabilityGrant{Name: c})
	}
	return grants
}

func TestAuthRequired(t *testing.T) {
	u := newUpstream(t, []byte(" advertisement "))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})

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

// TestNoTokenModeSkipsAuthentication pins the token-less mode: on a loopback
// listen without a proxy token, requests carrying no Authorization header are
// forwarded (still upstream-authenticated with the provider credential only),
// and a stray client credential neither authenticates nor leaks upstream.
func TestNoTokenModeSkipsAuthentication(t *testing.T) {
	ad := []byte("001e# service=git-upload-pack\n0000")
	u := newUpstream(t, ad)
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrantsToken(t, p,
		[]policy.CapabilityGrant{{Name: policy.CapRepoRead}}, "")

	target := "/git/" + providerName + "/" + repo + ".git/info/refs?service=git-upload-pack"

	rec := doRequest(t, s, http.MethodGet, target, nil, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous without token = %d, want 200, body %q", rec.Code, rec.Body)
	}
	if ch := rec.Header().Get("WWW-Authenticate"); ch != "" {
		t.Errorf("WWW-Authenticate = %q, want no challenge without a token", ch)
	}

	rec = doRequest(t, s, http.MethodGet, target, nil, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("stray credentials without token = %d, want 200", rec.Code)
	}
	got := u.last()
	if got.authorization != fakeGitAuth {
		t.Errorf("upstream Authorization = %q, want the provider credential", got.authorization)
	}
	if strings.Contains(got.authorization, proxyToken) {
		t.Error("upstream Authorization contains the proxy token")
	}
}

func TestAuthRequiredReflectsToken(t *testing.T) {
	p := newFetchProvider("http://upstream.invalid")
	if !newTestServer(t, p, []policy.Capability{policy.CapRepoRead}).AuthRequired() {
		t.Error("AuthRequired() = false with a token, want true")
	}
	s := newTestServerGrantsToken(t, p, []policy.CapabilityGrant{{Name: policy.CapRepoRead}}, "")
	if s.AuthRequired() {
		t.Error("AuthRequired() = true without a token, want false")
	}
}

func TestFetchInfoRefsForwardsWithProviderAuth(t *testing.T) {
	ad := []byte("001e# service=git-upload-pack\n0000")
	u := newUpstream(t, ad)
	u.contentType = "application/x-git-upload-pack-advertisement"
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})

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

// TestInboundCredentialHeadersStripped pins the identity separation: a client
// that attaches its own credentials (proxy Basic auth plus Private-Token,
// Cookie and Proxy-Authorization) must never have any of them reach the
// upstream. The upstream sees exclusively the provider's git credential.
func TestInboundCredentialHeadersStripped(t *testing.T) {
	ad := []byte("001e# service=git-upload-pack\n0000")
	u := newUpstream(t, ad)
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})

	req := httptest.NewRequest(http.MethodGet,
		"/git/"+providerName+"/"+repo+".git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("git", proxyToken) // becomes the Authorization header
	req.Header.Set("Private-Token", "attacker-token")
	req.Header.Set("Cookie", "session=x")
	req.Header.Set("Proxy-Authorization", "Basic zzz")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body)
	}

	got := u.last()
	// The only credential upstream is the provider's git auth, not the
	// client's Basic proxy token.
	if got.authorization != fakeGitAuth {
		t.Errorf("upstream Authorization = %q, want the provider credential %q", got.authorization, fakeGitAuth)
	}
	if strings.Contains(got.authorization, proxyToken) {
		t.Error("upstream Authorization contains the proxy token")
	}
	// Every other inbound credential header is stripped outright: an empty
	// value would still prove presence, so the key must be absent entirely.
	for _, h := range []string{"Private-Token", "Cookie", "Proxy-Authorization"} {
		if vs := got.header.Values(h); len(vs) > 0 {
			t.Errorf("upstream header %q = %v, want it stripped completely", h, vs)
		}
	}
	// No client credential leaks through any other header either.
	for name, values := range got.header {
		for _, v := range values {
			if strings.Contains(v, proxyToken) || strings.Contains(v, "attacker-token") || strings.Contains(v, "session=x") {
				t.Errorf("upstream header %q leaks a client credential: %v", name, v)
			}
		}
	}
}

func TestFetchUploadPackPostForwardsBody(t *testing.T) {
	u := newUpstream(t, []byte("pack-negotiation-response"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})

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
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})

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
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead, policy.CapRepoWrite})

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
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})

	target := "/git/" + providerName + "/" + repo + ".git/info/refs?service=git-receive-pack"
	rec := doRequest(t, s, http.MethodGet, target, nil, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("receive-pack discovery without repo:write = %d, want 403", rec.Code)
	}

	sWrite := newTestServer(t, newFetchProvider(u.serverURL),
		[]policy.Capability{policy.CapRepoWrite})
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
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

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
		name    string
		old     string
		new     string
		ref     string
		include []string
		tips    map[string]string
		mb      string
		def     string
	}{
		{
			// The branch filter allows "main" here so the hard-coded
			// default-branch rule rejects it, not the filter.
			name: "default branch", old: sha1Old, new: sha1New,
			ref: "refs/heads/main", include: []string{"**"}, def: "main",
		},
		{
			name: "branch outside policy filter", old: sha1Zero, new: sha1New,
			ref: "refs/heads/feature/x", include: []string{"ai/**"}, def: "main",
		},
		{
			name: "delete", old: sha1Old, new: sha1Zero,
			ref: "refs/heads/ai/fix", include: []string{"ai/**"}, def: "main",
		},
		{
			name: "non fast-forward", old: sha1Old, new: sha1New,
			ref: "refs/heads/ai/fix", include: []string{"ai/**"},
			tips: map[string]string{"ai/fix": sha1Old}, mb: notFastFwd, def: "main",
		},
		{
			name: "tag ref", old: sha1Zero, new: sha1New,
			ref: "refs/tags/v1", include: []string{"ai/**"}, def: "main",
		},
		{
			name: "note ref", old: sha1Zero, new: sha1New,
			ref: "refs/notes/ai", include: []string{"ai/**"}, def: "main",
		},
		{
			name: "invalid branch name", old: sha1Zero, new: sha1New,
			ref: "refs/heads/ai/bad..name", include: []string{"ai/**"}, def: "main",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, []byte("should not be called"))
			p := newFetchProvider(u.serverURL)
			p.defaultBranch = tc.def
			p.mergeBase = tc.mb
			maps.Copy(p.tips, tc.tips)
			s := newTestServerGrants(t, p, branchFilter(tc.include, nil))

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
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

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
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

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
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

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

// TestPushStaleBaseDenied pins the advertised-base check: an update whose old
// object id differs from the provider's current tip is a stale base (a
// non-fast-forward/force push in disguise) and denies with the branch-policy 403
// directly after ResolveRef — the merge base is never asked, because the base
// comparison already fails.
func TestPushStaleBaseDenied(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	// The client advertises sha1Old as its base, but the branch moved on.
	p.tips["ai/fix"] = strings.Repeat("c", 40)
	// mergeBase would claim a fast-forward; the stale base must deny before it.
	p.mergeBase = strings.Repeat("c", 40)
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Old, sha1New, "refs/heads/ai/fix"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("push from a stale base = %d, want 403, body %q", rec.Code, rec.Body)
	}
	if want := "git proxy: push rejected by branch policy\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want the branch-policy denial %q", rec.Body.String(), want)
	}
	if p.mergeBaseCalls != 0 {
		t.Errorf("merge base calls = %d, want 0: the stale base denies before the merge base", p.mergeBaseCalls)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", u.count())
	}
}

func TestPushMalformedCommandSection(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

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

// TestPushMergeRequestOptionRequiresMRWrite pins the mr:write gate: the very same
// push is forwarded with the grant and denied without it, and the denial never
// reaches the upstream.
func TestPushMergeRequestOptionRequiresMRWrite(t *testing.T) {
	u := newUpstream(t, []byte("000cunpack ok\n0009"))
	body := pushBodyWithOptions(sha1Zero, sha1New, "refs/heads/ai/fix", "merge_request.create")

	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, pushOptionGrants([]string{"ai/**"}, policy.CapMRWrite))
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("push option with mr:write = %d, want 200, body %q", rec.Code, rec.Body)
	}
	// The options are policy-checked but forwarded verbatim: the upstream must
	// still create the merge request from the original bytes.
	if !bytes.Equal(u.last().body, body) {
		t.Errorf("upstream body = %q, want the original %q", u.last().body, body)
	}

	pNo := newFetchProvider(u.serverURL)
	sNo := newTestServerGrants(t, pNo, pushOptionGrants([]string{"ai/**"}))
	rec = doRequest(t, sNo, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("push option without mr:write = %d, want 403", rec.Code)
	}
	if want := "git proxy: repository is not accessible\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want the generic capability denial %q", rec.Body.String(), want)
	}
	if u.count() != 1 {
		t.Errorf("upstream calls = %d, want 1 (only the allowed push)", u.count())
	}
	// The capability denial precedes every marker read for mr:write: the guard
	// answers from the policy, so only the two branch-authorization reads remain.
	if want := []string{"", "ai/fix"}; !slices.Equal(pNo.fileExistsRefs, want) {
		t.Errorf("marker refs = %v, want %v (denied by policy, before any mr:write marker read)",
			pNo.fileExistsRefs, want)
	}
}

// TestPushMergeRequestOptionDeniedByNoAIOverlay pins that the mr:write check of a
// push option runs through the guard with the .noai overlay: the repo:write grant
// is exempt here, so the branch authorization passes on a marked repository, but
// the non-exempt mr:write grant denies — and the answer is the one identical
// "not accessible" 403.
func TestPushMergeRequestOptionDeniedByNoAIOverlay(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	p.marker = true
	s := newTestServerGrants(t, p, []policy.CapabilityGrant{
		{Name: policy.CapRepoWrite, Filter: policy.CapabilityFilter{
			NoAIExempt: true,
			Branches:   policy.PathFilter{Include: []string{"ai/**"}},
		}},
		{Name: policy.CapMRWrite},
	})

	body := pushBodyWithOptions(sha1Zero, sha1New, "refs/heads/ai/fix", "merge_request.create")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("push option on a .noai repository = %d, want 403, body %q", rec.Code, rec.Body)
	}
	if want := "git proxy: repository is not accessible\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want the generic denial %q", rec.Body.String(), want)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", u.count())
	}
	// The exempt branch authorization read no marker; the mr:write check did, on
	// the default branch.
	if want := []string{""}; !slices.Equal(p.fileExistsRefs, want) {
		t.Errorf("marker refs = %v, want %v (the .noai overlay applies to mr:write)", p.fileExistsRefs, want)
	}
}

// TestPushAutoMergeOptionRequiresMRMerge pins the additional mr:merge gate for
// auto-merge: mr:write alone is not enough, both capabilities together allow it.
func TestPushAutoMergeOptionRequiresMRMerge(t *testing.T) {
	u := newUpstream(t, []byte("000cunpack ok\n0009"))
	for _, option := range []string{"merge_request.auto_merge", "merge_request.merge_when_pipeline_succeeds"} {
		body := pushBodyWithOptions(sha1Zero, sha1New, "refs/heads/ai/fix", "merge_request.create", option)

		writeOnly := newTestServerGrants(t, newFetchProvider(u.serverURL),
			pushOptionGrants([]string{"ai/**"}, policy.CapMRWrite))
		rec := doRequest(t, writeOnly, http.MethodPost,
			"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with mr:write only = %d, want 403", option, rec.Code)
		}

		both := newTestServerGrants(t, newFetchProvider(u.serverURL),
			pushOptionGrants([]string{"ai/**"}, policy.CapMRWrite, policy.CapMRMerge))
		rec = doRequest(t, both, http.MethodPost,
			"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with mr:write and mr:merge = %d, want 200, body %q", option, rec.Code, rec.Body)
		}
	}
	if want := 2; u.count() != want {
		t.Errorf("upstream calls = %d, want %d (only the two allowed pushes)", u.count(), want)
	}
}

// TestPushOptionsDeniedCases pins the fail-closed option vocabulary: every option
// outside the allowed merge_request namespace denies with the push-policy 403,
// before a single byte is forwarded, and the denial never names the option value.
func TestPushOptionsDeniedCases(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	tests := []struct {
		name    string
		options []string
		leaks   []string
	}{
		{
			name:    "cross-project merge request",
			options: []string{"merge_request.target_project=other/secret"},
			leaks:   []string{"other/secret", "target_project"},
		},
		{
			name:    "invalid merge_request.target",
			options: []string{"merge_request.target=bad..name"},
			leaks:   []string{"bad..name"},
		},
		{"ci option", []string{"ci.skip"}, []string{"ci"}},
		{"unknown merge_request option", []string{"merge_request.future_action=1"}, []string{"future_action"}},
		{"unknown option", []string{"deploy.production=true"}, []string{"deploy"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// mr:write and mr:merge are granted here: only the option itself may
			// deny, so a 403 proves the vocabulary bound rather than a capability.
			p := newFetchProvider(u.serverURL)
			s := newTestServerGrants(t, p,
				pushOptionGrants([]string{"ai/**"}, policy.CapMRWrite, policy.CapMRMerge))
			body := pushBodyWithOptions(sha1Zero, sha1New, "refs/heads/ai/fix", tc.options...)

			rec := doRequest(t, s, http.MethodPost,
				"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403, body %q", rec.Code, rec.Body)
			}
			if want := "git proxy: push rejected by branch policy\n"; rec.Body.String() != want {
				t.Errorf("body = %q, want the push-policy denial %q", rec.Body.String(), want)
			}
			if u.count() != 0 {
				t.Errorf("upstream calls = %d, want 0 for a denied push option", u.count())
			}
			// No option content, value or ref ever reaches the client.
			for _, leak := range append(tc.leaks, "refs/", "merge_request.create", "ai/fix") {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("denial body leaks %q: %q", leak, rec.Body.String())
				}
			}
		})
	}
}

// TestPushOptionsCrossProjectDeniedWithoutMRWrite pins that the cross-project
// escape is refused outright, not merely gated: no guard read happens at all, so
// an unprivileged client learns nothing more than the identical 403.
func TestPushOptionsCrossProjectDeniedWithoutMRWrite(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, pushOptionGrants([]string{"ai/**"}))

	body := pushBodyWithOptions(sha1Zero, sha1New, "refs/heads/ai/fix", "merge_request.target_project=other/secret")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", u.count())
	}
	// Only the repo:write branch authorization read the marker; the mr:write guard
	// was never asked, so no third marker read happened.
	if want := []string{"", "ai/fix"}; !slices.Equal(p.fileExistsRefs, want) {
		t.Errorf("marker refs = %v, want %v (the hard denial precedes the guard)", p.fileExistsRefs, want)
	}
}

// TestPushNoOptionsUnchanged pins that a push without options behaves exactly as
// before: the negotiated-but-empty section costs no capability check, so a grant
// without mr:write still forwards, byte for byte.
func TestPushNoOptionsUnchanged(t *testing.T) {
	u := newUpstream(t, []byte("000cunpack ok\n0009"))
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, pushOptionGrants([]string{"ai/**"}))

	// Negotiated capability, empty options section.
	body := pushBodyWithOptions(sha1Zero, sha1New, "refs/heads/ai/fix")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("negotiated empty options = %d, want 200, body %q", rec.Code, rec.Body)
	}
	if !bytes.Equal(u.last().body, body) {
		t.Error("upstream body differs from the original request body")
	}
	// The mr:write capability was never asked: only the branch authorization read
	// the marker (default branch and target branch).
	if want := []string{"", "ai/fix"}; !slices.Equal(p.fileExistsRefs, want) {
		t.Errorf("marker refs = %v, want %v (no capability check without options)", p.fileExistsRefs, want)
	}

	// Nothing negotiated at all: the classic body still forwards.
	pPlain := newFetchProvider(u.serverURL)
	sPlain := newTestServerGrants(t, pPlain, pushOptionGrants([]string{"ai/**"}))
	rec = doRequest(t, sPlain, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Zero, sha1New, "refs/heads/ai/fix"), "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain push = %d, want 200, body %q", rec.Code, rec.Body)
	}
}

// TestPushMalformedOptionsSection pins that a broken options section is a
// protocol error (400) and never reaches the provider, even though the
// capabilities would allow the push.
func TestPushMalformedOptionsSection(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, pushOptionGrants([]string{"ai/**"}, policy.CapMRWrite))

	// The capability is negotiated but the section has no terminating flush.
	body := append(pkts(sha1Zero+" "+sha1New+" refs/heads/ai/fix\x00push-options\n"),
		pkt("merge_request.create\n")...)
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0 for a malformed push", u.count())
	}
	if len(p.fileExistsRefs) != 0 {
		t.Errorf("marker refs = %v, want none: parsing precedes authorization", p.fileExistsRefs)
	}
}

// TestPushNoAIMarkerOnTargetBranchDenies pins the .noai target-branch check. The
// default branch is marker-free, but the branch being pushed carries the marker.
// A push is authorized per unique target branch, so the guard checks the marker
// on the default branch AND on the branch; the branch occurrence denies. This
// happens in authorization, before any provider metadata call in checkPush, so
// the upstream is never contacted and DefaultBranch/ResolveRef/MergeBase never
// run. The denial is the one indistinguishable "not accessible" 403.
func TestPushNoAIMarkerOnTargetBranchDenies(t *testing.T) {
	u := newUpstream(t, []byte("should not be called"))
	p := newFetchProvider(u.serverURL)
	// Marker only on the push target branch; the default branch stays clean.
	p.markersByRef = map[string]bool{"ai/fix": true}
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

	body := pushBody(sha1Zero, sha1New, "refs/heads/ai/fix")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("push to a .noai target branch = %d, want 403, body %q", rec.Code, rec.Body)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0 for a .noai branch push", u.count())
	}
	// The marker was checked on the default branch and on the target branch.
	if !slices.Contains(p.fileExistsRefs, "") || !slices.Contains(p.fileExistsRefs, "ai/fix") {
		t.Errorf("marker refs = %v, want the default branch and ai/fix checked", p.fileExistsRefs)
	}
	// Authorization precedes every provider metadata call of checkPush.
	if p.defaultBranchCalls != 0 || p.resolveCalls != 0 || p.mergeBaseCalls != 0 {
		t.Errorf("metadata calls = default %d resolve %d mergeBase %d, want 0/0/0 before a denied authorization",
			p.defaultBranchCalls, p.resolveCalls, p.mergeBaseCalls)
	}
	// The denial is byte-identical to the unknown-repository denial.
	recUnknown := doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/other/secret.git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Body.String() != recUnknown.Body.String() {
		t.Errorf(".noai branch denial %q differs from the unknown-repository denial %q",
			rec.Body.String(), recUnknown.Body.String())
	}
}

// TestPushNoAIExemptGrantAllowsMarkedBranch pins that a noai: allow grant skips
// the .noai marker on BOTH the default branch and the push target branch, so a
// push to a marked branch is permitted once the branch policy passes. The marker
// is present everywhere here to prove the exemption, not an absent marker, is
// what allows the push.
func TestPushNoAIExemptGrantAllowsMarkedBranch(t *testing.T) {
	u := newUpstream(t, []byte("000cunpack ok\n0009"))
	p := newFetchProvider(u.serverURL)
	p.markersByRef = map[string]bool{"": true, "ai/fix": true}
	s := newTestServerGrants(t, p, []policy.CapabilityGrant{
		{Name: policy.CapRepoWrite, Filter: policy.CapabilityFilter{
			NoAIExempt: true,
			Branches:   policy.PathFilter{Include: []string{"ai/**"}},
		}},
	})

	body := pushBody(sha1Zero, sha1New, "refs/heads/ai/fix")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("exempt push to a .noai branch = %d, want 200, body %q", rec.Code, rec.Body)
	}
	// An exempt grant skips the marker read entirely: FileExists was never asked.
	if len(p.fileExistsRefs) != 0 {
		t.Errorf("marker refs = %v, want none for an exempt grant", p.fileExistsRefs)
	}
	if !bytes.Equal(u.last().body, body) {
		t.Error("upstream body differs from the original request body")
	}
}

// TestPushDuplicateTargetBranchAuthorizedOnce pins ref deduplication: two
// commands that resolve to the same branch authorize that branch once, so the
// marker is checked on the default branch and the branch exactly one time each.
func TestPushDuplicateTargetBranchAuthorizedOnce(t *testing.T) {
	u := newUpstream(t, []byte("000cunpack ok\n0009"))
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

	// Two creates that target the same branch: one capability line with caps, one
	// plain command line, both refs/heads/ai/fix.
	body := pushBodyMulti(
		sha1Zero+" "+sha1New+" refs/heads/ai/fix\x00report-status side-band-64k\n",
		sha1Zero+" "+sha1New+" refs/heads/ai/fix\n",
	)
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body)
	}
	// Deduplicated: the default branch ("") and ai/fix are each checked once.
	if want := []string{"", "ai/fix"}; !slices.Equal(p.fileExistsRefs, want) {
		t.Errorf("marker refs = %v, want exactly %v (deduplicated)", p.fileExistsRefs, want)
	}
	if !bytes.Equal(u.last().body, body) {
		t.Error("upstream body differs from the original request body")
	}
}

// TestPushTagRefAuthorizedAtRepoLevel documents the fail-closed handling of a
// non-branch ref: with repo:write granted it is authorized at repository level
// (the default-branch marker only, because a tag maps to no branch), then
// checkPush rejects it as a non-branch ref with the branch-policy 403. The
// grant carries a branches filter here: repository-level authorization carries
// no branch context, so the filter does not apply and only marks the ref type
// for rejection. A repository the client cannot write instead yields the
// generic "not accessible" 403 before any marker read, because the capability
// layer denies first.
func TestPushTagRefAuthorizedAtRepoLevel(t *testing.T) {
	u := newUpstream(t, []byte("nope"))

	// repo:write granted: authorization passes on a clean default branch, the
	// marker is checked at repository level only (ref ""), then checkPush denies.
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))
	body := pushBody(sha1Zero, sha1New, "refs/tags/v1")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if want := []string{""}; !slices.Equal(p.fileExistsRefs, want) {
		t.Errorf("marker refs = %v, want %v (repository-level authorization for a tag)", p.fileExistsRefs, want)
	}
	if want := "git proxy: push rejected by branch policy\n"; rec.Body.String() != want {
		t.Errorf("denial body = %q, want the branch-policy message %q", rec.Body.String(), want)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", u.count())
	}

	// Without repo:write the capability layer denies before the marker read, so
	// FileExists runs zero times and the answer is the generic denial.
	pNo := newFetchProvider(u.serverURL)
	sNo := newTestServer(t, pNo, []policy.Capability{policy.CapRepoRead})
	rec = doRequest(t, sNo, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no-write status = %d, want 403", rec.Code)
	}
	if len(pNo.fileExistsRefs) != 0 {
		t.Errorf("no-write marker refs = %v, want none (denied before the marker check)", pNo.fileExistsRefs)
	}
	if want := "git proxy: repository is not accessible\n"; rec.Body.String() != want {
		t.Errorf("no-write body = %q, want the generic denial %q", rec.Body.String(), want)
	}
}

// TestPushBranchWithoutBranchesFilterDenied pins the fail-closed push rule: a
// repo:write grant without a branches filter allows no push. The denial comes
// from the policy (AuthorizeBranch → "branch filter required") before any
// provider metadata read or marker check, so the answer is the generic
// "not accessible" 403 and no ref detail leaks.
func TestPushBranchWithoutBranchesFilterDenied(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoWrite})

	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Zero, sha1New, "refs/heads/ai/fix"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("push without a branches filter = %d, want 403", rec.Code)
	}
	if want := "git proxy: repository is not accessible\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want the generic denial %q", rec.Body.String(), want)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", u.count())
	}
	// Policy denial precedes the marker check and every provider metadata read.
	if len(p.fileExistsRefs) != 0 {
		t.Errorf("marker refs = %v, want none (denied before the marker check)", p.fileExistsRefs)
	}
	if p.defaultBranchCalls != 0 || p.resolveCalls != 0 || p.mergeBaseCalls != 0 {
		t.Errorf("metadata calls = default %d resolve %d mergeBase %d, want 0/0/0",
			p.defaultBranchCalls, p.resolveCalls, p.mergeBaseCalls)
	}
}

// TestPushBranchExcludeAndNonMatchDenied pins the branch filter in the proxy:
// branches.exclude wins over branches.include, and a branch outside the include
// list denies alike. Both denials answer the generic policy 403; a branch the
// same filter allows is still forwarded.
func TestPushBranchExcludeAndNonMatchDenied(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	tests := []struct {
		name    string
		include []string
		exclude []string
		ref     string
	}{
		{"exclude wins over include", []string{"**"}, []string{"ai/wip/**"}, "refs/heads/ai/wip/tmp"},
		{"include non-match", []string{"ai/**"}, nil, "refs/heads/bot/x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newFetchProvider(u.serverURL)
			s := newTestServerGrants(t, p, branchFilter(tc.include, tc.exclude))
			rec := doRequest(t, s, http.MethodPost,
				"/git/"+providerName+"/"+repo+".git/git-receive-pack",
				pushBody(sha1Zero, sha1New, tc.ref), "git", proxyToken)
			if rec.Code != http.StatusForbidden {
				t.Errorf("push %s = %d, want 403", tc.ref, rec.Code)
			}
			if want := "git proxy: repository is not accessible\n"; rec.Body.String() != want {
				t.Errorf("body = %q, want the generic denial %q", rec.Body.String(), want)
			}
		})
	}

	// A branch that passes the exclude filter (not excluded) is forwarded.
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, branchFilter([]string{"**"}, []string{"ai/wip/**"}))
	body := pushBody(sha1Zero, sha1New, "refs/heads/ai/fix")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed branch through exclude filter = %d, want 200, body %q", rec.Code, rec.Body)
	}
}

// TestPushMalformedBranchNotAuthorizedViaGuard pins the validation order of
// authorizePushRefs: a syntactically invalid branch name is never handed to
// Guard.AuthorizeBranch, so the guard performs no provider marker read at an
// unvalidated ref — even when the name would match the branches filter. The
// denial comes from checkPush instead, and the client-visible answer stays a
// 403 (the branch-policy one, as before this reordering).
func TestPushMalformedBranchNotAuthorizedViaGuard(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

	// "ai/bad..name" matches ai/** but fails check-ref-format (contains "..").
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Zero, sha1New, "refs/heads/ai/bad..name"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body %q", rec.Code, rec.Body)
	}
	if want := "git proxy: push rejected by branch policy\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want the branch-policy denial %q", rec.Body.String(), want)
	}
	// Skipping the guard means no marker read at the unvalidated ref.
	if len(p.fileExistsRefs) != 0 {
		t.Errorf("marker refs = %v, want none: AuthorizeBranch must not see an invalid ref", p.fileExistsRefs)
	}
	// checkPush rejects the name before any provider metadata call.
	if p.defaultBranchCalls != 0 || p.resolveCalls != 0 || p.mergeBaseCalls != 0 {
		t.Errorf("metadata calls = default %d resolve %d mergeBase %d, want 0/0/0",
			p.defaultBranchCalls, p.resolveCalls, p.mergeBaseCalls)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", u.count())
	}
}

// TestPushExcludeOnlyBranchFilter pins a branches filter without include at the
// proxy level: everything the exclude list does not name is pushable, and a
// denied branch gets the generic policy 403 (the guard denies before any marker
// read or metadata call, and nothing is forwarded).
func TestPushExcludeOnlyBranchFilter(t *testing.T) {
	u := newUpstream(t, []byte("000cunpack ok\n0009"))
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, branchFilter(nil, []string{"ai/blocked"}))

	body := pushBody(sha1Zero, sha1New, "refs/heads/ai/ok")
	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack", body, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("push outside the exclude list = %d, want 200, body %q", rec.Code, rec.Body)
	}
	if !bytes.Equal(u.last().body, body) {
		t.Error("upstream body differs from the original request body")
	}

	// A fresh provider records nothing from the allowed push above, so the
	// marker-call assertion below speaks only about the denied one.
	pDenied := newFetchProvider(u.serverURL)
	sDenied := newTestServerGrants(t, pDenied, branchFilter(nil, []string{"ai/blocked"}))
	rec = doRequest(t, sDenied, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Zero, sha1New, "refs/heads/ai/blocked"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("push of an excluded branch = %d, want 403", rec.Code)
	}
	if want := "git proxy: repository is not accessible\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want the generic denial %q", rec.Body.String(), want)
	}
	if len(pDenied.fileExistsRefs) != 0 {
		t.Errorf("marker refs = %v, want none (branch-filter denial precedes the marker check)",
			pDenied.fileExistsRefs)
	}
	if u.count() != 1 {
		t.Errorf("upstream calls = %d, want 1 (only the allowed push)", u.count())
	}
}

// TestPushTagConstrainedGrantFailsClosed pins the documented proxy bound on the
// push path: git traffic carries no tags, so a repo:write grant with a tag
// requirement (alongside a matching branches filter) sees unknown tags and
// denies fail-closed. The tag check runs inside the policy evaluation, before
// the marker check and before any provider metadata read, and the answer is the
// generic "not accessible" 403.
func TestPushTagConstrainedGrantFailsClosed(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, []policy.CapabilityGrant{{
		Name: policy.CapRepoWrite,
		Filter: policy.CapabilityFilter{
			Require:  []string{"ai-ok"},
			Branches: policy.PathFilter{Include: []string{"ai/**"}},
		},
	}})

	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Zero, sha1New, "refs/heads/ai/fix"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("push with unknown tags = %d, want 403, body %q", rec.Code, rec.Body)
	}
	if want := "git proxy: repository is not accessible\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want the generic denial %q", rec.Body.String(), want)
	}
	if len(p.fileExistsRefs) != 0 {
		t.Errorf("marker refs = %v, want none (tag denial precedes the marker check)", p.fileExistsRefs)
	}
	if p.defaultBranchCalls != 0 || p.resolveCalls != 0 || p.mergeBaseCalls != 0 {
		t.Errorf("metadata calls = default %d resolve %d mergeBase %d, want 0/0/0",
			p.defaultBranchCalls, p.resolveCalls, p.mergeBaseCalls)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", u.count())
	}
}

// TestPushBranchFilterIsCaseSensitive pins exact, case-sensitive doublestar
// matching for the branches filter at the proxy level: the syntactically valid
// branch "AI/x" does not match the include "ai/**", so the push denies
// fail-closed with the generic policy 403 before any marker read.
func TestPushBranchFilterIsCaseSensitive(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

	rec := doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Zero, sha1New, "refs/heads/AI/x"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("branch differing only in case = %d, want 403, body %q", rec.Code, rec.Body)
	}
	if want := "git proxy: repository is not accessible\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want the generic denial %q", rec.Body.String(), want)
	}
	if len(p.fileExistsRefs) != 0 {
		t.Errorf("marker refs = %v, want none (branch-filter denial precedes the marker check)", p.fileExistsRefs)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0", u.count())
	}
}

// TestReceivePackDiscoveryForBranchFilteredGrantPasses pins the single policy
// core: push discovery carries no branch context, so the branches filter does
// not apply there and the advertisement is served; the per-branch authorization
// of the actual push still denies non-matching branches.
func TestReceivePackDiscoveryForBranchFilteredGrantPasses(t *testing.T) {
	ad := []byte("001e# service=git-receive-pack\n0000")
	u := newUpstream(t, ad)
	p := newFetchProvider(u.serverURL)
	s := newTestServerGrants(t, p, branchFilter([]string{"ai/**"}, nil))

	target := "/git/" + providerName + "/" + repo + ".git/info/refs?service=git-receive-pack"
	rec := doRequest(t, s, http.MethodGet, target, nil, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery with a branch-filtered grant = %d, want 200, body %q", rec.Code, rec.Body)
	}

	rec = doRequest(t, s, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Zero, sha1New, "refs/heads/feature/x"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("push outside the filter = %d, want 403", rec.Code)
	}
}

// TestPathFilteredGrantDeniesGitTraffic pins "paths ⇒ no git": an active paths
// filter cannot be enforced over git traffic, so fetch discovery and push deny
// fail-closed ("path required") even when the branch filter would match.
func TestPathFilteredGrantDeniesGitTraffic(t *testing.T) {
	u := newUpstream(t, []byte("nope"))

	read := newTestServerGrants(t, newFetchProvider(u.serverURL), []policy.CapabilityGrant{{
		Name: policy.CapRepoRead,
		Filter: policy.CapabilityFilter{
			Paths: policy.PathFilter{Include: []string{"docs/**"}},
		},
	}})
	rec := doRequest(t, read, http.MethodGet,
		"/git/"+providerName+"/"+repo+".git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fetch with a path-filtered repo:read = %d, want 403", rec.Code)
	}

	write := newTestServerGrants(t, newFetchProvider(u.serverURL), []policy.CapabilityGrant{{
		Name: policy.CapRepoWrite,
		Filter: policy.CapabilityFilter{
			Paths:    policy.PathFilter{Include: []string{"src/**"}},
			Branches: policy.PathFilter{Include: []string{"ai/**"}},
		},
	}})
	rec = doRequest(t, write, http.MethodPost,
		"/git/"+providerName+"/"+repo+".git/git-receive-pack",
		pushBody(sha1Zero, sha1New, "refs/heads/ai/fix"), "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("push with a path-filtered repo:write = %d, want 403", rec.Code)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0 for path-filtered denials", u.count())
	}
}

func TestRoutingUnsupportedEndpoints(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p,
		[]policy.Capability{policy.CapRepoRead, policy.CapRepoWrite})

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
		[]policy.Capability{policy.CapRepoRead, policy.CapRepoWrite})

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
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})

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
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})

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

	// Canonicalization applies here too: a mixed-case path is lowercased.
	got, err = s.RemoteURL(providerName, "Team/APP")
	if err != nil {
		t.Fatalf("RemoteURL mixed case: %v", err)
	}
	if want := "https://gitproxy.example.com/git/gl/team/app.git"; got != want {
		t.Errorf("RemoteURL mixed case = %q, want %q", got, want)
	}

	if _, err := s.RemoteURL("nope", repo); err == nil {
		t.Error("RemoteURL accepted an unknown provider")
	}
	bad := []string{
		"", "app", "42", "..", "team/../x", "team//x", "team/app.git", "team/ap p",
		"team/a?b", "team/a#b", "team/a@b", "team/a:b", "team/a%40b",
	}
	for _, badRepo := range bad {
		if _, err := s.RemoteURL(providerName, badRepo); err == nil {
			t.Errorf("RemoteURL accepted invalid repo %q", badRepo)
		}
	}
}

func TestCanonicalRepoPath(t *testing.T) {
	valid := map[string]string{
		"team/app":     "team/app",
		"Team/App":     "team/app",
		"team/sub/APP": "team/sub/app",
	}
	for in, want := range valid {
		got, err := canonicalRepoPath(in)
		if err != nil || got != want {
			t.Errorf("canonicalRepoPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	invalid := []string{"", "app", "42", "team/../x", "team//x", "team/app.git", "team/ap p"}
	for _, in := range invalid {
		if _, err := canonicalRepoPath(in); err == nil {
			t.Errorf("canonicalRepoPath(%q) accepted an invalid path", in)
		}
	}
}

// TestRouteCanonicalizesRepoCase pins that the repository is lowercased before
// it reaches the policy: a case-variant of a denied repository must resolve to
// the denied canonical path (GitLab is case-insensitive), and a case-variant of
// an allowed repository must still work.
func TestRouteCanonicalizesRepoCase(t *testing.T) {
	u := newUpstream(t, []byte(" advertisement "))
	p := newFetchProvider(u.serverURL)
	pol, err := policy.Build([]policy.RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: string(policy.EffectDeny)},
		{Repositories: []string{"team/**"}, Effect: string(policy.EffectAllow),
			Capabilities: []policy.CapabilityGrant{{Name: policy.CapRepoRead}}},
	})
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
	s, err := New(Config{Listen: "127.0.0.1:0", PublicURL: "https://gitproxy.example.com/", Token: proxyToken}, registry, guard, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/team/SECRET.git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("case-variant denied repo = %d, want 403, body %q", rec.Code, rec.Body)
	}
	if u.count() != 0 {
		t.Errorf("upstream calls = %d, want 0 for the denied repository", u.count())
	}

	rec = doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/TEAM/APP.git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Code != http.StatusOK {
		t.Errorf("case-variant allowed repo = %d, want 200, body %q", rec.Code, rec.Body)
	}
}

// TestRouteRejectsBareNumericRepo pins that a bare numeric repository route
// (GitLab would read it as a project id) is rejected before the policy runs.
func TestRouteRejectsBareNumericRepo(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})

	rec := doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/42.git/info/refs?service=git-upload-pack", nil, "git", proxyToken)
	if rec.Code != http.StatusNotFound {
		t.Errorf("bare numeric repo = %d, want 404", rec.Code)
	}
	if u.count() != 0 || p.remoteURLCalls != 0 || p.authCalls != 0 {
		t.Errorf("upstream/provider calls = %d/%d/%d, want 0/0/0", u.count(), p.remoteURLCalls, p.authCalls)
	}
}

// TestRouteRejectsDuplicateService pins that a duplicated service query parameter
// is rejected: the policy authorizes the first value while the upstream parses
// the last, so accepting both could authorize one service and forward another.
func TestRouteRejectsDuplicateService(t *testing.T) {
	u := newUpstream(t, []byte("nope"))
	p := newFetchProvider(u.serverURL)
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead, policy.CapRepoWrite})

	rec := doRequest(t, s, http.MethodGet,
		"/git/"+providerName+"/"+repo+".git/info/refs?service=git-upload-pack&service=git-receive-pack", nil, "git", proxyToken)
	if rec.Code != http.StatusNotFound {
		t.Errorf("duplicate service = %d, want 404, body %q", rec.Code, rec.Body)
	}
	if u.count() != 0 || p.remoteURLCalls != 0 {
		t.Errorf("upstream/provider calls = %d/%d, want 0/0", u.count(), p.remoteURLCalls)
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
	// An empty token is the token-less loopback-only mode: valid on loopback.
	if _, err := New(Config{Listen: base.Listen, PublicURL: base.PublicURL}, registry, guard, nil); err != nil {
		t.Errorf("New rejected the token-less loopback mode: %v", err)
	}
	// Without a token a non-loopback listen fails even with allow_insecure, so
	// the rejection is the token rule, not the plain-HTTP rule.
	if _, err := New(Config{Listen: "0.0.0.0:8417", PublicURL: base.PublicURL,
		AllowInsecure: true}, registry, guard, nil); err == nil {
		t.Error("New accepted a token-less proxy on a non-loopback listen address")
	}
	if _, err := New(Config{Listen: base.Listen, PublicURL: "git.example.com", Token: base.Token}, registry, guard, nil); err == nil {
		t.Error("New accepted a relative public URL")
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
	// checkRefUpdate is exercised directly: branch filtering is policy work
	// (Guard.AuthorizeBranch), so only the name validation can reject here.
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoWrite})

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
	s := newTestServer(t, p, []policy.Capability{policy.CapRepoRead})
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
