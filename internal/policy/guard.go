package policy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
)

// Sentinel errors returned (wrapped) by Guard.Authorize. Callers should use
// errors.Is to distinguish the failure modes.
var (
	// ErrUnknownProvider reports that no policy exists for the provider.
	ErrUnknownProvider = errors.New("unknown provider")
	// ErrUnknownRepository reports that no rule matched the repository.
	ErrUnknownRepository = errors.New("unknown repository")
	// ErrDenied reports that a matching rule denied the capability.
	ErrDenied = errors.New("access denied")
	// ErrNoAI reports that the repository carries the .noai marker.
	ErrNoAI = errors.New(".noai marker present")
	// ErrMarkerCheck reports that the .noai marker could not be checked.
	ErrMarkerCheck = errors.New("marker check failed")
)

// FileChecker checks for the existence of a file in a repository. It is the
// minimal interface required by the guard; providers implement it without the
// policy package importing the provider package.
type FileChecker interface {
	// FileExists reports whether path exists in repo at ref. An empty ref means
	// the repository's default branch.
	FileExists(ctx context.Context, repo, path, ref string) (bool, error)
}

// Guard authorizes capability requests against per-provider policies and the
// repository's .noai marker. Authorization is deny-by-default and the marker
// check is fail-closed.
type Guard struct {
	policies   map[string]*Policy
	checkers   map[string]FileChecker
	markerFile string
	logger     *slog.Logger
}

// NewGuard constructs a Guard. policies and checkers are keyed by provider
// name. A nil logger discards logs.
func NewGuard(policies map[string]*Policy, checkers map[string]FileChecker, markerFile string, logger *slog.Logger) *Guard {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Guard{
		policies:   policies,
		checkers:   checkers,
		markerFile: markerFile,
		logger:     logger,
	}
}

// Authorize checks whether capability c may be used on repo at providerName
// with unknown tags. It is equivalent to AuthorizeWithTags(..., TagSet{}).
func (g *Guard) Authorize(ctx context.Context, providerName, repo string, c Capability) error {
	return g.AuthorizeWithTags(ctx, providerName, repo, c, TagSet{})
}

// AuthorizeWithTags checks whether capability c may be used on repo at
// providerName with the observed tags. It evaluates the provider's policy
// first, then performs the .noai marker check. Every decision is logged; tokens
// and request bodies are never logged.
func (g *Guard) AuthorizeWithTags(ctx context.Context, providerName, repo string, c Capability, tags TagSet) error {
	p, ok := g.policies[providerName]
	if !ok {
		g.logDecision(providerName, repo, c, "deny", "unknown provider")
		return fmt.Errorf("%w: %s", ErrUnknownProvider, providerName)
	}

	decision := p.EvaluateWithTags(repo, c, tags)
	g.logDecision(providerName, repo, c, decisionWord(decision.Allowed), decision.Reason)

	if !decision.Matched {
		return fmt.Errorf("%w: %s", ErrUnknownRepository, repo)
	}
	if !decision.Allowed {
		return fmt.Errorf("%w: %s", ErrDenied, decision.Reason)
	}

	return g.checkMarker(ctx, providerName, repo)
}

// AuthorizeRepoCapability is a policy-only pre-check that ignores tag filters
// and the .noai marker. It succeeds when the first matching rule allows the
// capability (regardless of any active tag filter) and is used before a
// privileged metadata fetch whose result is needed to evaluate the tags.
func (g *Guard) AuthorizeRepoCapability(ctx context.Context, providerName, repo string, c Capability) error {
	p, ok := g.policies[providerName]
	if !ok {
		g.logDecision(providerName, repo, c, "deny", "unknown provider")
		return fmt.Errorf("%w: %s", ErrUnknownProvider, providerName)
	}
	decision := p.Evaluate(repo, c)
	g.logDecision(providerName, repo, c, decisionWord(decision.Allowed), decision.Reason)
	if !decision.Matched {
		return fmt.Errorf("%w: %s", ErrUnknownRepository, repo)
	}
	if !decision.CapabilityGranted {
		return fmt.Errorf("%w: %s", ErrDenied, decision.Reason)
	}
	return nil
}

func (g *Guard) checkMarker(ctx context.Context, providerName, repo string) error {
	checker, ok := g.checkers[providerName]
	if !ok || checker == nil {
		g.logger.Info("marker check", "provider", providerName, "repo", repo, "result", "error")
		return fmt.Errorf("%w: no file checker for provider %s", ErrMarkerCheck, providerName)
	}

	exists, err := checker.FileExists(ctx, repo, g.markerFile, "")
	if err != nil {
		g.logger.Info("marker check", "provider", providerName, "repo", repo, "marker", g.markerFile, "result", "error")
		return fmt.Errorf("%w: %s", ErrMarkerCheck, repo)
	}
	g.logger.Info("marker check", "provider", providerName, "repo", repo, "marker", g.markerFile, "exists", exists)
	if exists {
		return fmt.Errorf("%w: %s", ErrNoAI, repo)
	}
	return nil
}

// AuthorizeList checks whether providerName may list repositories at all. It
// succeeds when the provider is known and at least one allow rule grants
// CapRepoList. It does not perform a .noai marker check: listing filters each
// candidate through Authorize instead.
func (g *Guard) AuthorizeList(providerName string) error {
	p, ok := g.policies[providerName]
	if !ok {
		g.logDecision(providerName, "", CapRepoList, "deny", "unknown provider")
		return fmt.Errorf("%w: %s", ErrUnknownProvider, providerName)
	}
	if !p.GrantsAnywhere(CapRepoList) {
		g.logDecision(providerName, "", CapRepoList, "deny", "repo:list not granted")
		return fmt.Errorf("%w: repo:list not granted", ErrDenied)
	}
	g.logDecision(providerName, "", CapRepoList, "allow", "repo:list granted")
	return nil
}

// StaticRepositories returns the concrete repository paths configured for
// providerName whose first matching rule allows them. It performs no marker
// check and requires no capability.
func (g *Guard) StaticRepositories(providerName string) ([]string, error) {
	p, ok := g.policies[providerName]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, providerName)
	}
	return p.StaticRepositories(), nil
}

// Evaluate performs a policy-only evaluation for providerName, repo and
// capability. It performs no .noai marker check. It returns ErrUnknownProvider
// when the provider has no policy. Every decision is logged.
func (g *Guard) Evaluate(providerName, repo string, c Capability) (Decision, error) {
	p, ok := g.policies[providerName]
	if !ok {
		g.logDecision(providerName, repo, c, "deny", "unknown provider")
		return Decision{}, fmt.Errorf("%w: %s", ErrUnknownProvider, providerName)
	}
	decision := p.Evaluate(repo, c)
	g.logDecision(providerName, repo, c, decisionWord(decision.Allowed), decision.Reason)
	return decision, nil
}

// ConfiguredRule describes a rule as it appears in the configuration. It is
// used by list_configured_rules and deliberately exposes only configured
// capabilities, never effective access.
type ConfiguredRule struct {
	// Provider is the logical provider name.
	Provider string
	// Repositories are the configured glob patterns.
	Repositories []string
	// Effect is the rule outcome.
	Effect Effect
	// Capabilities are the configured capabilities with their filters.
	Capabilities []CapabilityGrant
}

// ConfiguredRules returns every configured rule, sorted by provider name for
// stable output. Rule order within a provider is preserved.
func (g *Guard) ConfiguredRules() []ConfiguredRule {
	var out []ConfiguredRule
	for name, p := range g.policies {
		for _, rule := range p.Rules() {
			out = append(out, ConfiguredRule{
				Provider:     name,
				Repositories: rule.Repositories,
				Effect:       rule.Effect,
				Capabilities: rule.SortedCapabilities(),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

func (g *Guard) logDecision(providerName, repo string, c Capability, decision, reason string) {
	g.logger.Info("authorization decision",
		"provider", providerName,
		"repo", repo,
		"capability", string(c),
		"decision", decision,
		"reason", reason,
	)
}

func decisionWord(allowed bool) string {
	if allowed {
		return "allow"
	}
	return "deny"
}
