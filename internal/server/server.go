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
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hvo/mcp-forj/internal/policy"
	"github.com/hvo/mcp-forj/internal/provider"
)

// Output limits. Oversized values are truncated and marked.
const (
	maxFileBytes      = 1 << 20 // 1 MiB
	maxTextBytes      = 64 << 10
	maxListDescBytes  = 1 << 10 // 1 KiB per MR description in listings
	maxListResults    = 100     // MRs and notes
	maxRepoResults    = 1000    // repository discovery cap
	maxDiffFiles      = 100     // diff files per merge request
	maxDiffFileBytes  = 128 << 10
	maxDiffTotalBytes = 512 << 10
	maxRefBytes       = 255 // git ref length bound
	truncatedMarker   = "\n[truncated]"

	maxSearchResults  = 100 // commits returned by search_commits
	maxSearchRepos    = 50  // discovered candidates and named-repos cap per search_commits (literals uncapped)
	maxCommitsPerRepo = 200 // commit match limit and scan budget per repository
	maxAuthorBytes    = 255 // author filter length bound
)

// GitRemoteResolver resolves the git proxy clone URL for a repository and
// reports whether the proxy requires clients to authenticate with the proxy
// token (false in the token-less loopback-only mode). It is implemented by the
// git proxy server. A nil resolver disables the git_remote tool.
type GitRemoteResolver interface {
	RemoteURL(providerName, repo string) (string, error)
	AuthRequired() bool
}

// Server holds the MCP tool handlers and their dependencies.
type Server struct {
	guard     *policy.Guard
	registry  *provider.Registry
	logger    *slog.Logger
	gitRemote GitRemoteResolver
}

// New constructs a Server.
func New(guard *policy.Guard, registry *provider.Registry, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{guard: guard, registry: registry, logger: logger}
}

// SetGitRemoteResolver installs the resolver used by the git_remote tool. It
// must be called before MCPServer to register the tool.
func (s *Server) SetGitRemoteResolver(r GitRemoteResolver) { s.gitRemote = r }

// MCPServer builds a mcp.Server with all tools registered. version is
// advertised to clients and is typically injected at build time.
func (s *Server) MCPServer(version string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcp-forj", Version: version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_configured_rules",
		Description: "List the access rules and configured capabilities for providers that grant the policy:read capability. Other providers are omitted. Configured capabilities may be further restricted by the .noai marker unless the capability is exempted with `noai: allow`.",
	}, s.listConfiguredRules)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_repositories",
		Description: "List repositories explicitly configured plus repositories discovered through the provider API when the repo:list capability is granted. Literal repositories are returned unless a listing deny (a deny rule without capabilities, or one listing repo:list) hides them or an active repo:list tag filter excludes them; discovered .noai repositories are omitted unless the repo:list grant is exempted.",
	}, s.listRepositories)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_merge_requests",
		Description: "List merge requests for a repository. Requires the mr:read capability; .noai-protected unless the grant is exempted.",
	}, s.listMergeRequests)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_merge_request",
		Description: "Fetch a single merge request by number. Requires the mr:read capability; .noai-protected unless the grant is exempted.",
	}, s.getMergeRequest)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_merge_request_diff",
		Description: "Fetch the file diffs of a merge request. Requires the mr:diff capability; .noai-protected unless the grant is exempted.",
	}, s.getMergeRequestDiff)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_merge_request_notes",
		Description: "List the comments on a merge request. Requires the mr:read capability; .noai-protected unless the grant is exempted.",
	}, s.listMergeRequestNotes)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "add_merge_request_note",
		Description: "Create a comment on a merge request. Requires the mr:comment capability; .noai-protected unless the grant is exempted.",
	}, s.addMergeRequestNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "rebase_merge_request",
		Description: "Trigger an asynchronous rebase of a merge request's source branch onto its target branch. The outcome is visible later via get_merge_request (merge_error, rebase_in_progress). Requires the mr:rebase capability; .noai-protected unless the grant is exempted.",
	}, s.rebaseMergeRequest)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "merge_merge_request",
		Description: "Merge a merge request. Requires the mr:merge capability; .noai-protected unless the grant is exempted.",
	}, s.mergeMergeRequest)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_commits",
		Description: "Search the commit history of repositories for one author (default: the authenticated user), newest first, default branch only. IMPORTANT: if the request names one or more repositories, pass them in `repos` (namespace/project paths) to restrict the search to exactly those repositories; that path is content-only and needs the commit:read capability (implied by repo:read), with no repo:list required. If `repos` is omitted, the search covers the literal commit:read repositories and, when repo:list is granted, additionally discovers repositories. Optional filters: `since`/`until` (inclusive committer-date range) and `query` (case-insensitive substring of the commit message). Repositories you cannot access contribute no commits. .noai-protected unless the grant is exempted.",
	}, s.searchCommits)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a repository file at an optional ref. Requires the repo:read capability; .noai-protected unless the grant is exempted.",
	}, s.readFile)

	// git_remote only exists while the git proxy is enabled: without a
	// resolver there is no proxy URL to advertise.
	if s.gitRemote != nil {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "git_remote",
			Description: "Return the git smart-HTTP proxy clone URL for a repository together with the authentication mode: HTTP Basic with any username and the configured git proxy token as the password (the token is never returned by this tool), or auth.type `none` for a loopback-only proxy running without a token. Requires the repo:read capability; .noai-protected unless the grant is exempted.",
		}, s.handleGitRemote)
	}

	return srv
}

type listConfiguredRulesInput struct{}

type listRepositoriesInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Search   string `json:"search,omitempty" jsonschema:"optional provider-side search term; when omitted, search prefixes are derived from the repo:list rules"`
	Limit    int    `json:"limit,omitempty" jsonschema:"optional maximum number of discovered repositories (capped at 1000); configured repositories are always returned"`
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

type getMergeRequestDiffInput struct {
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

type rebaseMergeRequestInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Repo     string `json:"repo" jsonschema:"repository path (namespace/project)"`
	Number   int64  `json:"number" jsonschema:"merge request number"`
}

type mergeMergeRequestInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Repo     string `json:"repo" jsonschema:"repository path (namespace/project)"`
	Number   int64  `json:"number" jsonschema:"merge request number"`
}

type readFileInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Repo     string `json:"repo" jsonschema:"repository path (namespace/project)"`
	Path     string `json:"path" jsonschema:"repository-relative file path"`
	Ref      string `json:"ref,omitempty" jsonschema:"optional git ref; defaults to the default branch"`
}

type gitRemoteInput struct {
	Provider string `json:"provider" jsonschema:"logical provider name"`
	Repo     string `json:"repo" jsonschema:"repository path (namespace/project)"`
}

type searchCommitsInput struct {
	Provider string   `json:"provider" jsonschema:"logical provider name"`
	Repos    []string `json:"repos,omitempty" jsonschema:"optional list of repository paths (namespace/project) to search, e.g. [\"team/service-a\"]; set this whenever the request names repositories. When omitted, the literal commit:read repositories are searched and, if repo:list is granted, discovered ones too. At most 50 are searched."`
	Author   string   `json:"author,omitempty" jsonschema:"optional author filter (provider-side name/email search); defaults to the authenticated user"`
	Since    string   `json:"since,omitempty" jsonschema:"optional inclusive lower bound on the committer date, RFC3339 or YYYY-MM-DD"`
	Until    string   `json:"until,omitempty" jsonschema:"optional inclusive upper bound on the committer date, RFC3339 or YYYY-MM-DD; a date-only value covers the whole day (UTC)"`
	Query    string   `json:"query,omitempty" jsonschema:"optional case-insensitive substring matched client-side against the commit message (e.g. a ticket id)"`
	Limit    int      `json:"limit,omitempty" jsonschema:"optional maximum number of commits (capped at 100)"`
}

type configuredPathFilter struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

type configuredCapability struct {
	Name       string                `json:"name"`
	Require    []string              `json:"require,omitempty"`
	Exclude    []string              `json:"exclude,omitempty"`
	Paths      *configuredPathFilter `json:"paths,omitempty"`
	Branches   *configuredPathFilter `json:"branches,omitempty"`
	NoAIExempt bool                  `json:"noai_exempt,omitempty"`
}

type configuredRepository struct {
	Provider               string                 `json:"provider"`
	Repositories           []string               `json:"repositories"`
	Effect                 string                 `json:"effect"`
	ConfiguredCapabilities []configuredCapability `json:"configured_capabilities"`
}

type listConfiguredRulesOutput struct {
	Repositories []configuredRepository `json:"repositories"`
}

type repositoryJSON struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
	WebURL   string `json:"web_url,omitempty"`
}

type listRepositoriesOutput struct {
	Repositories []repositoryJSON `json:"repositories"`
	Truncated    bool             `json:"truncated"`
}

type mergeRequestJSON struct {
	Number              int64  `json:"number"`
	Title               string `json:"title"`
	Description         string `json:"description"`
	State               string `json:"state"`
	Author              string `json:"author"`
	SourceBranch        string `json:"source_branch"`
	TargetBranch        string `json:"target_branch"`
	WebURL              string `json:"web_url"`
	RebaseInProgress    bool   `json:"rebase_in_progress"`
	MergeError          string `json:"merge_error,omitempty"`
	HasConflicts        bool   `json:"has_conflicts"`
	DetailedMergeStatus string `json:"detailed_merge_status,omitempty"`
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

type diffFileJSON struct {
	OldPath       string `json:"old_path"`
	NewPath       string `json:"new_path"`
	NewFile       bool   `json:"new_file"`
	RenamedFile   bool   `json:"renamed_file"`
	DeletedFile   bool   `json:"deleted_file"`
	GeneratedFile bool   `json:"generated_file"`
	TooLarge      bool   `json:"too_large"`
	Diff          string `json:"diff"`
	Truncated     bool   `json:"truncated"`
}

type mergeRequestDiffOutput struct {
	Number    int64          `json:"number"`
	Files     []diffFileJSON `json:"files"`
	Truncated bool           `json:"truncated"`
}

type rebaseMergeRequestOutput struct {
	Provider string `json:"provider"`
	Repo     string `json:"repo"`
	Number   int64  `json:"number"`
	Status   string `json:"status"`
}

type readFileOutput struct {
	Provider  string `json:"provider"`
	Repo      string `json:"repo"`
	Path      string `json:"path"`
	Ref       string `json:"ref"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// gitRemoteAuthJSON describes how to authenticate against the git proxy. It
// deliberately carries no secret: in "http-basic" mode the password is the
// configured proxy token, which this tool never outputs; "none" is the
// token-less loopback-only mode.
type gitRemoteAuthJSON struct {
	Type     string `json:"type"`               // "http-basic" or "none"
	Username string `json:"username,omitempty"` // "any": the proxy ignores the username
	Note     string `json:"note"`               // where the password comes from
}

type gitRemoteOutput struct {
	Provider  string            `json:"provider"`
	Repo      string            `json:"repo"`
	RemoteURL string            `json:"remote_url"`
	Auth      gitRemoteAuthJSON `json:"auth"`
}

// commitJSON is one commit in the search_commits output. It deliberately
// carries no author email.
type commitJSON struct {
	Provider    string `json:"provider"`
	Repo        string `json:"repo"`
	SHA         string `json:"sha"`
	ShortSHA    string `json:"short_sha"`
	Title       string `json:"title"`
	Message     string `json:"message"`
	Author      string `json:"author"`
	AuthoredAt  string `json:"authored_at"`
	CommittedAt string `json:"committed_at"`
	WebURL      string `json:"web_url"`
}

type searchCommitsOutput struct {
	Commits   []commitJSON `json:"commits"`
	Truncated bool         `json:"truncated"`
}

// searchCommit is a commit paired with the repository it came from, used
// internally for deduplication and sorting before serialization.
type searchCommit struct {
	repo   string
	commit provider.Commit
}

// listConfiguredRules exposes the configured policy for providers whose rules
// grant the policy:read capability. Providers without it are omitted, so the
// policy is not disclosed by default. It never returns runtime labels/topics,
// .noai state or tokens. See ai/method-specs/list_configured_rules.md.
func (s *Server) listConfiguredRules(_ context.Context, _ *mcp.CallToolRequest, _ listConfiguredRulesInput) (*mcp.CallToolResult, any, error) {
	rules := s.guard.ConfiguredRules()
	authorized := make(map[string]bool)
	repos := make([]configuredRepository, 0, len(rules))
	for _, rule := range rules {
		ok, seen := authorized[rule.Provider]
		if !seen {
			ok = s.guard.AuthorizePolicyRead(rule.Provider) == nil
			authorized[rule.Provider] = ok
		}
		if !ok {
			continue
		}
		caps := make([]configuredCapability, len(rule.Capabilities))
		for i, grant := range rule.Capabilities {
			c := configuredCapability{
				Name:       string(grant.Name),
				Require:    grant.Filter.Require,
				Exclude:    grant.Filter.Exclude,
				NoAIExempt: grant.Filter.NoAIExempt,
			}
			if !grant.Filter.Paths.IsZero() {
				c.Paths = &configuredPathFilter{
					Include: grant.Filter.Paths.Include,
					Exclude: grant.Filter.Paths.Exclude,
				}
			}
			if !grant.Filter.Branches.IsZero() {
				c.Branches = &configuredPathFilter{
					Include: grant.Filter.Branches.Include,
					Exclude: grant.Filter.Branches.Exclude,
				}
			}
			caps[i] = c
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
	if limit <= 0 {
		limit = maxListResults
	}
	if limit > maxRepoResults {
		limit = maxRepoResults
	}

	name := in.Provider
	if name == "" {
		return nil, nil, errors.New("provider is required")
	}
	p, ok := s.registry.Get(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown provider %q", name)
	}

	static, err := s.guard.StaticRepositories(name)
	if err != nil {
		return nil, nil, fmt.Errorf("unknown provider %q", name)
	}

	var (
		collected       []repositoryJSON
		omitted         int
		truncated       bool
		discoveredCount int
		seen            = make(map[string]bool, len(static))
	)

	// Repositories listed literally in the configuration are returned (subject
	// to deny rules) with no capability, provider call or marker check. The one
	// exception is an active repo:list tag filter: it must also apply to static
	// repositories, otherwise the filter is trivially bypassed. In that case the
	// topics are fetched and evaluated, and a fetch error or non-match omits the
	// repository (fail-closed). They are not subject to the result limit: only
	// discovery is bounded.
	for _, repoPath := range static {
		seen[name+"\x00"+repoPath] = true

		hasFilter, err := s.guard.HasFilter(name, repoPath, policy.CapRepoList)
		if err != nil {
			return nil, nil, fmt.Errorf("unknown provider %q", name)
		}
		if hasFilter {
			tags, topicErr := fetchTopics(ctx, p, repoPath)
			if topicErr != nil {
				omitted++
				continue
			}
			decision, err := s.guard.EvaluateWithTags(name, repoPath, policy.CapRepoList, tags)
			if err != nil {
				return nil, nil, fmt.Errorf("unknown provider %q", name)
			}
			if !decision.Allowed {
				omitted++
				continue
			}
		}
		collected = append(collected, repositoryJSON{Provider: name, Path: repoPath})
	}

	// Dynamic discovery through the provider API requires repo:list. Without it
	// the listing simply contains the static repositories (possibly none). The
	// caller's search is a passthrough; when omitted, provider-side prefixes are
	// derived from the repo:list rules so glob patterns (e.g. "archive/**")
	// actually find their namespaces instead of relying on an unfiltered window.
	if s.guard.AuthorizeList(name) == nil {
		terms := []string{in.Search}
		if in.Search == "" {
			prefixes, err := s.guard.ListSearchPrefixes(name, policy.CapRepoList)
			if err != nil {
				return nil, nil, fmt.Errorf("unknown provider %q", name)
			}
			terms = prefixes
			if len(terms) == 0 {
				terms = []string{""}
			}
		}

		for _, term := range terms {
			remaining := max(limit-discoveredCount, 0)
			repos, err := p.ListRepositories(ctx, provider.RepoListOptions{Search: term, Limit: remaining + 1})
			if err != nil {
				return nil, nil, mapProviderError(err, "repository listing needs the Project: Read permission (and the group/project in the token scope)")
			}
			newCandidates := 0
			for _, repo := range repos {
				key := name + "\x00" + repo.Path
				if seen[key] {
					continue
				}
				seen[key] = true
				newCandidates++

				decision, err := s.guard.EvaluateWithTags(name, repo.Path, policy.CapRepoList, policy.TagSet{
					Known:  repo.TopicsKnown,
					Values: repo.Topics,
				})
				if err != nil {
					return nil, nil, fmt.Errorf("unknown provider %q", name)
				}
				if !decision.Allowed {
					// The count is logged server-side only; it is never
					// returned, so the client cannot tell how many repositories
					// are hidden.
					if decision.Matched {
						omitted++
					}
					continue
				}
				// .noai is a default-deny overlay: a discovered .noai
				// repository is omitted unless the repo:list grant exempts it.
				// A marker-check failure omits the candidate (fail-closed).
				if err := s.guard.CheckNoAI(ctx, name, repo.Path, decision.NoAIExempt); err != nil {
					omitted++
					continue
				}
				if discoveredCount >= limit {
					truncated = true
					continue
				}
				collected = append(collected, repositoryJSON{
					Provider: name,
					Path:     repo.Path,
					WebURL:   repo.WebURL,
				})
				discoveredCount++
			}
			// The provider fetch window itself was exhausted: this call offered
			// more distinct candidates than the remaining capacity, so there may
			// be more that were never returned. Duplicates across prefix searches
			// do not count.
			if newCandidates > remaining {
				truncated = true
			}
		}
	}

	// Deterministic output: sort by provider then path (static entries included).
	sort.SliceStable(collected, func(i, j int) bool {
		if collected[i].Provider != collected[j].Provider {
			return collected[i].Provider < collected[j].Provider
		}
		return collected[i].Path < collected[j].Path
	})

	// omitted is logged server-side only: returning it would tell the client that
	// more repositories exist than it can see.
	s.logger.Info("repository listing",
		"provider", name,
		"returned", len(collected),
		"omitted", omitted,
		"truncated", truncated,
	)
	return jsonResult(listRepositoriesOutput{Repositories: collected, Truncated: truncated})
}

// listMergeRequests lists merge request metadata. The GitLab list endpoint
// returns labels, so an active mr:read tag filter is enforced client-side here:
// each MR is evaluated with its own labels (unknown labels fail closed).
func (s *Server) listMergeRequests(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestsInput) (*mcp.CallToolResult, any, error) {
	limit := in.Limit
	if limit <= 0 || limit > maxListResults {
		limit = maxListResults
	}
	repo, err := validateRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}
	repo, err = p.CanonicalRepository(repo)
	if err != nil {
		return nil, nil, err
	}
	in.Repo = repo
	if err := s.guard.AuthorizeRepoCapability(in.Provider, in.Repo, policy.CapMRRead); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	// The list endpoint carries labels, so tags are evaluated per merge request
	// below; the .noai overlay is repository-level and is checked once here. The
	// exemption is conservative: unless every allow grant of mr:read is
	// noai-exempt, the marker is checked, because a per-item tag filter can fall
	// through to a non-exempt grant (ADR 0012).
	exempt, err := s.guard.NoAIExemptAll(in.Provider, in.Repo, policy.CapMRRead)
	if err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	if err := s.guard.CheckNoAI(ctx, in.Provider, in.Repo, exempt); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	mrs, err := p.ListMergeRequests(ctx, in.Repo, provider.ListOptions{State: in.State, Limit: limit + 1})
	if err != nil {
		return nil, nil, mapProviderError(err, "reading merge requests needs the Merge Request: Read permission")
	}

	// The provider fetch window itself was exhausted: there may be more
	// candidates that were never returned, so the listing is incomplete.
	truncated := len(mrs) > limit
	var (
		out     []mergeRequestJSON
		omitted int
	)
	for _, mr := range mrs {
		decision, err := s.guard.EvaluateWithTags(in.Provider, in.Repo, policy.CapMRRead, tagSetFromMR(mr))
		if err != nil {
			return nil, nil, mapAuthError(err, in.Provider, in.Repo)
		}
		if !decision.Allowed {
			if decision.Matched {
				omitted++
			}
			continue
		}
		if len(out) >= limit {
			truncated = true
			continue
		}
		out = append(out, toMergeRequestJSON(mr, maxListDescBytes))
	}
	// omitted is logged server-side only: returning it would tell the client that
	// more merge requests exist than it can see.
	s.logger.Info("merge request listing",
		"provider", in.Provider,
		"repo", in.Repo,
		"returned", len(out),
		"omitted", omitted,
		"truncated", truncated,
	)
	return jsonResult(listMergeRequestsOutput{MergeRequests: out, Truncated: truncated})
}

func (s *Server) getMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestInput) (*mcp.CallToolResult, any, error) {
	if in.Number <= 0 {
		return nil, nil, errors.New("number must be positive")
	}
	repo, err := validateRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}
	repo, err = p.CanonicalRepository(repo)
	if err != nil {
		return nil, nil, err
	}
	in.Repo = repo
	if err := s.guard.AuthorizeRepoCapability(in.Provider, in.Repo, policy.CapMRRead); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	mr, err := p.GetMergeRequest(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderError(err, "reading merge requests needs the Merge Request: Read permission")
	}
	if err := s.guard.AuthorizeWithTags(ctx, in.Provider, in.Repo, policy.CapMRRead, tagSetFromMR(*mr)); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	return jsonResult(toMergeRequestJSON(*mr, maxTextBytes))
}

// getMergeRequestDiff fetches the file diffs of a merge request. The MR metadata
// is fetched first as an internal authorization input for tag filters; a fetch
// failure denies the request (fail-closed). Diffs are `.noai`-protected unless
// the matched grant is exempted with `noai: allow`.
func (s *Server) getMergeRequestDiff(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestDiffInput) (*mcp.CallToolResult, any, error) {
	if in.Number <= 0 {
		return nil, nil, errors.New("number must be positive")
	}
	repo, err := validateRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}
	repo, err = p.CanonicalRepository(repo)
	if err != nil {
		return nil, nil, err
	}
	in.Repo = repo
	if err := s.guard.AuthorizeRepoCapability(in.Provider, in.Repo, policy.CapMRDiff); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	mr, err := p.GetMergeRequest(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderError(err, "reading merge requests needs the Merge Request: Read permission")
	}
	if err := s.guard.AuthorizeWithTags(ctx, in.Provider, in.Repo, policy.CapMRDiff, tagSetFromMR(*mr)); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	files, err := p.ListMergeRequestDiffs(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderError(err, "reading merge request diffs needs the Merge Request: Read permission")
	}
	out, truncated := toDiffFiles(files)
	return jsonResult(mergeRequestDiffOutput{Number: in.Number, Files: out, Truncated: truncated})
}

// toDiffFiles caps the number of files, truncates each file's diff, and enforces
// the total diff budget. Shrinking is signalled via the per-file and top-level
// truncated flags.
func toDiffFiles(files []provider.DiffFile) ([]diffFileJSON, bool) {
	out := make([]diffFileJSON, 0, len(files))
	total := 0
	truncated := false
	for i, f := range files {
		if i >= maxDiffFiles {
			truncated = true
			break
		}
		diff := f.Diff
		fileTruncated := false
		if len(diff) > maxDiffFileBytes {
			diff = truncateText(diff, maxDiffFileBytes)
			fileTruncated = true
		}
		if total+len(diff) > maxDiffTotalBytes {
			truncated = true
			break
		}
		total += len(diff)
		out = append(out, diffFileJSON{
			OldPath:       f.OldPath,
			NewPath:       f.NewPath,
			NewFile:       f.NewFile,
			RenamedFile:   f.RenamedFile,
			DeletedFile:   f.DeletedFile,
			GeneratedFile: f.GeneratedFile,
			TooLarge:      f.TooLarge,
			Diff:          diff,
			Truncated:     fileTruncated,
		})
	}
	return out, truncated
}

func (s *Server) listMergeRequestNotes(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestNotesInput) (*mcp.CallToolResult, any, error) {
	if in.Number <= 0 {
		return nil, nil, errors.New("number must be positive")
	}
	repo, err := validateRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}
	repo, err = p.CanonicalRepository(repo)
	if err != nil {
		return nil, nil, err
	}
	in.Repo = repo
	if err := s.guard.AuthorizeRepoCapability(in.Provider, in.Repo, policy.CapMRRead); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	mr, err := p.GetMergeRequest(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderError(err, "reading merge requests needs the Merge Request: Read permission")
	}
	if err := s.guard.AuthorizeWithTags(ctx, in.Provider, in.Repo, policy.CapMRRead, tagSetFromMR(*mr)); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	limit := maxListResults
	notes, err := p.ListMergeRequestNotes(ctx, in.Repo, in.Number, provider.ListOptions{Limit: limit + 1})
	if err != nil {
		return nil, nil, mapProviderError(err, "reading merge request notes needs the Work Item: Read permission")
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
	if err := validateNoteBody(in.Body); err != nil {
		return nil, nil, err
	}
	repo, err := validateRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}
	repo, err = p.CanonicalRepository(repo)
	if err != nil {
		return nil, nil, err
	}
	in.Repo = repo
	if err := s.guard.AuthorizeRepoCapability(in.Provider, in.Repo, policy.CapMRComment); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	// The merge request metadata is an internal authorization input. If it
	// cannot be fetched, the post is denied (fail-closed).
	mr, err := p.GetMergeRequest(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderError(err, "reading merge requests needs the Merge Request: Read permission")
	}
	if err := s.guard.AuthorizeWithTags(ctx, in.Provider, in.Repo, policy.CapMRComment, tagSetFromMR(*mr)); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	note, err := p.AddMergeRequestNote(ctx, in.Repo, in.Number, in.Body)
	if err != nil {
		return nil, nil, mapProviderError(err, "writing a merge request comment needs the Work Item: Create permission (and the project in scope)")
	}
	return jsonResult(toNoteJSON(*note))
}

// rebaseMergeRequest triggers an asynchronous rebase. The merge request
// metadata is fetched first as an internal authorization input (for tag
// filters); if it cannot be fetched the rebase is denied (fail-closed). The
// rebase itself completes asynchronously, so the result only acknowledges that
// it was requested.
func (s *Server) rebaseMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in rebaseMergeRequestInput) (*mcp.CallToolResult, any, error) {
	if in.Number <= 0 {
		return nil, nil, errors.New("number must be positive")
	}
	repo, err := validateRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}
	repo, err = p.CanonicalRepository(repo)
	if err != nil {
		return nil, nil, err
	}
	in.Repo = repo
	if err := s.guard.AuthorizeRepoCapability(in.Provider, in.Repo, policy.CapRebase); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	mr, err := p.GetMergeRequest(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderError(err, "reading merge requests needs the Merge Request: Read permission")
	}
	if err := s.guard.AuthorizeWithTags(ctx, in.Provider, in.Repo, policy.CapRebase, tagSetFromMR(*mr)); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	if err := p.RebaseMergeRequest(ctx, in.Repo, in.Number); err != nil {
		return nil, nil, mapProviderErrorState(err,
			"rebasing needs the Merge Request: Update permission and a role allowed to push to the source branch",
			"the merge request is not in a rebaseable state")
	}
	return jsonResult(rebaseMergeRequestOutput{
		Provider: in.Provider,
		Repo:     in.Repo,
		Number:   in.Number,
		Status:   "rebase requested",
	})
}

// mergeMergeRequest merges a merge request synchronously. The merge request
// metadata is fetched first as an internal authorization input (for tag
// filters); if it cannot be fetched the merge is denied (fail-closed). The
// merged merge request is returned.
func (s *Server) mergeMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in mergeMergeRequestInput) (*mcp.CallToolResult, any, error) {
	if in.Number <= 0 {
		return nil, nil, errors.New("number must be positive")
	}
	repo, err := validateRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}
	repo, err = p.CanonicalRepository(repo)
	if err != nil {
		return nil, nil, err
	}
	in.Repo = repo
	if err := s.guard.AuthorizeRepoCapability(in.Provider, in.Repo, policy.CapMRMerge); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	mr, err := p.GetMergeRequest(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderError(err, "reading merge requests needs the Merge Request: Read permission")
	}
	if err := s.guard.AuthorizeWithTags(ctx, in.Provider, in.Repo, policy.CapMRMerge, tagSetFromMR(*mr)); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	merged, err := p.MergeMergeRequest(ctx, in.Repo, in.Number)
	if err != nil {
		return nil, nil, mapProviderErrorState(err,
			"merging needs the Merge Request: Update permission and a role allowed to merge (typically Developer)",
			"the merge request is not in a mergeable state")
	}
	return jsonResult(toMergeRequestJSON(*merged, maxTextBytes))
}

// searchCommits performs a bounded cross-repository search for one author's
// commits. Argument validation (an explicit author and the time bounds) happens
// before any provider call. Candidate enumeration mirrors list_repositories:
// literal commit:read repositories are always candidates (subject to a listing
// deny and an active repo:list filter) and provider-side discovery requires
// repo:list. When enumeration yields no candidate the search returns an empty
// result immediately, without resolving the current identity, so an empty search
// never probes the token's account scope. Only then does an omitted author
// resolve to the authenticated user. The content axis requires the commit:read
// capability on every candidate (policy-only pre-check, tag evaluation, then the
// .noai overlay). Only each repository's default branch is searched. See
// ai/method-specs/search_commits.md.
func (s *Server) searchCommits(ctx context.Context, _ *mcp.CallToolRequest, in searchCommitsInput) (*mcp.CallToolResult, any, error) {
	limit := in.Limit
	if limit <= 0 || limit > maxSearchResults {
		limit = maxSearchResults
	}
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}

	var opts provider.CommitListOptions
	if author := strings.TrimSpace(in.Author); author != "" {
		if len(author) > maxAuthorBytes {
			return nil, nil, fmt.Errorf("author exceeds the %d byte limit", maxAuthorBytes)
		}
		for _, r := range author {
			if r == 0 || r < 0x20 || r == 0x7f {
				return nil, nil, errors.New("author contains control characters")
			}
		}
		opts.AuthorQuery = author
	}

	since, _, err := parseCommitTime(in.Since)
	if err != nil {
		return nil, nil, fmt.Errorf("since: %w", err)
	}
	until, untilDateOnly, err := parseCommitTime(in.Until)
	if err != nil {
		return nil, nil, fmt.Errorf("until: %w", err)
	}
	if untilDateOnly && !until.IsZero() {
		// A date-only upper bound is inclusive of the whole day (UTC).
		until = until.Add(24*time.Hour - time.Second)
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return nil, nil, errors.New("until must not be before since")
	}
	opts.Since, opts.Until = since, until

	var (
		candidates []commitCandidate
		truncated  bool
	)
	if len(in.Repos) > 0 {
		// Explicitly named repositories restrict the search to exactly that
		// set. This is pure content authorization, exactly like read_file: no
		// repo:list and no discovery, so a repository hidden from listing by a
		// deny repo:list rule is still searchable by name. Validation and
		// canonicalization failures are tool errors; content denials are
		// handled per candidate below and stay indistinguishable from an
		// unknown repository. The list is deduplicated on the canonical path
		// and capped at maxSearchRepos, consistently with the discovery budget.
		seen := make(map[string]bool, len(in.Repos))
		for _, raw := range in.Repos {
			repo, err := validateRepo(raw)
			if err != nil {
				return nil, nil, err
			}
			repo, err = p.CanonicalRepository(repo)
			if err != nil {
				return nil, nil, err
			}
			if seen[repo] {
				continue
			}
			if len(candidates) >= maxSearchRepos {
				truncated = true
				continue
			}
			seen[repo] = true
			// Topics stay unknown; the per-candidate loop below fetches them
			// only under a commit:read tag constraint (it validates and
			// canonicalizes again, which is idempotent).
			candidates = append(candidates, commitCandidate{repo: repo})
		}
	} else {
		candidates, truncated, err = s.searchCommitCandidates(ctx, p, in.Provider)
		if err != nil {
			return nil, nil, err
		}
	}
	if len(candidates) == 0 {
		// No candidate repository: return without resolving the current
		// identity. An empty search must not make a provider account call.
		s.logger.Info("commit search",
			"provider", in.Provider,
			"candidates", 0,
			"returned", 0,
			"omitted_repos", 0,
			"truncated", truncated,
		)
		return jsonResult(searchCommitsOutput{Commits: []commitJSON{}, Truncated: truncated})
	}

	if opts.AuthorQuery == "" {
		// Fail-closed: without an identity the search must not degrade into
		// returning every author's commits.
		u, err := p.CurrentUser(ctx)
		if err != nil {
			return nil, nil, mapProviderError(err, "reading your account needs the read_user scope")
		}
		switch {
		case u.Email != "":
			opts.Author = provider.CommitAuthor{Email: u.Email}
		case u.Name != "":
			opts.Author = provider.CommitAuthor{Name: u.Name}
		default:
			return nil, nil, errors.New("could not determine your commit identity; pass author explicitly")
		}
	}

	query := strings.ToLower(in.Query)
	var (
		collected []searchCommit
		seen      = make(map[string]bool)
		omitted   int
	)
	for _, candidate := range candidates {
		repo, err := validateRepo(candidate.repo)
		if err != nil {
			omitted++
			continue
		}
		repo, err = p.CanonicalRepository(repo)
		if err != nil {
			omitted++
			continue
		}
		// Listing visibility is already handled during enumeration: literal
		// candidates passed StaticRepositoriesFor (which applies the listing
		// deny) and discovered candidates passed the repo:list evaluation, so a
		// separate Guard.Listable check here would be redundant.
		if err := s.guard.AuthorizeRepoCapability(in.Provider, repo, policy.CapCommitRead); err != nil {
			omitted++
			continue
		}
		// Reuse topics already observed during enumeration (a repo:list filter
		// fetch); only fetch them again when a commit:read tag constraint needs
		// them and they are still unknown.
		tags := candidate.tags
		if hasTags, err := s.guard.HasTagConstraint(in.Provider, repo, policy.CapCommitRead, ""); err != nil {
			omitted++
			continue
		} else if hasTags && !tags.Known {
			tags, err = fetchTopics(ctx, p, repo)
			if err != nil {
				omitted++
				continue
			}
		}
		decision, err := s.guard.EvaluateWithTags(in.Provider, repo, policy.CapCommitRead, tags)
		if err != nil || !decision.Allowed {
			omitted++
			continue
		}
		if err := s.guard.CheckNoAI(ctx, in.Provider, repo, decision.NoAIExempt); err != nil {
			omitted++
			continue
		}
		// The provider bounds both matched commits and inspected commits, so a
		// prefilter that over-matches cannot page without bound; it reports
		// whether it stopped early.
		repoOpts := opts
		repoOpts.Limit = maxCommitsPerRepo
		repoOpts.ScanLimit = maxCommitsPerRepo
		repoCommits, repoTruncated, err := p.ListCommits(ctx, repo, repoOpts)
		if err != nil {
			// One repository failing to enumerate must not abort the whole
			// search; it is omitted and only counted server-side. The provider
			// error is not logged verbatim (error hygiene).
			omitted++
			s.logger.Warn("search_commits repository skipped", "provider", in.Provider, "repo", repo)
			continue
		}
		truncated = truncated || repoTruncated
		for _, commit := range repoCommits {
			if query != "" && !strings.Contains(strings.ToLower(commit.Message), query) {
				continue
			}
			key := repo + "\x00" + commit.SHA
			if seen[key] {
				continue
			}
			seen[key] = true
			collected = append(collected, searchCommit{repo: repo, commit: commit})
		}
	}

	// Newest first by committer date, then a stable tie-break on repo and sha.
	sort.SliceStable(collected, func(i, j int) bool {
		ti, tj := collected[i].commit.CommittedAt, collected[j].commit.CommittedAt
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if collected[i].repo != collected[j].repo {
			return collected[i].repo < collected[j].repo
		}
		return collected[i].commit.SHA < collected[j].commit.SHA
	})
	if len(collected) > limit {
		collected = collected[:limit]
		truncated = true
	}

	out := make([]commitJSON, 0, len(collected))
	for _, sc := range collected {
		out = append(out, toCommitJSON(in.Provider, sc.repo, sc.commit))
	}
	// omitted is logged server-side only: returning it would tell the client
	// that more repositories exist than it can see.
	s.logger.Info("commit search",
		"provider", in.Provider,
		"candidates", len(candidates),
		"returned", len(out),
		"omitted_repos", omitted,
		"truncated", truncated,
	)
	return jsonResult(searchCommitsOutput{Commits: out, Truncated: truncated})
}

// commitCandidate is a candidate repository for search_commits together with
// the project topics observed while enumerating it. Known topics let the
// commit:read tag gate reuse them instead of fetching again.
type commitCandidate struct {
	repo string
	tags policy.TagSet
}

// searchCommitCandidates enumerates the candidate repositories for
// search_commits. Enumeration is exactly list_repositories: literal
// repositories whose allow rules grant commit:read (directly or via repo:read)
// are always candidates (subject to a listing deny and an active repo:list
// filter), while provider-side discovery requires repo:list and evaluates every
// discovered repository against repo:list (topics, then the .noai overlay for
// the repo:list grant). The commit:read content gate runs later per candidate.
// Only discovery is capped at maxSearchRepos (literal candidates are not, as in
// list_repositories); exceeding the cap or a fetch window sets truncated. A
// discovery failure is fatal (fail-closed).
func (s *Server) searchCommitCandidates(ctx context.Context, p provider.Provider, providerName string) ([]commitCandidate, bool, error) {
	static, err := s.guard.StaticRepositoriesFor(providerName, policy.CapCommitRead)
	if err != nil {
		return nil, false, fmt.Errorf("unknown provider %q", providerName)
	}
	seen := make(map[string]bool, len(static))
	out := make([]commitCandidate, 0, len(static))
	for _, repoPath := range static {
		key := providerName + "\x00" + repoPath
		if seen[key] {
			continue
		}
		seen[key] = true

		// A literal repository is a candidate even without repo:list, but an
		// active repo:list tag filter still applies: its topics are fetched and
		// evaluated, and a fetch error or non-match omits it (fail-closed),
		// exactly as in list_repositories.
		hasFilter, err := s.guard.HasFilter(providerName, repoPath, policy.CapRepoList)
		if err != nil {
			return nil, false, fmt.Errorf("unknown provider %q", providerName)
		}
		if !hasFilter {
			// Topics stay unknown; the commit:read gate fetches them only under
			// a tag constraint.
			out = append(out, commitCandidate{repo: repoPath})
			continue
		}
		tags, topicErr := fetchTopics(ctx, p, repoPath)
		if topicErr != nil {
			continue
		}
		decision, err := s.guard.EvaluateWithTags(providerName, repoPath, policy.CapRepoList, tags)
		if err != nil {
			return nil, false, fmt.Errorf("unknown provider %q", providerName)
		}
		if !decision.Allowed {
			continue
		}
		out = append(out, commitCandidate{repo: repoPath, tags: tags})
	}

	// Dynamic discovery through the provider API requires repo:list, exactly as
	// in list_repositories: without it only the literal candidates are searched.
	if s.guard.AuthorizeList(providerName) != nil {
		return out, false, nil //nolint:nilerr // AuthorizeList's error means repo:list is not granted, not a failure.
	}
	prefixes, err := s.guard.ListSearchPrefixes(providerName, policy.CapRepoList)
	if err != nil {
		return nil, false, fmt.Errorf("unknown provider %q", providerName)
	}
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}

	// Only discovery is bounded, mirroring list_repositories: literal
	// repositories are never capped (ADR 0007, "literal repositories win").
	truncated := false
	discovered := 0
	for _, term := range prefixes {
		remaining := max(maxSearchRepos-discovered, 0)
		repos, err := p.ListRepositories(ctx, provider.RepoListOptions{Search: term, Limit: remaining + 1})
		if err != nil {
			return nil, false, mapProviderError(err, "repository listing needs the Project: Read permission (and the group/project in the token scope)")
		}
		newCandidates := 0
		for _, repo := range repos {
			key := providerName + "\x00" + repo.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			newCandidates++

			tags := policy.TagSet{Known: repo.TopicsKnown, Values: repo.Topics}
			decision, err := s.guard.EvaluateWithTags(providerName, repo.Path, policy.CapRepoList, tags)
			if err != nil {
				return nil, false, fmt.Errorf("unknown provider %q", providerName)
			}
			if !decision.Allowed {
				continue
			}
			// .noai is a default-deny overlay: a discovered .noai repository is
			// omitted unless the repo:list grant exempts it. A marker-check
			// failure omits the candidate (fail-closed).
			if err := s.guard.CheckNoAI(ctx, providerName, repo.Path, decision.NoAIExempt); err != nil {
				continue
			}
			if discovered >= maxSearchRepos {
				truncated = true
				continue
			}
			out = append(out, commitCandidate{repo: repo.Path, tags: tags})
			discovered++
		}
		if newCandidates > remaining {
			truncated = true
		}
	}
	return out, truncated, nil
}

// parseCommitTime parses a commit time filter: an RFC3339 timestamp (with a
// zone) or a date-only YYYY-MM-DD value interpreted as UTC midnight. It reports
// whether the input was date-only so the caller can extend an inclusive upper
// bound to the end of that day. An empty input yields the zero time.
func parseCommitTime(s string) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("must be RFC3339 or YYYY-MM-DD, got %q", s)
}

// toCommitJSON maps a provider commit to its tool representation, truncating an
// oversized title and message. A commit message without a line break otherwise
// makes Title as long as Message.
func toCommitJSON(providerName, repo string, commit provider.Commit) commitJSON {
	return commitJSON{
		Provider:    providerName,
		Repo:        repo,
		SHA:         commit.SHA,
		ShortSHA:    commit.ShortSHA,
		Title:       truncateText(commit.Title, maxListDescBytes),
		Message:     truncateText(commit.Message, maxListDescBytes),
		Author:      commit.AuthorName,
		AuthoredAt:  formatCommitTime(commit.AuthoredAt),
		CommittedAt: formatCommitTime(commit.CommittedAt),
		WebURL:      commit.WebURL,
	}
}

// formatCommitTime renders a commit timestamp in UTC, or "" for the zero time.
func formatCommitTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// resolveProvider resolves a provider by name without authorization.
func (s *Server) resolveProvider(providerName string) (provider.Provider, error) {
	p, ok := s.registry.Get(providerName)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", providerName)
	}
	return p, nil
}

// tagSetFromMR converts merge request labels into a policy tag set.
func tagSetFromMR(mr provider.MergeRequest) policy.TagSet {
	return policy.TagSet{Known: mr.LabelsKnown, Values: mr.Labels}
}

// fetchTopics fetches a repository's topics and returns a known tag set. A
// fetch failure is reported as an error so the caller can fail closed.
func fetchTopics(ctx context.Context, p provider.Provider, repo string) (policy.TagSet, error) {
	topics, err := p.GetRepositoryTopics(ctx, repo)
	if err != nil {
		return policy.TagSet{}, fmt.Errorf("could not determine topics for repository %q; access denied", repo)
	}
	return policy.TagSet{Known: true, Values: topics}, nil
}

func (s *Server) readFile(ctx context.Context, _ *mcp.CallToolRequest, in readFileInput) (*mcp.CallToolResult, any, error) {
	cleaned, err := validatePath(in.Path)
	if err != nil {
		return nil, nil, err
	}
	repo, err := validateRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	ref, err := validateRef(in.Ref)
	if err != nil {
		return nil, nil, err
	}
	in.Ref = ref
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}
	repo, err = p.CanonicalRepository(repo)
	if err != nil {
		return nil, nil, err
	}
	in.Repo = repo
	if err := s.guard.AuthorizeRepoCapability(in.Provider, in.Repo, policy.CapRepoRead); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	// Topics are needed only when the matched grant has an active tag
	// constraint; a path-only filter does not require a provider call.
	tags := policy.TagSet{}
	if hasTags, err := s.guard.HasTagConstraint(in.Provider, in.Repo, policy.CapRepoRead, cleaned); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	} else if hasTags {
		tags, err = fetchTopics(ctx, p, in.Repo)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := s.guard.AuthorizeResourceRef(ctx, in.Provider, in.Repo, policy.CapRepoRead, tags, cleaned, in.Ref); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	data, err := p.ReadFile(ctx, in.Repo, cleaned, in.Ref)
	if err != nil {
		return nil, nil, mapProviderError(err, "reading repository files needs the Repository: Read permission")
	}
	// Bound the bytes before converting to a string so an oversized file is not
	// copied in full a second time.
	truncated := false
	if len(data) > maxFileBytes {
		data = safeBytePrefix(data, maxFileBytes)
		truncated = true
	}
	content := string(data)
	if truncated {
		content += truncatedMarker
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

// handleGitRemote returns the git proxy clone URL for a repository. It
// authorizes repo:read with the full guard (unknown tags plus the .noai
// overlay) before calling the resolver, mirroring the proxy's own fetch gate:
// a grant with a tag constraint fails closed and a .noai denial stays
// indistinguishable from an unknown repository. The auth mode comes from the
// resolver: "http-basic" when a proxy token is configured, "none" in the
// token-less loopback-only mode. The proxy token is never part of the output;
// resolver details are logged server-side only.
func (s *Server) handleGitRemote(ctx context.Context, _ *mcp.CallToolRequest, in gitRemoteInput) (*mcp.CallToolResult, any, error) {
	repo, err := validateRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.resolveProvider(in.Provider)
	if err != nil {
		return nil, nil, err
	}
	repo, err = p.CanonicalRepository(repo)
	if err != nil {
		return nil, nil, err
	}
	in.Repo = repo
	if err := s.guard.Authorize(ctx, in.Provider, in.Repo, policy.CapRepoRead); err != nil {
		return nil, nil, mapAuthError(err, in.Provider, in.Repo)
	}
	cloneURL, err := s.gitRemote.RemoteURL(in.Provider, in.Repo)
	if err != nil {
		// The resolver's message is a routing/validation detail, never a
		// secret, but it is still only logged, not surfaced.
		s.logger.Error("git remote url lookup failed",
			"provider", in.Provider, "repo", in.Repo, "error", err.Error())
		return nil, nil, errors.New("could not determine the git remote URL")
	}
	auth := gitRemoteAuthJSON{
		Type:     "http-basic",
		Username: "any",
		Note:     "Use the configured git proxy token as the password; it is never returned by this tool.",
	}
	if !s.gitRemote.AuthRequired() {
		auth = gitRemoteAuthJSON{
			Type: "none",
			Note: "No authentication required (loopback only).",
		}
	}
	return jsonResult(gitRemoteOutput{
		Provider:  in.Provider,
		Repo:      in.Repo,
		RemoteURL: cloneURL,
		Auth:      auth,
	})
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
		// Deliberately indistinguishable from an unknown repository: revealing
		// the .noai marker would confirm that the repository exists. The precise
		// reason is logged server-side by the guard.
		return fmt.Errorf("unknown repository %q for provider %q", repo, providerName)
	case errors.Is(err, policy.ErrMarkerCheck):
		status := provider.HTTPStatus(err)
		if errors.Is(err, provider.ErrForbidden) {
			return withStatus(fmt.Sprintf("could not verify the .noai marker for repository %q: forbidden (the token needs repository read access and the project must be in the token scope); access denied", repo), status)
		}
		return withStatus(fmt.Sprintf("could not verify the .noai marker for repository %q; access denied", repo), status)
	default:
		return errors.New("authorization failed")
	}
}

// mapProviderError maps a provider error to a safe message. hint, when
// non-empty, describes the resource permission the failed operation needs and is
// used for forbidden errors; response bodies and tokens are never surfaced.
func mapProviderError(err error, hint string) error {
	return mapProviderErrorState(err, hint, "")
}

// mapProviderErrorState is mapProviderError with an operation-specific message
// for an invalid-state error (HTTP 400/405/409). When invalidStateHint is empty
// a generic message is used, so a non-rebase tool never reports a rebase error.
func mapProviderErrorState(err error, hint, invalidStateHint string) error {
	status := provider.HTTPStatus(err)
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return withStatus("not found", status)
	case errors.Is(err, provider.ErrForbidden):
		if hint != "" {
			return withStatus("forbidden: "+hint, status)
		}
		return withStatus("forbidden: the provider token lacks the required permission for this operation", status)
	case errors.Is(err, provider.ErrInvalidState):
		if invalidStateHint != "" {
			return withStatus(invalidStateHint, status)
		}
		return withStatus("the resource is not in a state that permits this operation", status)
	default:
		return withStatus("provider request failed", status)
	}
}

// withStatus appends the HTTP status to a safe message when one is known.
func withStatus(message string, status int) error {
	if status != 0 {
		return fmt.Errorf("%s (HTTP %d)", message, status)
	}
	return errors.New(message)
}

// validateRepo performs provider-neutral structural validation of a repository
// path. It rejects absolute paths, backslashes, control characters, empty or
// relative segments and encoded traversal, and returns the lexically cleaned
// path. The provider-specific canonical form is applied afterwards by
// Provider.CanonicalRepository; the canonical value must be used
// for both policy matching and the provider call so the two cannot diverge (a
// mismatch would allow a deny rule to be bypassed).
func validateRepo(repo string) (string, error) {
	if repo == "" {
		return "", errors.New("repo must not be empty")
	}
	if path.IsAbs(repo) || strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, "\\") {
		return "", fmt.Errorf("repo %q must be relative", repo)
	}
	if strings.ContainsRune(repo, '\\') {
		return "", fmt.Errorf("repo %q must use forward slashes", repo)
	}
	for _, r := range repo {
		if r == 0 || r < 0x20 || r == 0x7f {
			return "", errors.New("repo contains control characters")
		}
	}
	if strings.Contains(repo, "//") {
		return "", fmt.Errorf("repo %q contains an empty segment", repo)
	}
	for segment := range strings.SplitSeq(repo, "/") {
		if segment == ".." || segment == "." {
			return "", fmt.Errorf("repo %q contains a relative segment", repo)
		}
	}
	cleaned := path.Clean(repo)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("repo %q escapes the namespace", repo)
	}
	lower := strings.ToLower(repo)
	for _, token := range []string{"%2e", "%2f", "%5c"} {
		if strings.Contains(lower, token) {
			return "", fmt.Errorf("repo %q contains an encoded traversal sequence", repo)
		}
	}
	// The canonical provider-specific form is applied by
	// Provider.CanonicalRepository; this function only enforces the
	// provider-neutral, security-critical structure.
	return cleaned, nil
}

// validateRef validates an optional git ref. An empty ref means the default
// branch. It rejects control characters and overlong refs; the ref is otherwise
// passed through to the provider.
func validateRef(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if len(ref) > maxRefBytes {
		return "", fmt.Errorf("ref exceeds the %d byte limit", maxRefBytes)
	}
	for _, r := range ref {
		if r == 0 || r < 0x20 || r == 0x7f {
			return "", errors.New("ref contains control characters")
		}
	}
	return ref, nil
}

// validateNoteBody rejects control characters in a note body, allowing the
// whitespace characters that notes legitimately contain.
func validateNoteBody(body string) error {
	for _, r := range body {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r == 0 || r < 0x20 || r == 0x7f {
			return errors.New("body contains control characters")
		}
	}
	return nil
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

func toMergeRequestJSON(mr provider.MergeRequest, descriptionLimit int) mergeRequestJSON {
	return mergeRequestJSON{
		Number:              mr.Number,
		Title:               mr.Title,
		Description:         truncateText(mr.Description, descriptionLimit),
		State:               mr.State,
		Author:              mr.Author,
		SourceBranch:        mr.SourceBranch,
		TargetBranch:        mr.TargetBranch,
		WebURL:              mr.WebURL,
		RebaseInProgress:    mr.RebaseInProgress,
		MergeError:          truncateText(mr.MergeError, maxTextBytes),
		HasConflicts:        mr.HasConflicts,
		DetailedMergeStatus: mr.DetailedMergeStatus,
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

// safeBytePrefix returns the longest prefix of b that is at most max bytes and
// ends on a UTF-8 rune boundary. Invalid trailing bytes are dropped.
func safeBytePrefix(b []byte, max int) []byte {
	if max <= 0 {
		return nil
	}
	if max >= len(b) {
		return b
	}
	p := b[:max]
	for len(p) > 0 {
		r, size := utf8.DecodeLastRune(p)
		if r != utf8.RuneError || size > 1 {
			break
		}
		p = p[:len(p)-size]
	}
	return p
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
