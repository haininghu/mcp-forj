package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hvo/mcp-forj/internal/policy"
	"github.com/hvo/mcp-forj/internal/provider"
)

type fakeProvider struct {
	name            string
	marker          bool
	markerErr       error
	markerByRepo    map[string]bool
	markerErrByRepo map[string]error
	providerErr     error
	files           map[string][]byte
	repos           []provider.Repository
	mrs             []provider.MergeRequest
	notes           []provider.Note
	added           []string
	rebaseCalls     int
	rebaseErr       error
	listReposCalls  int
	topics          map[string][]string
	topicsErr       map[string]error
	topicsCalls     int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Type() string { return "fake" }

func (f *fakeProvider) ListRepositories(_ context.Context, opts provider.RepoListOptions) ([]provider.Repository, error) {
	f.listReposCalls++
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	return applyLimit(f.repos, opts.Limit), nil
}

func (f *fakeProvider) GetRepositoryTopics(_ context.Context, repo string) ([]string, error) {
	f.topicsCalls++
	if err, ok := f.topicsErr[repo]; ok {
		return nil, err
	}
	if topics, ok := f.topics[repo]; ok {
		return topics, nil
	}
	return nil, nil
}

func (f *fakeProvider) ListMergeRequests(_ context.Context, _ string, opts provider.ListOptions) ([]provider.MergeRequest, error) {
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	return applyLimit(f.mrs, opts.Limit), nil
}

func (f *fakeProvider) GetMergeRequest(_ context.Context, _ string, number int64) (*provider.MergeRequest, error) {
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	for i := range f.mrs {
		if f.mrs[i].Number == number {
			return &f.mrs[i], nil
		}
	}
	return nil, provider.ErrNotFound
}

func (f *fakeProvider) ListMergeRequestNotes(_ context.Context, _ string, _ int64, opts provider.ListOptions) ([]provider.Note, error) {
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	return applyLimit(f.notes, opts.Limit), nil
}

// applyLimit mimics a provider honoring a page-size limit.
func applyLimit[T any](items []T, limit int) []T {
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

func (f *fakeProvider) AddMergeRequestNote(_ context.Context, _ string, _ int64, body string) (*provider.Note, error) {
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	f.added = append(f.added, body)
	return &provider.Note{ID: 1, Body: body, Author: "me", CreatedAt: time.Now()}, nil
}

func (f *fakeProvider) RebaseMergeRequest(_ context.Context, _ string, _ int64) error {
	f.rebaseCalls++
	if f.rebaseErr != nil {
		return f.rebaseErr
	}
	if f.providerErr != nil {
		return f.providerErr
	}
	return nil
}

func (f *fakeProvider) ReadFile(_ context.Context, _, path, _ string) ([]byte, error) {
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	data, ok := f.files[path]
	if !ok {
		return nil, provider.ErrNotFound
	}
	return data, nil
}

func (f *fakeProvider) FileExists(_ context.Context, repo, _, _ string) (bool, error) {
	if err, ok := f.markerErrByRepo[repo]; ok {
		return false, err
	}
	if marked, ok := f.markerByRepo[repo]; ok {
		return marked, nil
	}
	if f.markerErr != nil {
		return false, f.markerErr
	}
	return f.marker, nil
}

func newFake() *fakeProvider {
	return &fakeProvider{
		name: "fake",
		files: map[string][]byte{
			"README.md": []byte("hello world"),
		},
		mrs: []provider.MergeRequest{{
			Number:       1,
			Title:        "First",
			Description:  "description",
			State:        "opened",
			Author:       "alice",
			SourceBranch: "feat",
			TargetBranch: "main",
			WebURL:       "https://example.com/mr/1",
		}},
		notes: []provider.Note{{ID: 10, Body: "a note", Author: "bob", CreatedAt: time.Now()}},
	}
}

type testEnv struct {
	session *mcp.ClientSession
	logs    *bytes.Buffer
	fake    *fakeProvider
}

func newTestEnv(t *testing.T, rules []policy.RuleSpec, fake *fakeProvider) *testEnv {
	t.Helper()
	pol, err := policy.Build(rules)
	if err != nil {
		t.Fatalf("policy.Build: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	guard := policy.NewGuard(
		map[string]*policy.Policy{"fake": pol},
		map[string]policy.FileChecker{"fake": fake},
		".noai",
		logger,
	)
	registry := provider.NewRegistry()
	registry.Register(fake)
	srv := New(guard, registry, logger)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.MCPServer("test").Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return &testEnv{session: session, logs: &logs, fake: fake}
}

func newMultiEnv(t *testing.T, fakes []*fakeProvider, rules map[string][]policy.RuleSpec) *testEnv {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	policies := make(map[string]*policy.Policy, len(fakes))
	checkers := make(map[string]policy.FileChecker, len(fakes))
	registry := provider.NewRegistry()
	for _, fake := range fakes {
		pol, err := policy.Build(rules[fake.name])
		if err != nil {
			t.Fatalf("policy.Build(%s): %v", fake.name, err)
		}
		policies[fake.name] = pol
		checkers[fake.name] = fake
		registry.Register(fake)
	}
	guard := policy.NewGuard(policies, checkers, ".noai", logger)
	srv := New(guard, registry, logger)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.MCPServer("test").Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return &testEnv{session: session, logs: &logs}
}

func (e *testEnv) call(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := e.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		return ""
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", res.Content[0])
	}
	return text.Text
}

// grants builds unfiltered capability grants from names.
func grants(names ...policy.Capability) []policy.CapabilityGrant {
	out := make([]policy.CapabilityGrant, len(names))
	for i, name := range names {
		out[i] = policy.CapabilityGrant{Name: name}
	}
	return out
}

func allowRules(caps ...policy.Capability) []policy.RuleSpec {
	return []policy.RuleSpec{{
		Repositories: []string{"team/app"},
		Effect:       "allow",
		Capabilities: grants(caps...),
	}}
}

func mrArgs() map[string]any {
	return map[string]any{"provider": "fake", "repo": "team/app"}
}

func TestNoAIProtectsOnlyReadFile(t *testing.T) {
	fake := newFake()
	fake.marker = true
	env := newTestEnv(t, allowRules("mr:read", "mr:comment", "repo:read"), fake)

	res := env.call(t, "read_file", readFileArgs())
	if !res.IsError || !strings.Contains(resultText(t, res), ".noai") {
		t.Fatalf("read_file result = %q (isError=%v), want .noai denial", resultText(t, res), res.IsError)
	}

	allowed := []struct {
		name string
		args map[string]any
	}{
		{"list_merge_requests", mrArgs()},
		{"get_merge_request", map[string]any{"provider": "fake", "repo": "team/app", "number": 1}},
		{"list_merge_request_notes", map[string]any{"provider": "fake", "repo": "team/app", "number": 1}},
		{"add_merge_request_note", map[string]any{"provider": "fake", "repo": "team/app", "number": 1, "body": "hi"}},
	}
	for _, c := range allowed {
		t.Run(c.name, func(t *testing.T) {
			res := env.call(t, c.name, c.args)
			if res.IsError {
				t.Fatalf("%s denied on a .noai repo: %s", c.name, resultText(t, res))
			}
		})
	}
}

func TestListMergeRequestsWorksOnNoAIRepo(t *testing.T) {
	fake := newFake()
	fake.marker = true
	env := newTestEnv(t, allowRules("mr:read"), fake)

	res := env.call(t, "list_merge_requests", mrArgs())
	if res.IsError {
		t.Fatalf("list_merge_requests denied on a .noai repo: %s", resultText(t, res))
	}
}

func TestMarkerCheckErrorDeniesReadFile(t *testing.T) {
	fake := newFake()
	fake.markerErr = errors.New("network down")
	env := newTestEnv(t, allowRules("repo:read"), fake)

	res := env.call(t, "read_file", readFileArgs())
	if !res.IsError {
		t.Fatal("read_file succeeded despite marker check error")
	}
	if !strings.Contains(resultText(t, res), "marker") {
		t.Errorf("error = %q, want mention of the marker", resultText(t, res))
	}
}

func TestAddNoteRequiresCommentCapability(t *testing.T) {
	env := newTestEnv(t, allowRules("mr:read"), newFake())

	res := env.call(t, "add_merge_request_note", map[string]any{
		"provider": "fake", "repo": "team/app", "number": 1, "body": "hi",
	})
	if !res.IsError {
		t.Fatal("add_merge_request_note succeeded without mr:comment")
	}
	if !strings.Contains(resultText(t, res), "denied") {
		t.Errorf("error = %q, want access denied", resultText(t, res))
	}

	res = env.call(t, "list_merge_requests", mrArgs())
	if res.IsError {
		t.Fatalf("list_merge_requests denied: %s", resultText(t, res))
	}
}

func TestMRReadWithoutRepoRead(t *testing.T) {
	env := newTestEnv(t, allowRules("mr:read"), newFake())

	res := env.call(t, "read_file", map[string]any{
		"provider": "fake", "repo": "team/app", "path": "README.md",
	})
	if !res.IsError {
		t.Fatal("read_file succeeded without repo:read")
	}

	res = env.call(t, "list_merge_requests", mrArgs())
	if res.IsError {
		t.Errorf("list_merge_requests denied: %s", resultText(t, res))
	}
	res = env.call(t, "list_merge_request_notes", map[string]any{"provider": "fake", "repo": "team/app", "number": 1})
	if res.IsError {
		t.Errorf("list_merge_request_notes denied: %s", resultText(t, res))
	}
	res = env.call(t, "get_merge_request", map[string]any{"provider": "fake", "repo": "team/app", "number": 1})
	if res.IsError {
		t.Errorf("get_merge_request denied: %s", resultText(t, res))
	}
}

func TestReadFileAllowed(t *testing.T) {
	env := newTestEnv(t, allowRules("repo:read"), newFake())
	res := env.call(t, "read_file", map[string]any{
		"provider": "fake", "repo": "team/app", "path": "README.md",
	})
	if res.IsError {
		t.Fatalf("read_file denied: %s", resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "hello world") {
		t.Errorf("read_file output = %q", resultText(t, res))
	}
}

func TestAddNoteSucceedsWithCapability(t *testing.T) {
	env := newTestEnv(t, allowRules("mr:comment"), newFake())
	res := env.call(t, "add_merge_request_note", map[string]any{
		"provider": "fake", "repo": "team/app", "number": 1, "body": "hello",
	})
	if res.IsError {
		t.Fatalf("add_merge_request_note denied: %s", resultText(t, res))
	}
	if len(env.fake.added) != 1 || env.fake.added[0] != "hello" {
		t.Errorf("added notes = %v", env.fake.added)
	}
}

func TestNoDiffContentReturned(t *testing.T) {
	env := newTestEnv(t, allowRules("mr:read"), newFake())

	res := env.call(t, "get_merge_request", map[string]any{"provider": "fake", "repo": "team/app", "number": 1})
	text := resultText(t, res)
	for _, forbidden := range []string{"\"diff\"", "diff_refs", "changes"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("merge request output contains %q: %s", forbidden, text)
		}
	}

	tools, err := env.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, "diff") {
			t.Errorf("unexpected diff tool registered: %s", tool.Name)
		}
	}
}

func TestPathTraversalRejected(t *testing.T) {
	env := newTestEnv(t, allowRules("repo:read"), newFake())
	paths := []string{
		"../secret",
		"/etc/passwd",
		"a/../secret",
		"%2e%2e/secret",
		"..\\secret",
		"a\x00b",
		".",
		"a/./b",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			res := env.call(t, "read_file", map[string]any{
				"provider": "fake", "repo": "team/app", "path": p,
			})
			if !res.IsError {
				t.Fatalf("read_file accepted traversal path %q", p)
			}
		})
	}
}

func TestListConfiguredRulesReportsConfiguredCapabilities(t *testing.T) {
	fake := newFake()
	fake.marker = true // must not affect list_configured_rules
	env := newTestEnv(t, allowRules("mr:read", "mr:comment"), fake)

	res := env.call(t, "list_configured_rules", map[string]any{})
	if res.IsError {
		t.Fatalf("list_configured_rules denied: %s", resultText(t, res))
	}
	text := resultText(t, res)
	if !strings.Contains(text, "configured_capabilities") {
		t.Errorf("output missing configured_capabilities: %s", text)
	}
	if !strings.Contains(text, "mr:read") || !strings.Contains(text, "mr:comment") {
		t.Errorf("output missing configured capabilities: %s", text)
	}
	if strings.Contains(text, ".noai") || strings.Contains(text, "marker") {
		t.Errorf("output leaked .noai state: %s", text)
	}
	if strings.Contains(text, "token") || strings.Contains(text, "secret") {
		t.Errorf("output leaked secret material: %s", text)
	}
}

func listReposRules(patterns ...string) []policy.RuleSpec {
	return []policy.RuleSpec{{
		Repositories: patterns,
		Effect:       "allow",
		Capabilities: grants("repo:list"),
	}}
}

func repoJSON(t *testing.T, res *mcp.CallToolResult) listRepositoriesOutput {
	t.Helper()
	var out listRepositoriesOutput
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestListRepositoriesStaticWithoutCapability(t *testing.T) {
	fake := newFake()
	env := newTestEnv(t, allowRules("mr:read"), fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories errored without repo:list: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 1 || out.Repositories[0].Path != "team/app" {
		t.Fatalf("repositories = %v, want [team/app]", out.Repositories)
	}
	if out.Repositories[0].WebURL != "" {
		t.Errorf("static repository web_url = %q, want empty", out.Repositories[0].WebURL)
	}
	if fake.listReposCalls != 0 {
		t.Errorf("provider ListRepositories called %d times, want 0 for static-only listing", fake.listReposCalls)
	}

	// provider is required: omitting it must fail rather than list anything.
	res = env.call(t, "list_repositories", map[string]any{})
	if !res.IsError {
		t.Fatalf("list_repositories with omitted provider succeeded: %s", resultText(t, res))
	}
}

func TestListRepositoriesStaticDeniedExcluded(t *testing.T) {
	fake := newFake()
	rules := []policy.RuleSpec{
		{Repositories: []string{"team/secret"}, Effect: "deny"},
		{Repositories: []string{"team/app"}, Effect: "allow", Capabilities: grants("mr:read")},
	}
	env := newTestEnv(t, rules, fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 1 || out.Repositories[0].Path != "team/app" {
		t.Fatalf("repositories = %v, want [team/app] (team/secret denied)", out.Repositories)
	}
}

func TestListRepositoriesStaticNoAIIncluded(t *testing.T) {
	fake := newFake()
	fake.markerByRepo = map[string]bool{"team/noai": true}
	fake.markerErr = errors.New("marker check must not run during listing")
	rules := []policy.RuleSpec{{
		Repositories: []string{"team/noai"},
		Effect:       "allow",
		Capabilities: grants("mr:read"),
	}}
	env := newTestEnv(t, rules, fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 1 || out.Repositories[0].Path != "team/noai" {
		t.Fatalf("repositories = %v, want [team/noai] (marker must not affect listing)", out.Repositories)
	}
}

func TestListRepositoriesDynamicRequiresRepoList(t *testing.T) {
	fake := newFake()
	fake.repos = []provider.Repository{{Provider: "fake", Path: "archive/x", WebURL: "https://x/archive/x"}}
	rules := []policy.RuleSpec{{
		Repositories: []string{"team/app", "archive/**"},
		Effect:       "allow",
		Capabilities: grants("mr:read"),
	}}
	env := newTestEnv(t, rules, fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 1 || out.Repositories[0].Path != "team/app" {
		t.Fatalf("repositories = %v, want only static team/app", out.Repositories)
	}
	if fake.listReposCalls != 0 {
		t.Errorf("provider ListRepositories called %d times, want 0 without repo:list", fake.listReposCalls)
	}
}

func TestListRepositoriesFiltersByPattern(t *testing.T) {
	fake := newFake()
	fake.repos = []provider.Repository{
		{Provider: "fake", Path: "archive/a", WebURL: "https://x/archive/a"},
		{Provider: "fake", Path: "team/b", WebURL: "https://x/team/b"},
		{Provider: "fake", Path: "archive/c", WebURL: "https://x/archive/c"},
	}
	env := newTestEnv(t, listReposRules("archive/**"), fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 2 {
		t.Fatalf("repositories = %v, want 2", out.Repositories)
	}
	for _, r := range out.Repositories {
		if !strings.HasPrefix(r.Path, "archive/") {
			t.Errorf("unexpected repository %q", r.Path)
		}
	}
	if out.Omitted != 0 {
		t.Errorf("omitted = %d, want 0", out.Omitted)
	}
}

func TestListRepositoriesDenyBeforeAllow(t *testing.T) {
	fake := newFake()
	fake.repos = []provider.Repository{
		{Provider: "fake", Path: "archive/secret"},
		{Provider: "fake", Path: "archive/ok"},
	}
	rules := []policy.RuleSpec{
		{Repositories: []string{"archive/secret"}, Effect: "deny"},
		{Repositories: []string{"archive/**"}, Effect: "allow", Capabilities: grants("repo:list")},
	}
	env := newTestEnv(t, rules, fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 1 || out.Repositories[0].Path != "archive/ok" {
		t.Fatalf("repositories = %v, want [archive/ok]", out.Repositories)
	}
	if out.Omitted != 1 {
		t.Errorf("omitted = %d, want 1", out.Omitted)
	}
}

func TestListRepositoriesDiscoveredNoAIIncluded(t *testing.T) {
	fake := newFake()
	fake.repos = []provider.Repository{
		{Provider: "fake", Path: "archive/noai"},
		{Provider: "fake", Path: "archive/ok"},
	}
	fake.markerByRepo = map[string]bool{"archive/noai": true}
	env := newTestEnv(t, listReposRules("archive/**"), fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 2 {
		t.Fatalf("repositories = %v, want both (marker must not affect listing)", out.Repositories)
	}
	if out.Omitted != 0 {
		t.Errorf("omitted = %d, want 0", out.Omitted)
	}
}

func TestListRepositoriesIgnoresMarkerErrors(t *testing.T) {
	fake := newFake()
	fake.repos = []provider.Repository{
		{Provider: "fake", Path: "archive/bad"},
		{Provider: "fake", Path: "archive/ok"},
	}
	fake.markerErrByRepo = map[string]error{"archive/bad": errors.New("network down")}
	env := newTestEnv(t, listReposRules("archive/**"), fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 2 {
		t.Fatalf("repositories = %v, want both (marker errors ignored during listing)", out.Repositories)
	}
	if out.Omitted != 0 {
		t.Errorf("omitted = %d, want 0", out.Omitted)
	}
}

func TestListRepositoriesDedupesStaticAndDynamic(t *testing.T) {
	fake := newFake()
	fake.repos = []provider.Repository{
		{Provider: "fake", Path: "archive/a", WebURL: "https://x/archive/a"},
		{Provider: "fake", Path: "archive/b", WebURL: "https://x/archive/b"},
	}
	rules := []policy.RuleSpec{{
		Repositories: []string{"archive/a", "archive/**"},
		Effect:       "allow",
		Capabilities: grants("repo:list"),
	}}
	env := newTestEnv(t, rules, fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 2 {
		t.Fatalf("repositories = %v, want static archive/a + dynamic archive/b with no duplicate", out.Repositories)
	}
	paths := []string{out.Repositories[0].Path, out.Repositories[1].Path}
	if paths[0] != "archive/a" || paths[1] != "archive/b" {
		t.Fatalf("repositories = %v, want [archive/a archive/b]", paths)
	}
	if out.Repositories[0].WebURL != "" {
		t.Errorf("static archive/a web_url = %q, want empty", out.Repositories[0].WebURL)
	}
	if out.Repositories[1].WebURL != "https://x/archive/b" {
		t.Errorf("dynamic archive/b web_url = %q, want https://x/archive/b", out.Repositories[1].WebURL)
	}
}

func TestListRepositoriesTruncation(t *testing.T) {
	fake := newFake()
	fake.repos = nil
	for i := 0; i < maxListResults+5; i++ {
		fake.repos = append(fake.repos, provider.Repository{Provider: "fake", Path: "archive/r" + string(rune('a'+i%26)) + string(rune('0'+i/26))})
	}
	env := newTestEnv(t, listReposRules("archive/**"), fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if !out.Truncated {
		t.Error("truncated = false, want true")
	}
	if len(out.Repositories) != maxListResults {
		t.Errorf("len = %d, want %d", len(out.Repositories), maxListResults)
	}
}

func TestListRepositoriesTruncatedWhenFetchWindowExhausted(t *testing.T) {
	fake := newFake()
	// The provider returns limit+1 candidates, but most match no rule. Even
	// though filtering leaves only one repository (under the cap), the fetch
	// window was exhausted so the listing may be incomplete.
	fake.repos = []provider.Repository{
		{Provider: "fake", Path: "other/a"},
		{Provider: "fake", Path: "other/b"},
		{Provider: "fake", Path: "other/c"},
		{Provider: "fake", Path: "archive/ok"},
	}
	env := newTestEnv(t, listReposRules("archive/**"), fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake", "limit": 3})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if !out.Truncated {
		t.Error("truncated = false, want true when the provider fetch window was exhausted")
	}
	if len(out.Repositories) != 1 || out.Repositories[0].Path != "archive/ok" {
		t.Fatalf("repositories = %v, want [archive/ok]", out.Repositories)
	}
	if out.Omitted != 0 {
		t.Errorf("omitted = %d, want 0 (no-rule candidates are not counted)", out.Omitted)
	}
}

func TestListRepositoriesProviderSelection(t *testing.T) {
	eligible := newFake()
	eligible.name = "eligible"
	eligible.repos = []provider.Repository{{Provider: "eligible", Path: "archive/a"}}
	ineligible := newFake()
	ineligible.name = "ineligible"
	ineligible.repos = []provider.Repository{{Provider: "ineligible", Path: "archive/b"}}

	env := newMultiEnv(t,
		[]*fakeProvider{eligible, ineligible},
		map[string][]policy.RuleSpec{
			"eligible":   listReposRules("archive/**"),
			"ineligible": allowRules("mr:read"),
		},
	)

	// Omitted provider is rejected: provider is required.
	res := env.call(t, "list_repositories", map[string]any{})
	if !res.IsError {
		t.Fatalf("list_repositories with omitted provider succeeded: %s", resultText(t, res))
	}

	// Explicit eligible provider lists its repositories.
	res = env.call(t, "list_repositories", map[string]any{"provider": "eligible"})
	if res.IsError {
		t.Fatalf("list_repositories(eligible): %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 1 || out.Repositories[0].Provider != "eligible" {
		t.Fatalf("repositories = %v, want only eligible", out.Repositories)
	}

	// Explicit provider without repo:list is not an error: it returns only the
	// static repositories and never calls the provider API.
	res = env.call(t, "list_repositories", map[string]any{"provider": "ineligible"})
	if res.IsError {
		t.Fatalf("list_repositories(ineligible) errored: %s", resultText(t, res))
	}
	out = repoJSON(t, res)
	if len(out.Repositories) != 1 || out.Repositories[0].Path != "team/app" {
		t.Fatalf("ineligible repositories = %v, want static [team/app]", out.Repositories)
	}
	if ineligible.listReposCalls != 0 {
		t.Errorf("ineligible provider ListRepositories called %d times, want 0", ineligible.listReposCalls)
	}

	// Explicit unknown provider is an error.
	res = env.call(t, "list_repositories", map[string]any{"provider": "missing"})
	if !res.IsError || !strings.Contains(resultText(t, res), "unknown provider") {
		t.Errorf("unknown provider result = %q", resultText(t, res))
	}
}

func TestListRepositoriesSchemaRequiresProvider(t *testing.T) {
	env := newTestEnv(t, listReposRules("archive/**"), newFake())

	tools, err := env.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema any
	for _, tool := range tools.Tools {
		if tool.Name == "list_repositories" {
			schema = tool.InputSchema
		}
	}
	if schema == nil {
		t.Fatal("list_repositories tool not found")
	}
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	required, _ := parsed["required"].([]any)
	found := false
	for _, field := range required {
		if field == "provider" {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider is not required in list_repositories input schema: %s", data)
	}
}

func TestListMergeRequestsTruncation(t *testing.T) {
	fake := newFake()
	fake.mrs = nil
	for i := 0; i < maxListResults+5; i++ {
		fake.mrs = append(fake.mrs, provider.MergeRequest{Number: int64(i + 1), Title: "mr"})
	}
	env := newTestEnv(t, allowRules("mr:read"), fake)

	res := env.call(t, "list_merge_requests", mrArgs())
	if res.IsError {
		t.Fatalf("list_merge_requests: %s", resultText(t, res))
	}
	var out listMergeRequestsOutput
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.Truncated {
		t.Error("truncated = false, want true")
	}
	if len(out.MergeRequests) != maxListResults {
		t.Errorf("len = %d, want %d", len(out.MergeRequests), maxListResults)
	}
}

func TestListMergeRequestNotesTruncation(t *testing.T) {
	fake := newFake()
	fake.notes = nil
	for i := 0; i < maxListResults+5; i++ {
		fake.notes = append(fake.notes, provider.Note{ID: int64(i + 1), Body: "note"})
	}
	env := newTestEnv(t, allowRules("mr:read"), fake)

	res := env.call(t, "list_merge_request_notes", map[string]any{"provider": "fake", "repo": "team/app", "number": 1})
	if res.IsError {
		t.Fatalf("list_merge_request_notes: %s", resultText(t, res))
	}
	var out listNotesOutput
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.Truncated {
		t.Error("truncated = false, want true")
	}
	if len(out.Notes) != maxListResults {
		t.Errorf("len = %d, want %d", len(out.Notes), maxListResults)
	}
}

func TestTruncateTextRuneSafe(t *testing.T) {
	s := "a€b" // 'a' (1 byte) + '€' (3 bytes) + 'b' (1 byte)
	if got := truncateText(s, 3); got != "a"+truncatedMarker {
		t.Errorf("truncateText(s, 3) = %q, want %q", got, "a"+truncatedMarker)
	}
	if got := truncateText(s, len(s)); got != s {
		t.Errorf("truncateText(s, len(s)) = %q, want %q", got, s)
	}
	if got := safePrefix(s, 4); got != "a€" {
		t.Errorf("safePrefix(s, 4) = %q, want %q", got, "a€")
	}
	if got := safePrefix(s, 2); got != "a" {
		t.Errorf("safePrefix(s, 2) = %q, want %q", got, "a")
	}
	got := truncateText(s, 3)
	if !utf8.ValidString(strings.TrimSuffix(got, truncatedMarker)) {
		t.Errorf("truncated prefix is not valid UTF-8: %q", got)
	}
}

func TestTokenNeverLeaks(t *testing.T) {
	const token = "supersecrettoken123"
	fake := newFake()
	fake.providerErr = errors.New("upstream failure using " + token)
	env := newTestEnv(t, allowRules("mr:read"), fake)

	res := env.call(t, "list_merge_requests", mrArgs())
	if !res.IsError {
		t.Fatal("expected provider error to surface as a tool error")
	}
	if strings.Contains(resultText(t, res), token) {
		t.Errorf("token leaked into tool output: %s", resultText(t, res))
	}
	if strings.Contains(env.logs.String(), token) {
		t.Errorf("token leaked into logs: %s", env.logs.String())
	}
}

func TestUnknownProviderAndRepository(t *testing.T) {
	env := newTestEnv(t, allowRules("mr:read"), newFake())

	res := env.call(t, "list_merge_requests", map[string]any{"provider": "nope", "repo": "team/app"})
	if !res.IsError || !strings.Contains(resultText(t, res), "unknown provider") {
		t.Errorf("unknown provider result = %q (isError=%v)", resultText(t, res), res.IsError)
	}

	res = env.call(t, "list_merge_requests", map[string]any{"provider": "fake", "repo": "other/repo"})
	if !res.IsError || !strings.Contains(resultText(t, res), "unknown repository") {
		t.Errorf("unknown repository result = %q (isError=%v)", resultText(t, res), res.IsError)
	}
}

func filteredReadRules() []policy.RuleSpec {
	return []policy.RuleSpec{{
		Repositories: []string{"team/app"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{
			Name:   policy.CapMRRead,
			Filter: policy.TagFilter{Require: []string{"ai-reviewed"}, Exclude: []string{"do-not-touch"}},
		}},
	}}
}

func filteredCommentRules() []policy.RuleSpec {
	return []policy.RuleSpec{{
		Repositories: []string{"team/app"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{
			Name:   policy.CapMRComment,
			Filter: policy.TagFilter{Require: []string{"ai-reviewed"}},
		}},
	}}
}

func setLabels(f *fakeProvider, known bool, labels ...string) {
	f.mrs[0].LabelsKnown = known
	f.mrs[0].Labels = labels
}

func TestGetMergeRequestTagFilter(t *testing.T) {
	fake := newFake()
	setLabels(fake, true, "ai-reviewed")
	env := newTestEnv(t, filteredReadRules(), fake)

	res := env.call(t, "get_merge_request", map[string]any{"provider": "fake", "repo": "team/app", "number": 1})
	if res.IsError {
		t.Fatalf("matching label denied: %s", resultText(t, res))
	}
}

func TestGetMergeRequestExcludedTag(t *testing.T) {
	fake := newFake()
	setLabels(fake, true, "ai-reviewed", "do-not-touch")
	env := newTestEnv(t, filteredReadRules(), fake)

	res := env.call(t, "get_merge_request", map[string]any{"provider": "fake", "repo": "team/app", "number": 1})
	if !res.IsError {
		t.Fatal("excluded tag allowed, want denial")
	}
}

func TestGetMergeRequestUnknownLabelsFailClosed(t *testing.T) {
	fake := newFake()
	setLabels(fake, false, "ai-reviewed")
	env := newTestEnv(t, filteredReadRules(), fake)

	res := env.call(t, "get_merge_request", map[string]any{"provider": "fake", "repo": "team/app", "number": 1})
	if !res.IsError {
		t.Fatal("unknown labels allowed an active filter, want denial")
	}
}

func TestListMergeRequestNotesTagFilter(t *testing.T) {
	fake := newFake()
	setLabels(fake, true, "ai-reviewed")
	env := newTestEnv(t, filteredReadRules(), fake)

	res := env.call(t, "list_merge_request_notes", map[string]any{"provider": "fake", "repo": "team/app", "number": 1})
	if res.IsError {
		t.Fatalf("matching label denied: %s", resultText(t, res))
	}

	setLabels(fake, true)
	res = env.call(t, "list_merge_request_notes", map[string]any{"provider": "fake", "repo": "team/app", "number": 1})
	if !res.IsError {
		t.Fatal("missing required tag allowed, want denial")
	}
}

func TestAddMergeRequestNoteTagFilter(t *testing.T) {
	fake := newFake()
	setLabels(fake, true, "ai-reviewed")
	env := newTestEnv(t, filteredCommentRules(), fake)

	res := env.call(t, "add_merge_request_note", map[string]any{
		"provider": "fake", "repo": "team/app", "number": 1, "body": "hello",
	})
	if res.IsError {
		t.Fatalf("matching label denied: %s", resultText(t, res))
	}
	if len(fake.added) != 1 {
		t.Fatalf("added = %v, want one note", fake.added)
	}

	setLabels(fake, true)
	res = env.call(t, "add_merge_request_note", map[string]any{
		"provider": "fake", "repo": "team/app", "number": 1, "body": "second",
	})
	if !res.IsError {
		t.Fatal("missing required tag allowed a note, want denial")
	}
	if len(fake.added) != 1 {
		t.Errorf("added = %v, want no extra note", fake.added)
	}
}

func TestAddMergeRequestNoteMetadataErrorDenies(t *testing.T) {
	fake := newFake()
	fake.providerErr = errors.New("metadata unavailable")
	env := newTestEnv(t, filteredCommentRules(), fake)

	res := env.call(t, "add_merge_request_note", map[string]any{
		"provider": "fake", "repo": "team/app", "number": 1, "body": "hello",
	})
	if !res.IsError {
		t.Fatal("metadata fetch error did not deny the post")
	}
	if len(fake.added) != 0 {
		t.Errorf("added = %v, want no note on fail-closed", fake.added)
	}
}

func TestListMergeRequestsFailsClosedWithReadFilter(t *testing.T) {
	env := newTestEnv(t, filteredReadRules(), newFake())

	res := env.call(t, "list_merge_requests", mrArgs())
	if !res.IsError {
		t.Fatal("list_merge_requests allowed an active mr:read tag filter, want denial")
	}
}

func TestListConfiguredRulesExposesFilters(t *testing.T) {
	env := newTestEnv(t, filteredReadRules(), newFake())

	res := env.call(t, "list_configured_rules", map[string]any{})
	if res.IsError {
		t.Fatalf("list_configured_rules: %s", resultText(t, res))
	}
	var out listConfiguredRulesOutput
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Repositories) != 1 || len(out.Repositories[0].ConfiguredCapabilities) != 1 {
		t.Fatalf("configured rules = %+v", out)
	}
	cap := out.Repositories[0].ConfiguredCapabilities[0]
	if cap.Name != "mr:read" {
		t.Errorf("capability name = %q, want mr:read", cap.Name)
	}
	if len(cap.Require) != 1 || cap.Require[0] != "ai-reviewed" {
		t.Errorf("require = %v, want [ai-reviewed]", cap.Require)
	}
	if len(cap.Exclude) != 1 || cap.Exclude[0] != "do-not-touch" {
		t.Errorf("exclude = %v, want [do-not-touch]", cap.Exclude)
	}
}

func TestLabelsNeverReturned(t *testing.T) {
	fake := newFake()
	setLabels(fake, true, "internal-only", "secret-label-value")
	env := newTestEnv(t, allowRules("mr:read", "mr:comment"), fake)

	calls := []struct {
		name string
		args map[string]any
	}{
		{"get_merge_request", map[string]any{"provider": "fake", "repo": "team/app", "number": 1}},
		{"add_merge_request_note", map[string]any{"provider": "fake", "repo": "team/app", "number": 1, "body": "hi"}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			res := env.call(t, c.name, c.args)
			if res.IsError {
				t.Fatalf("%s: %s", c.name, resultText(t, res))
			}
			lower := strings.ToLower(resultText(t, res))
			for _, forbidden := range []string{"labels", "labels_known", "label"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("%s output contains %q: %s", c.name, forbidden, resultText(t, res))
				}
			}
		})
	}
}

func repoReadTopicRules() []policy.RuleSpec {
	return []policy.RuleSpec{{
		Repositories: []string{"team/app"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{
			Name:   policy.CapRepoRead,
			Filter: policy.TagFilter{Require: []string{"ai-ok"}, Exclude: []string{"confidential"}},
		}},
	}}
}

func repoListTopicRules() []policy.RuleSpec {
	return []policy.RuleSpec{{
		Repositories: []string{"team/app"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{
			Name:   policy.CapRepoList,
			Filter: policy.TagFilter{Require: []string{"ai-ok"}},
		}},
	}}
}

func readFileArgs() map[string]any {
	return map[string]any{"provider": "fake", "repo": "team/app", "path": "README.md"}
}

func TestReadFileRepoReadTopicFilter(t *testing.T) {
	tests := []struct {
		name      string
		topics    map[string][]string
		topicsErr map[string]error
		wantError bool
	}{
		{"matching topic", map[string][]string{"team/app": {"ai-ok"}}, nil, false},
		{"missing required topic", map[string][]string{"team/app": {"other"}}, nil, true},
		{"excluded topic present", map[string][]string{"team/app": {"ai-ok", "confidential"}}, nil, true},
		{"topic fetch error", nil, map[string]error{"team/app": errors.New("boom")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake()
			fake.topics = tt.topics
			fake.topicsErr = tt.topicsErr
			env := newTestEnv(t, repoReadTopicRules(), fake)

			res := env.call(t, "read_file", readFileArgs())
			if res.IsError != tt.wantError {
				t.Fatalf("isError = %v, want %v: %s", res.IsError, tt.wantError, resultText(t, res))
			}
			if fake.topicsCalls != 1 {
				t.Errorf("GetRepositoryTopics called %d times, want exactly 1", fake.topicsCalls)
			}
		})
	}
}

func TestReadFileNoRepoFilterNoTopicCall(t *testing.T) {
	fake := newFake()
	env := newTestEnv(t, allowRules("repo:read"), fake)

	res := env.call(t, "read_file", readFileArgs())
	if res.IsError {
		t.Fatalf("read_file denied: %s", resultText(t, res))
	}
	if fake.topicsCalls != 0 {
		t.Errorf("GetRepositoryTopics called %d times, want 0 when no filter is active", fake.topicsCalls)
	}
}

func TestNoAIDeniesReadFileWhenTopicFilterPasses(t *testing.T) {
	fake := newFake()
	fake.marker = true
	fake.topics = map[string][]string{"team/app": {"ai-ok"}}
	env := newTestEnv(t, repoReadTopicRules(), fake)

	res := env.call(t, "read_file", readFileArgs())
	if !res.IsError || !strings.Contains(resultText(t, res), ".noai") {
		t.Fatalf("read_file result = %q (isError=%v), want .noai denial", resultText(t, res), res.IsError)
	}
}

func TestListRepositoriesStaticTopicFilter(t *testing.T) {
	tests := []struct {
		name        string
		topics      map[string][]string
		topicsErr   map[string]error
		wantCount   int
		wantOmitted int
	}{
		{"matching topic included", map[string][]string{"team/app": {"ai-ok"}}, nil, 1, 0},
		{"missing topic omitted", map[string][]string{"team/app": {"other"}}, nil, 0, 1},
		{"topic error omitted", nil, map[string]error{"team/app": errors.New("boom")}, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake()
			fake.topics = tt.topics
			fake.topicsErr = tt.topicsErr
			env := newTestEnv(t, repoListTopicRules(), fake)

			res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
			if res.IsError {
				t.Fatalf("list_repositories: %s", resultText(t, res))
			}
			out := repoJSON(t, res)
			if len(out.Repositories) != tt.wantCount {
				t.Errorf("repositories = %v, want %d", out.Repositories, tt.wantCount)
			}
			if out.Omitted != tt.wantOmitted {
				t.Errorf("omitted = %d, want %d", out.Omitted, tt.wantOmitted)
			}
			if fake.topicsCalls != 1 {
				t.Errorf("GetRepositoryTopics called %d times, want exactly 1", fake.topicsCalls)
			}
		})
	}
}

func TestListRepositoriesStaticNoFilterNoTopicCall(t *testing.T) {
	fake := newFake()
	env := newTestEnv(t, allowRules("repo:list"), fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 1 || out.Repositories[0].Path != "team/app" {
		t.Fatalf("repositories = %v, want [team/app]", out.Repositories)
	}
	if fake.topicsCalls != 0 {
		t.Errorf("GetRepositoryTopics called %d times, want 0 when no repo:list filter is active", fake.topicsCalls)
	}
}

func TestListRepositoriesDiscoveredTopicFilter(t *testing.T) {
	fake := newFake()
	fake.repos = []provider.Repository{
		{Provider: "fake", Path: "archive/good", Topics: []string{"ai-ok"}, TopicsKnown: true},
		{Provider: "fake", Path: "archive/bad", Topics: []string{"other"}, TopicsKnown: true},
		{Provider: "fake", Path: "archive/unknown", TopicsKnown: false},
	}
	rules := []policy.RuleSpec{{
		Repositories: []string{"archive/**"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{
			Name:   policy.CapRepoList,
			Filter: policy.TagFilter{Require: []string{"ai-ok"}},
		}},
	}}
	env := newTestEnv(t, rules, fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake"})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 1 || out.Repositories[0].Path != "archive/good" {
		t.Fatalf("repositories = %v, want [archive/good]", out.Repositories)
	}
	if out.Omitted != 2 {
		t.Errorf("omitted = %d, want 2 (non-matching + unknown topics)", out.Omitted)
	}
}

func TestListRepositoriesStaticDoesNotCountTowardLimit(t *testing.T) {
	fake := newFake()
	fake.repos = []provider.Repository{{Provider: "fake", Path: "archive/a", WebURL: "https://x/archive/a"}}
	rules := []policy.RuleSpec{{
		Repositories: []string{"team/app", "archive/**"},
		Effect:       "allow",
		Capabilities: grants("repo:list"),
	}}
	env := newTestEnv(t, rules, fake)

	res := env.call(t, "list_repositories", map[string]any{"provider": "fake", "limit": 1})
	if res.IsError {
		t.Fatalf("list_repositories: %s", resultText(t, res))
	}
	out := repoJSON(t, res)
	if len(out.Repositories) != 2 {
		t.Fatalf("repositories = %v, want static team/app + discovered archive/a", out.Repositories)
	}
	if out.Repositories[0].Path != "team/app" || out.Repositories[1].Path != "archive/a" {
		t.Errorf("repositories = %v, want [team/app archive/a]", out.Repositories)
	}
	if out.Truncated {
		t.Error("truncated = true, want false: static entries must not count toward the discovered cap")
	}
}

func TestTopicsNeverReturned(t *testing.T) {
	fake := newFake()
	fake.repos = []provider.Repository{{
		Provider:    "fake",
		Path:        "archive/a",
		WebURL:      "https://x/archive/a",
		Topics:      []string{"ai-ok", "secret-topic"},
		TopicsKnown: true,
	}}
	fake.topics = map[string][]string{"team/app": {"ai-ok"}}
	rules := []policy.RuleSpec{{
		Repositories: []string{"team/app", "archive/**"},
		Effect:       "allow",
		Capabilities: grants("repo:read", "repo:list"),
	}}
	env := newTestEnv(t, rules, fake)

	calls := []struct {
		name string
		args map[string]any
	}{
		{"list_repositories", map[string]any{"provider": "fake"}},
		{"read_file", readFileArgs()},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			res := env.call(t, c.name, c.args)
			if res.IsError {
				t.Fatalf("%s: %s", c.name, resultText(t, res))
			}
			lower := strings.ToLower(resultText(t, res))
			for _, forbidden := range []string{"topics", "topics_known"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("%s output contains %q: %s", c.name, forbidden, resultText(t, res))
				}
			}
		})
	}
}

func filteredRebaseRules() []policy.RuleSpec {
	return []policy.RuleSpec{{
		Repositories: []string{"team/app"},
		Effect:       "allow",
		Capabilities: []policy.CapabilityGrant{{
			Name:   policy.CapRebase,
			Filter: policy.TagFilter{Require: []string{"ai-reviewed"}, Exclude: []string{"do-not-touch"}},
		}},
	}}
}

func rebaseArgs() map[string]any {
	return map[string]any{"provider": "fake", "repo": "team/app", "number": 1}
}

func TestRebaseMergeRequestAllowed(t *testing.T) {
	fake := newFake()
	env := newTestEnv(t, allowRules("mr:rebase"), fake)

	res := env.call(t, "rebase_merge_request", rebaseArgs())
	if res.IsError {
		t.Fatalf("rebase_merge_request denied: %s", resultText(t, res))
	}
	if fake.rebaseCalls != 1 {
		t.Errorf("RebaseMergeRequest called %d times, want 1", fake.rebaseCalls)
	}
	if !strings.Contains(resultText(t, res), "rebase requested") {
		t.Errorf("output = %q, want rebase requested status", resultText(t, res))
	}
}

func TestRebaseMergeRequestRequiresCapability(t *testing.T) {
	fake := newFake()
	env := newTestEnv(t, allowRules("mr:read"), fake)

	res := env.call(t, "rebase_merge_request", rebaseArgs())
	if !res.IsError {
		t.Fatal("rebase_merge_request succeeded without mr:rebase")
	}
	if fake.rebaseCalls != 0 {
		t.Errorf("RebaseMergeRequest called %d times, want 0", fake.rebaseCalls)
	}
}

func TestRebaseMergeRequestTagFilter(t *testing.T) {
	t.Run("matching label allows", func(t *testing.T) {
		fake := newFake()
		setLabels(fake, true, "ai-reviewed")
		env := newTestEnv(t, filteredRebaseRules(), fake)

		res := env.call(t, "rebase_merge_request", rebaseArgs())
		if res.IsError {
			t.Fatalf("matching label denied: %s", resultText(t, res))
		}
		if fake.rebaseCalls != 1 {
			t.Errorf("RebaseMergeRequest called %d times, want 1", fake.rebaseCalls)
		}
	})
	t.Run("excluded label denies", func(t *testing.T) {
		fake := newFake()
		setLabels(fake, true, "ai-reviewed", "do-not-touch")
		env := newTestEnv(t, filteredRebaseRules(), fake)

		res := env.call(t, "rebase_merge_request", rebaseArgs())
		if !res.IsError {
			t.Fatal("excluded label allowed the rebase")
		}
		if fake.rebaseCalls != 0 {
			t.Errorf("RebaseMergeRequest called %d times, want 0", fake.rebaseCalls)
		}
	})
	t.Run("unknown labels fail closed", func(t *testing.T) {
		fake := newFake()
		setLabels(fake, false, "ai-reviewed")
		env := newTestEnv(t, filteredRebaseRules(), fake)

		res := env.call(t, "rebase_merge_request", rebaseArgs())
		if !res.IsError {
			t.Fatal("unknown labels allowed an active filter")
		}
		if fake.rebaseCalls != 0 {
			t.Errorf("RebaseMergeRequest called %d times, want 0", fake.rebaseCalls)
		}
	})
}

func TestRebaseMergeRequestMetadataErrorDenies(t *testing.T) {
	fake := newFake()
	fake.providerErr = errors.New("metadata unavailable")
	env := newTestEnv(t, allowRules("mr:rebase"), fake)

	res := env.call(t, "rebase_merge_request", rebaseArgs())
	if !res.IsError {
		t.Fatal("metadata fetch error did not deny the rebase")
	}
	if fake.rebaseCalls != 0 {
		t.Errorf("RebaseMergeRequest called %d times, want 0", fake.rebaseCalls)
	}
}

func TestRebaseMergeRequestProviderErrorIsSafe(t *testing.T) {
	fake := newFake()
	fake.rebaseErr = errors.New("403 forbidden secret-detail")
	env := newTestEnv(t, allowRules("mr:rebase"), fake)

	res := env.call(t, "rebase_merge_request", rebaseArgs())
	if !res.IsError {
		t.Fatal("provider rebase error did not surface as a tool error")
	}
	if strings.Contains(resultText(t, res), "secret-detail") {
		t.Errorf("provider error leaked: %s", resultText(t, res))
	}
	if fake.rebaseCalls != 1 {
		t.Errorf("RebaseMergeRequest called %d times, want 1", fake.rebaseCalls)
	}
}

func TestRebaseMergeRequestAllowedOnNoAIRepo(t *testing.T) {
	fake := newFake()
	fake.marker = true
	env := newTestEnv(t, allowRules("mr:rebase"), fake)

	res := env.call(t, "rebase_merge_request", rebaseArgs())
	if res.IsError {
		t.Fatalf("rebase_merge_request denied on a .noai repo: %s", resultText(t, res))
	}
	if fake.rebaseCalls != 1 {
		t.Errorf("RebaseMergeRequest called %d times, want 1", fake.rebaseCalls)
	}
}
