package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hvo/mcp-forj/internal/policy"
	"github.com/hvo/mcp-forj/internal/provider"
)

type fakeProvider struct {
	name        string
	marker      bool
	markerErr   error
	providerErr error
	files       map[string][]byte
	mrs         []provider.MergeRequest
	notes       []provider.Note
	added       []string
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Type() string { return "fake" }

func (f *fakeProvider) ListMergeRequests(context.Context, string, provider.ListOptions) ([]provider.MergeRequest, error) {
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	return f.mrs, nil
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

func (f *fakeProvider) ListMergeRequestNotes(context.Context, string, int64) ([]provider.Note, error) {
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	return f.notes, nil
}

func (f *fakeProvider) AddMergeRequestNote(_ context.Context, _ string, _ int64, body string) (*provider.Note, error) {
	if f.providerErr != nil {
		return nil, f.providerErr
	}
	f.added = append(f.added, body)
	return &provider.Note{ID: 1, Body: body, Author: "me", CreatedAt: time.Now()}, nil
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

func (f *fakeProvider) FileExists(context.Context, string, string, string) (bool, error) {
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

func allowRules(caps ...string) []policy.RuleSpec {
	return []policy.RuleSpec{{
		Repositories: []string{"team/app"},
		Effect:       "allow",
		Capabilities: caps,
	}}
}

func mrArgs() map[string]any {
	return map[string]any{"provider": "fake", "repo": "team/app"}
}

func TestNoAIDeniesCapabilityTools(t *testing.T) {
	fake := newFake()
	fake.marker = true
	env := newTestEnv(t, allowRules("mr:read", "mr:comment", "repo:read"), fake)

	calls := []struct {
		name string
		args map[string]any
	}{
		{"list_merge_requests", mrArgs()},
		{"get_merge_request", map[string]any{"provider": "fake", "repo": "team/app", "number": 1}},
		{"list_merge_request_notes", map[string]any{"provider": "fake", "repo": "team/app", "number": 1}},
		{"add_merge_request_note", map[string]any{"provider": "fake", "repo": "team/app", "number": 1, "body": "hi"}},
		{"read_file", map[string]any{"provider": "fake", "repo": "team/app", "path": "README.md"}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			res := env.call(t, c.name, c.args)
			if !res.IsError {
				t.Fatalf("%s succeeded, want .noai denial", c.name)
			}
			if !strings.Contains(resultText(t, res), ".noai") {
				t.Errorf("%s error = %q, want mention of .noai", c.name, resultText(t, res))
			}
		})
	}
}

func TestMarkerCheckErrorDenies(t *testing.T) {
	fake := newFake()
	fake.markerErr = errors.New("network down")
	env := newTestEnv(t, allowRules("mr:read"), fake)

	res := env.call(t, "list_merge_requests", mrArgs())
	if !res.IsError {
		t.Fatal("list_merge_requests succeeded despite marker check error")
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

func TestListRepositoriesReportsConfiguredCapabilities(t *testing.T) {
	fake := newFake()
	fake.marker = true // must not affect list_repositories
	env := newTestEnv(t, allowRules("mr:read", "mr:comment"), fake)

	res := env.call(t, "list_repositories", map[string]any{})
	if res.IsError {
		t.Fatalf("list_repositories denied: %s", resultText(t, res))
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
