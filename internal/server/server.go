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
	"unicode/utf8"

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
		Name:        "list_configured_rules",
		Description: "List the access rules and configured capabilities for this server. Configured capabilities may be further restricted by the .noai marker.",
	}, s.listConfiguredRules)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_repositories",
		Description: "Discover repositories the token is a member of that match the configured patterns. Results are capped and filtered per repository; truncated=true means the list may be incomplete because the cap was reached or the provider fetch window was exhausted. Requires the repo:list capability.",
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

type listConfiguredRulesInput struct{}

type listRepositoriesInput struct {
	Provider string `json:"provider,omitempty" jsonschema:"optional logical provider name; all listable providers when omitted"`
	Search   string `json:"search,omitempty" jsonschema:"optional provider-side search term"`
	Limit    int    `json:"limit,omitempty" jsonschema:"optional maximum number of results (capped at 100)"`
}

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

type listConfiguredRulesOutput struct {
	Repositories []configuredRepository `json:"repositories"`
}

type repositoryJSON struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
	WebURL   string `json:"web_url"`
}

type listRepositoriesOutput struct {
	Repositories []repositoryJSON `json:"repositories"`
	Omitted      int              `json:"omitted"`
	Truncated    bool             `json:"truncated"`
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

func (s *Server) listConfiguredRules(_ context.Context, _ *mcp.CallToolRequest, _ listConfiguredRulesInput) (*mcp.CallToolResult, any, error) {
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
	return jsonResult(listConfiguredRulesOutput{Repositories: repos})
}

func (s *Server) listRepositories(ctx context.Context, _ *mcp.CallToolRequest, in listRepositoriesInput) (*mcp.CallToolResult, any, error) {
	limit := in.Limit
	if limit <= 0 || limit > maxListResults {
		limit = maxListResults
	}

	names, err := s.listableProviders(in.Provider)
	if err != nil {
		return nil, nil, err
	}

	var (
		collected []repositoryJSON
		omitted   int
		truncated bool
		seen      = make(map[string]bool)
	)
	for _, name := range names {
		p, ok := s.registry.Get(name)
		if !ok {
			continue
		}
		repos, err := p.ListRepositories(ctx, provider.RepoListOptions{Search: in.Search, Limit: limit + 1})
		if err != nil {
			return nil, nil, mapProviderError(err)
		}
		// The provider fetch window itself was exhausted: there may be more
		// candidates that were never returned, so the listing is incomplete even
		// if filtering happens to leave room under the cap.
		if len(repos) > limit {
			truncated = true
		}
		for _, repo := range repos {
			key := name + "\x00" + repo.Path
			if seen[key] {
				continue
			}
			seen[key] = true

			if err := s.guard.Authorize(ctx, name, repo.Path, policy.CapRepoList); err != nil {
				if errors.Is(err, policy.ErrNoAI) || errors.Is(err, policy.ErrMarkerCheck) || errors.Is(err, policy.ErrDenied) {
					omitted++
				}
				continue
			}
			if len(collected) >= limit {
				truncated = true
				continue
			}
			collected = append(collected, repositoryJSON{
				Provider: name,
				Path:     repo.Path,
				WebURL:   repo.WebURL,
			})
		}
	}
	return jsonResult(listRepositoriesOutput{Repositories: collected, Omitted: omitted, Truncated: truncated})
}

// listableProviders returns the providers that may list repositories. When
// requested is non-empty it must be registered and grant repo:list; otherwise
// every registered provider that grants repo:list is returned.
func (s *Server) listableProviders(requested string) ([]string, error) {
	if requested != "" {
		if _, ok := s.registry.Get(requested); !ok {
			return nil, fmt.Errorf("unknown provider %q", requested)
		}
		if err := s.guard.AuthorizeList(requested); err != nil {
			return nil, mapAuthorizeListError(err, requested)
		}
		return []string{requested}, nil
	}
	names := s.registry.Names()
	listable := make([]string, 0, len(names))
	for _, name := range names {
		if err := s.guard.AuthorizeList(name); err == nil {
			listable = append(listable, name)
		}
	}
	return listable, nil
}

func mapAuthorizeListError(err error, providerName string) error {
	switch {
	case errors.Is(err, policy.ErrUnknownProvider):
		return fmt.Errorf("unknown provider %q", providerName)
	case errors.Is(err, policy.ErrDenied):
		return fmt.Errorf("provider %q does not grant repo:list", providerName)
	default:
		return errors.New("authorization failed")
	}
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
	mrs, err := p.ListMergeRequests(ctx, in.Repo, provider.ListOptions{State: in.State, Limit: limit + 1})
	if err != nil {
		return nil, nil, mapProviderError(err)
	}
	truncated := false
	if len(mrs) > limit {
		mrs = mrs[:limit]
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
	limit := maxListResults
	notes, err := p.ListMergeRequestNotes(ctx, in.Repo, in.Number, provider.ListOptions{Limit: limit + 1})
	if err != nil {
		return nil, nil, mapProviderError(err)
	}
	truncated := false
	if len(notes) > limit {
		notes = notes[:limit]
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
		content = safePrefix(string(data), maxFileBytes) + truncatedMarker
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

// truncateText limits s to at most max bytes without splitting a UTF-8 rune,
// appending truncatedMarker when truncation occurs.
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return safePrefix(s, max) + truncatedMarker
}

// safePrefix returns the longest prefix of s that is at most max bytes and ends
// on a UTF-8 rune boundary. Invalid trailing bytes are dropped.
func safePrefix(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if max >= len(s) {
		return s
	}
	p := s[:max]
	for len(p) > 0 {
		r, size := utf8.DecodeLastRuneInString(p)
		if r != utf8.RuneError || size > 1 {
			break
		}
		p = p[:len(p)-size]
	}
	return p
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
