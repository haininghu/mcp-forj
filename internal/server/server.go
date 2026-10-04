// Package server exposes the mcp-forj capabilities as MCP tools. Every
// capability-gated tool authorizes the request before touching a provider.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hvo/mcp-forj/internal/policy"
	"github.com/hvo/mcp-forj/internal/provider"
)

// Output limits. Oversized values are truncated and marked.
const (
	maxFileBytes    = 1 << 20 // 1 MiB
	maxTextBytes    = 64 << 10
	maxListResults  = 100
	truncatedMarker = "\n[truncated]"
)

// Server holds the MCP tool handlers and their dependencies.
type Server struct {
	guard    *policy.Guard
	registry *provider.Registry
	logger   *slog.Logger
}

// New constructs a Server.
func New(guard *policy.Guard, registry *provider.Registry, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{guard: guard, registry: registry, logger: logger}
}

// MCPServer builds an mcp.Server with all tools registered. version is
// advertised to clients and is typically injected at build time.
func (s *Server) MCPServer(version string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcp-forj", Version: version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_repositories",
		Description: "List the repositories and capabilities configured for this server. Configured capabilities may be further restricted by the .noai marker.",
	}, s.listRepositories)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_merge_requests",
		Description: "List merge requests for a repository. Requires the mr:read capability.",
	}, s.listMergeRequests)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_merge_request",
		Description: "Fetch a single merge request by number. Requires the mr:read capability.",
	}, s.getMergeRequest)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_merge_request_notes",
		Description: "List the comments on a merge request. Requires the mr:read capability.",
	}, s.listMergeRequestNotes)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "add_merge_request_note",
		Description: "Create a comment on a merge request. Requires the mr:comment capability.",
	}, s.addMergeRequestNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a repository file at an optional ref. Requires the repo:read capability.",
	}, s.readFile)

	return srv
}

type listRepositoriesInput struct{}

type listMergeRequestsInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Repo     string `json:"repo" jsonschema:"repository path (namespace/project)"`
	State    string `json:"state,omitempty" jsonschema:"optional merge request state filter"`
	Limit    int    `json:"limit,omitempty" jsonschema:"optional maximum number of results (capped at 100)"`
}

type getMergeRequestInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Repo     string `json:"repo" jsonschema:"repository path (namespace/project)"`
	Number   int64  `json:"number" jsonschema:"merge request number"`
}

type listMergeRequestNotesInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Repo     string `json:"repo" jsonschema:"repository path (namespace/project)"`
	Number   int64  `json:"number" jsonschema:"merge request number"`
}

type addMergeRequestNoteInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Repo     string `json:"repo" jsonschema:"repository path (namespace/project)"`
	Number   int64  `json:"number" jsonschema:"merge request number"`
	Body     string `json:"body" jsonschema:"comment text"`
}

type readFileInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Repo     string `json:"repo" jsonschema:"repository path (namespace/project)"`
	Path     string `json:"path" jsonschema:"repository-relative file path"`
	Ref      string `json:"ref,omitempty" jsonschema:"optional git ref; defaults to the default branch"`
}

type configuredRepository struct {
	Provider               string   `json:"provider"`
	Repositories           []string `json:"repositories"`
	Effect                 string   `json:"effect"`
	ConfiguredCapabilities []string `json:"configured_capabilities"`
}

type listRepositoriesOutput struct {
	Repositories []configuredRepository `json:"repositories"`
}

type mergeRequestJSON struct {
	Number       int64  `json:"number"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	Author       string `json:"author"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	WebURL       string `json:"web_url"`
}

type listMergeRequestsOutput struct {
	MergeRequests []mergeRequestJSON `json:"merge_requests"`
	Truncated     bool               `json:"truncated"`
}

type noteJSON struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

type listNotesOutput struct {
	Notes     []noteJSON `json:"notes"`
	Truncated bool       `json:"truncated"`
}

type readFileOutput struct {
	Provider  string `json:"provider"`
	Repo      string `json:"repo"`
	Path      string `json:"path"`
	Ref       string `json:"ref"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

func (s *Server) listRepositories(_ context.Context, _ *mcp.CallToolRequest, _ listRepositoriesInput) (*mcp.CallToolResult, any, error) {
	rules := s.guard.ConfiguredRules()
	repos := make([]configuredRepository, 0, len(rules))
	for _, rule := range rules {
		caps := make([]string, len(rule.Capabilities))
		for i, c := range rule.Capabilities {
			caps[i] = string(c)
		}
		repos = append(repos, configuredRepository{
			Provider:               rule.Provider,
			Repositories:           rule.Repositories,
			Effect:                 string(rule.Effect),
			ConfiguredCapabilities: caps,
		})
	}
	return jsonResult(listRepositoriesOutput{Repositories: repos})
}

func (s *Server) listMergeRequests(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestsInput) (*mcp.CallToolResult, any, error) {
	p, err := s.resolveAuthorized(ctx, in.Provider, in.Repo, policy.CapMRRead)
	if err != nil {
		return nil, nil, err
	}
	limit := in.Limit
	if limit <= 0 || limit > maxListResults {
		limit = maxListResults
	}
	mrs, err := p.ListMergeRequests(ctx, in.Repo, provider.ListOptions{State: in.State, Limit: limit})
	if err != nil {
		return nil, nil, mapProviderError(err)
	}
	truncated := false
	if len(mrs) > maxListResults {
		mrs = mrs[:maxListResults]
		truncated = true
	}
	out := make([]mergeRequestJSON, 0, len(mrs))
	for _, mr := range mrs {
		out = append(out, toMergeRequestJSON(mr))
	}
	return jsonResult(listMergeRequestsOutput{MergeRequests: out, Truncated: truncated})
}

func (s *Server) getMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestInput) (*mcp.CallToolResult, any, error) {
	if in.Number <= 0 {
		return nil, nil, errors.New("number must be positive")
	}
	p, err := s.resolveAuthorized(ctx, in.Provider, in.Repo, policy.CapMRRead)
	if err != nil {
		return nil, nil, err
	}
	mr, err := p.GetMergeRequest(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderError(err)
	}
	return jsonResult(toMergeRequestJSON(*mr))
}

func (s *Server) listMergeRequestNotes(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestNotesInput) (*mcp.CallToolResult, any, error) {
	if in.Number <= 0 {
		return nil, nil, errors.New("number must be positive")
	}
	p, err := s.resolveAuthorized(ctx, in.Provider, in.Repo, policy.CapMRRead)
	if err != nil {
		return nil, nil, err
	}
	notes, err := p.ListMergeRequestNotes(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderError(err)
	}
	truncated := false
	if len(notes) > maxListResults {
		notes = notes[:maxListResults]
		truncated = true
	}
	out := make([]noteJSON, 0, len(notes))
	for _, note := range notes {
		out = append(out, toNoteJSON(note))
	}
	return jsonResult(listNotesOutput{Notes: out, Truncated: truncated})
}

func (s *Server) addMergeRequestNote(ctx context.Context, _ *mcp.CallToolRequest, in addMergeRequestNoteInput) (*mcp.CallToolResult, any, error) {
	if in.Number <= 0 {
		return nil, nil, errors.New("number must be positive")
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, nil, errors.New("body must not be empty")
	}
	if len(in.Body) > maxTextBytes {
		return nil, nil, fmt.Errorf("body exceeds the %d byte limit", maxTextBytes)
	}
	p, err := s.resolveAuthorized(ctx, in.Provider, in.Repo, policy.CapMRComment)
	if err != nil {
		return nil, nil, err
	}
	note, err := p.AddMergeRequestNote(ctx, in.Repo, in.Number, in.Body)
	if err != nil {
		return nil, nil, mapProviderError(err)
	}
	return jsonResult(toNoteJSON(*note))
}

func (s *Server) readFile(ctx context.Context, _ *mcp.CallToolRequest, in readFileInput) (*mcp.CallToolResult, any, error) {
	cleaned, err := validatePath(in.Path)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.resolveAuthorized(ctx, in.Provider, in.Repo, policy.CapRepoRead)
	if err != nil {
		return nil, nil, err
	}
	data, err := p.ReadFile(ctx, in.Repo, cleaned, in.Ref)
	if err != nil {
		return nil, nil, mapProviderError(err)
	}
	content := string(data)
	truncated := false
	if len(data) > maxFileBytes {
		content = string(data[:maxFileBytes]) + truncatedMarker
		truncated = true
	}
	return jsonResult(readFileOutput{
		Provider:  in.Provider,
		Repo:      in.Repo,
		Path:      cleaned,
		Ref:       in.Ref,
		Content:   content,
		Truncated: truncated,
	})
}

func (s *Server) resolveAuthorized(ctx context.Context, providerName, repo string, c policy.Capability) (provider.Provider, error) {
	p, ok := s.registry.Get(providerName)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", providerName)
	}
	if err := s.guard.Authorize(ctx, providerName, repo, c); err != nil {
		return nil, mapAuthError(err, providerName, repo)
	}
	return p, nil
}

func mapAuthError(err error, providerName, repo string) error {
	switch {
	case errors.Is(err, policy.ErrUnknownProvider):
		return fmt.Errorf("unknown provider %q", providerName)
	case errors.Is(err, policy.ErrUnknownRepository):
		return fmt.Errorf("unknown repository %q for provider %q", repo, providerName)
	case errors.Is(err, policy.ErrDenied):
		return fmt.Errorf("access denied for repository %q", repo)
	case errors.Is(err, policy.ErrNoAI):
		return fmt.Errorf("repository %q is marked .noai and is off limits", repo)
	case errors.Is(err, policy.ErrMarkerCheck):
		return fmt.Errorf("could not verify the .noai marker for repository %q; access denied", repo)
	default:
		return errors.New("authorization failed")
	}
}

func mapProviderError(err error) error {
	if errors.Is(err, provider.ErrNotFound) {
		return errors.New("not found")
	}
	return errors.New("provider request failed")
}

// validatePath applies the ordered path-traversal checks from the design. It
// returns the cleaned path, which callers must use.
func validatePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path must not be empty")
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return "", fmt.Errorf("path %q must be relative", p)
	}
	for _, r := range p {
		if r == 0 || r < 0x20 || r == 0x7f {
			return "", errors.New("path contains control characters")
		}
	}
	for _, segment := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == ".." || segment == "." {
			return "", fmt.Errorf("path %q contains a relative segment", p)
		}
	}
	cleaned := path.Clean(p)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes the repository", p)
	}
	lower := strings.ToLower(p)
	for _, token := range []string{"%2e", "%2f", "%5c"} {
		if strings.Contains(lower, token) {
			return "", fmt.Errorf("path %q contains an encoded traversal sequence", p)
		}
	}
	return cleaned, nil
}

func toMergeRequestJSON(mr provider.MergeRequest) mergeRequestJSON {
	return mergeRequestJSON{
		Number:       mr.Number,
		Title:        mr.Title,
		Description:  truncateText(mr.Description, maxTextBytes),
		State:        mr.State,
		Author:       mr.Author,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		WebURL:       mr.WebURL,
	}
}

func toNoteJSON(note provider.Note) noteJSON {
	out := noteJSON{
		ID:     note.ID,
		Body:   truncateText(note.Body, maxTextBytes),
		Author: note.Author,
	}
	if !note.CreatedAt.IsZero() {
		out.CreatedAt = note.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return out
}

func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + truncatedMarker
}

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, nil, fmt.Errorf("encode result: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil, nil
}
