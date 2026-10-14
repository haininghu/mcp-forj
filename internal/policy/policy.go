package policy

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Effect is the outcome of a matching rule.
type Effect string

// Rule effects. A rule either grants the listed capabilities or denies access
// outright.
const (
	// EffectAllow grants only the capabilities listed on the rule.
	EffectAllow Effect = "allow"
	// EffectDeny denies access regardless of any capability.
	EffectDeny Effect = "deny"
)

// PathFilter constrains one doublestar-matched string dimension, such as a
// repository-relative file path or a push target branch name. An empty Include
// allows everything of that dimension (subject to Exclude); Include wins
// nothing — Exclude always wins over Include.
type PathFilter struct {
	// Include lists globs; when non-empty the value must match one.
	Include []string
	// Exclude lists globs; the value must match none (exclude wins).
	Exclude []string
}

// IsZero reports whether the path filter imposes no constraints.
func (f PathFilter) IsZero() bool {
	return len(f.Include) == 0 && len(f.Exclude) == 0
}

// CapabilityFilter constrains a granted capability. Tags (Require/Exclude) are
// matched by exact, case-sensitive equality; path globs (Paths) are matched
// against a repository-relative file path with doublestar; branch globs
// (Branches) are matched against a push target branch name with doublestar and
// are valid on repo:write only. An empty list imposes no constraint of that
// kind.
type CapabilityFilter struct {
	// Require lists tags the subject must all carry.
	Require []string
	// Exclude lists tags the subject must not carry.
	Exclude []string
	// Paths constrains the repository-relative file path.
	Paths PathFilter
	// Branches constrains the push target branch (repo:write only). A branch
	// evaluation without an active branch filter denies (fail-closed).
	Branches PathFilter
	// NoAIExempt exempts the capability from the .noai default-deny overlay. It
	// is an override, not a constraint: IsZero ignores it.
	NoAIExempt bool
}

// IsZero reports whether the filter imposes no constraints. NoAIExempt is an
// override and is not a constraint, so it does not affect IsZero.
func (f CapabilityFilter) IsZero() bool {
	return len(f.Require) == 0 && len(f.Exclude) == 0 && f.Paths.IsZero() && f.Branches.IsZero()
}

// TagSet is an observed set of tags. Known=false means the tags could not be
// determined, in which case an active tag filter fails closed.
type TagSet struct {
	// Known reports whether Values is authoritative.
	Known bool
	// Values are the observed tags.
	Values []string
}

// CapabilityGrant is a capability together with its optional filter.
type CapabilityGrant struct {
	// Name is the capability.
	Name Capability
	// Filter is the optional capability filter.
	Filter CapabilityFilter
}

// RuleSpec is the configuration-facing description of a rule. It is decoupled
// from config so that policy does not import config.
type RuleSpec struct {
	// Repositories are doublestar glob patterns matched against the canonical
	// namespace/project path.
	Repositories []string
	// Effect is either "allow" or "deny".
	Effect string
	// Capabilities are the rule's capability entries (granted by an "allow"
	// rule, denied by a "deny" rule), each with an optional filter. A "deny"
	// rule without capabilities denies every capability in its scope.
	Capabilities []CapabilityGrant
}

// Rule is a validated, compiled rule.
type Rule struct {
	// Repositories are the glob patterns the rule matches.
	Repositories []string
	// Effect is the rule outcome.
	Effect Effect
	// Capabilities maps each capability entry to its filter, for either effect.
	// On a deny rule an empty map means "deny every capability in scope".
	Capabilities map[Capability]CapabilityFilter
}

// Policy is an ordered set of rules. Rules are evaluated first-match-wins.
type Policy struct {
	rules []Rule
}

// Decision is the result of evaluating a policy for one repository and
// capability.
type Decision struct {
	// Allowed reports whether the capability may be used, including every
	// applicable filter.
	Allowed bool
	// Matched reports whether any rule's repository scope matched, even if no
	// rule applied (fall-through). It distinguishes "unknown repository" from
	// "denied" for existence hygiene; the guard maps the two to different
	// errors.
	Matched bool
	// NoAIExempt reports whether the applicable grant exempts the capability
	// from the .noai default-deny overlay. It is only meaningful when Allowed
	// is true.
	NoAIExempt bool
	// Reason is a short human-readable explanation of the decision.
	Reason string
}

// Build compiles and validates rule specifications. It rejects unknown effects,
// empty repository lists, invalid glob patterns and unknown capabilities. A
// deny rule may list capabilities and filters; without capabilities it denies
// every capability in its scope (firewall-style rules, ADR 0012).
func Build(specs []RuleSpec) (*Policy, error) {
	rules := make([]Rule, 0, len(specs))
	for i, spec := range specs {
		effect := Effect(spec.Effect)
		if effect != EffectAllow && effect != EffectDeny {
			return nil, fmt.Errorf("policy: rule %d: unknown effect %q", i, spec.Effect)
		}
		if len(spec.Repositories) == 0 {
			return nil, fmt.Errorf("policy: rule %d: no repositories", i)
		}
		repos := append([]string(nil), spec.Repositories...)
		for _, pattern := range repos {
			if pattern == "" {
				return nil, fmt.Errorf("policy: rule %d: empty repository pattern", i)
			}
			if !doublestar.ValidatePattern(pattern) {
				return nil, fmt.Errorf("policy: rule %d: invalid pattern %q", i, pattern)
			}
		}
		caps := make(map[Capability]CapabilityFilter, len(spec.Capabilities))
		for _, grant := range spec.Capabilities {
			if !IsKnownCapability(string(grant.Name)) {
				return nil, fmt.Errorf("policy: rule %d: unknown capability %q", i, grant.Name)
			}
			// A deny rule refuses regardless of the marker, so a noai exemption
			// on it is meaningless; reject it to avoid a false expectation.
			if effect == EffectDeny && grant.Filter.NoAIExempt {
				return nil, fmt.Errorf("policy: rule %d: noai is not allowed on a deny rule", i)
			}
			if err := validatePathPatterns(grant.Filter.Paths.Include); err != nil {
				return nil, fmt.Errorf("policy: rule %d: capability %q paths.include: %w", i, grant.Name, err)
			}
			if err := validatePathPatterns(grant.Filter.Paths.Exclude); err != nil {
				return nil, fmt.Errorf("policy: rule %d: capability %q paths.exclude: %w", i, grant.Name, err)
			}
			if err := validatePathPatterns(grant.Filter.Branches.Include); err != nil {
				return nil, fmt.Errorf("policy: rule %d: capability %q branches.include: %w", i, grant.Name, err)
			}
			if err := validatePathPatterns(grant.Filter.Branches.Exclude); err != nil {
				return nil, fmt.Errorf("policy: rule %d: capability %q branches.exclude: %w", i, grant.Name, err)
			}
			caps[grant.Name] = grant.Filter
		}
		rules = append(rules, Rule{
			Repositories: repos,
			Effect:       effect,
			Capabilities: caps,
		})
	}
	return &Policy{rules: rules}, nil
}

// Evaluate returns the decision for repo and capability c with unknown tags and
// no path. It is equivalent to EvaluateResource(repo, c, TagSet{}, "").
func (p *Policy) Evaluate(repo string, c Capability) Decision {
	return p.EvaluateResource(repo, c, TagSet{}, "")
}

// EvaluateWithTags returns the decision for repo, capability c and the observed
// tags, with no path. It is equivalent to EvaluateResource(repo, c, tags, "").
func (p *Policy) EvaluateWithTags(repo string, c Capability, tags TagSet) Decision {
	return p.EvaluateResource(repo, c, tags, "")
}

// EvaluateResource returns the decision for repo, capability c, the observed
// tags and the repository-relative path. It follows the firewall-style rule
// model of ADR 0012: a rule applies when its repository scope matches AND (it
// is a deny rule without capabilities, or a capability entry matches c and all
// of that entry's filters pass). The first applicable rule wins. A filter that
// is evaluated and does not pass means the rule does not apply (fall-through);
// a filter whose input is unavailable fails closed (a hard deny). When no rule
// applies, access is denied. This entry point has no branch context, so a
// branch filter is not applied; use EvaluateResourceBranch to authorize an
// operation that names a branch.
func (p *Policy) EvaluateResource(repo string, c Capability, tags TagSet, path string) Decision {
	return p.evaluateResource(repo, c, tags, path, false, "")
}

// EvaluateResourceBranch is EvaluateResource extended with a branch dimension.
// A non-empty branch requires a matching branch filter (a grant without one
// fails closed, so an unfiltered repo:write grant never allows a push); an
// empty branch against an active branch filter fails closed ("branch
// required"). Tags, path and branch must all pass, checked in that order: tags
// (fail closed when unknown) then path (fail closed when empty) then branch. A
// branch filter that is evaluated and does not match makes the rule fall
// through, while the branch filter's exclude list wins over its include list.
func (p *Policy) EvaluateResourceBranch(repo string, c Capability, tags TagSet, path, branch string) Decision {
	return p.evaluateResource(repo, c, tags, path, true, branch)
}

// evaluateResource is the shared first-match-wins evaluation. When withBranch
// is false the caller has no branch context and the branch dimension imposes
// nothing; when true the branch rules apply to the given branch (which may be
// empty).
//
// The boundary between fall-through and fail-closed is deliberate: a filter
// that is evaluated against known input and does not match makes the rule not
// apply, but a filter whose input is unavailable (unknown tags, an empty path
// for an active path filter, or a branch dimension that cannot be resolved)
// denies outright, so fail-closed is never defeated by a later, broader rule.
func (p *Policy) evaluateResource(repo string, c Capability, tags TagSet, path string, withBranch bool, branch string) Decision {
	matchedScope := false
	reason := ""
	for _, rule := range p.rules {
		if !ruleMatches(rule, repo) {
			continue
		}
		matchedScope = true

		// A deny rule without capabilities denies every capability in its scope.
		if rule.Effect == EffectDeny && len(rule.Capabilities) == 0 {
			return Decision{Matched: true, Reason: "denied by rule"}
		}

		filter, listed := rule.Capabilities[c]
		if !listed {
			reason = moreRelevantReason(reason, fmt.Sprintf("capability %s not granted", c))
			continue
		}
		exempt := filter.NoAIExempt

		// Tag dimension. Unavailable tag information fails closed; an evaluated
		// miss makes the rule fall through.
		if len(filter.Require) > 0 || len(filter.Exclude) > 0 {
			if !tags.Known {
				return Decision{Matched: true, Reason: "tag information unavailable"}
			}
			if miss := tagMiss(filter, tags.Values); miss != "" {
				reason = moreRelevantReason(reason, miss)
				continue
			}
		}

		// Path dimension. An active path filter with no path fails closed.
		if !filter.Paths.IsZero() {
			if path == "" {
				return Decision{Matched: true, Reason: "path required"}
			}
			if miss := globMiss(filter.Paths, path, "path excluded", "path not allowed"); miss != "" {
				reason = moreRelevantReason(reason, miss)
				continue
			}
		}

		// Branch dimension (only when the caller has a branch context). A named
		// branch without a branch filter, and an active branch filter without a
		// branch, both fail closed; an evaluated branch miss falls through.
		if withBranch {
			switch {
			case filter.Branches.IsZero():
				if branch != "" {
					return Decision{Matched: true, Reason: "branch filter required"}
				}
			case branch == "":
				return Decision{Matched: true, Reason: "branch required"}
			default:
				if miss := globMiss(filter.Branches, branch, "branch excluded", "branch not allowed"); miss != "" {
					reason = moreRelevantReason(reason, miss)
					continue
				}
			}
		}

		if rule.Effect == EffectDeny {
			return Decision{Matched: true, Reason: "denied by rule"}
		}
		return Decision{Allowed: true, Matched: true, NoAIExempt: exempt, Reason: "capability granted"}
	}
	if !matchedScope {
		return Decision{Matched: false, Reason: "no matching rule"}
	}
	if reason == "" {
		reason = "no applicable rule"
	}
	return Decision{Matched: true, Reason: reason}
}

// EvaluatePreCheck is a policy-only pre-check that ignores every capability
// filter (tags, paths, branches) and the .noai marker. It reports granted when
// some allow rule could grant c and matched when any rule's scope matched repo.
// It denies early only when a rule refuses c unconditionally: a deny rule
// without capabilities, or a deny rule that lists c with no filter. A filtered
// deny is not conclusive here, so a later allow rule may still grant; the
// caller must fetch the metadata and evaluate the filters with
// EvaluateResource. It is used before a privileged metadata fetch whose result
// is needed to evaluate a tag filter.
func (p *Policy) EvaluatePreCheck(repo string, c Capability) (granted, matched bool) {
	for _, rule := range p.rules {
		if !ruleMatches(rule, repo) {
			continue
		}
		matched = true
		if rule.Effect == EffectDeny {
			if len(rule.Capabilities) == 0 {
				return false, true
			}
			if filter, listed := rule.Capabilities[c]; listed && filter.IsZero() {
				return false, true
			}
			continue
		}
		if _, listed := rule.Capabilities[c]; listed {
			return true, true
		}
	}
	return false, matched
}

// tagMiss reports why the observed tags fail filter, or "" when they pass.
func tagMiss(filter CapabilityFilter, values []string) string {
	for _, required := range filter.Require {
		if !slices.Contains(values, required) {
			return "tag requirement not met"
		}
	}
	for _, excluded := range filter.Exclude {
		if slices.Contains(values, excluded) {
			return "excluded tag present"
		}
	}
	return ""
}

// globMiss reports why value fails filter, or "" when it passes. Exclude wins
// over include. The caller supplies the reason strings for the two miss modes
// (path vs branch).
func globMiss(filter PathFilter, value, excludedReason, notAllowedReason string) string {
	if matchesAnyGlob(filter.Exclude, value) {
		return excludedReason
	}
	if len(filter.Include) > 0 && !matchesAnyGlob(filter.Include, value) {
		return notAllowedReason
	}
	return ""
}

// globPass reports whether value passes filter: it must match none of Exclude
// and, when Include is non-empty, at least one of Include.
func globPass(filter PathFilter, value string) bool {
	if matchesAnyGlob(filter.Exclude, value) {
		return false
	}
	return len(filter.Include) == 0 || matchesAnyGlob(filter.Include, value)
}

// moreRelevantReason keeps the more specific of two fall-through reasons. A
// filter miss explains an attempted match and outranks a bare capability miss,
// which outranks the default reason.
func moreRelevantReason(current, candidate string) string {
	if reasonRank(candidate) > reasonRank(current) {
		return candidate
	}
	return current
}

func reasonRank(reason string) int {
	switch {
	case reason == "":
		return 0
	case strings.HasPrefix(reason, "capability "):
		return 1
	default:
		return 2
	}
}

func matchesAnyGlob(patterns []string, value string) bool {
	for _, pattern := range patterns {
		ok, err := doublestar.Match(pattern, value)
		if err != nil {
			// Patterns are validated at config load; treat an unexpected match
			// error as a match so exclude patterns stay fail-closed.
			return true
		}
		if ok {
			return true
		}
	}
	return false
}

func validatePathPatterns(patterns []string) error {
	for _, pattern := range patterns {
		if pattern == "" {
			return fmt.Errorf("empty pattern")
		}
		if !doublestar.ValidatePattern(pattern) {
			return fmt.Errorf("invalid pattern %q", pattern)
		}
	}
	return nil
}

// listable reports whether a literal repository is visible in
// list_repositories. A repository is hidden when the first scope-matching rule
// denies the listing: a deny rule without capabilities, or a deny rule that
// lists repo:list (fail-closed). A deny rule that does not list repo:list does
// not hide the repository, and any allow rule exposes it. Explicitly configured
// repositories are listed even without the repo:list capability.
func (p *Policy) listable(repo string) bool {
	for _, rule := range p.rules {
		if !ruleMatches(rule, repo) {
			continue
		}
		if rule.Effect == EffectDeny {
			if len(rule.Capabilities) == 0 {
				return false
			}
			if _, ok := rule.Capabilities[CapRepoList]; ok {
				return false
			}
			continue
		}
		return true
	}
	return false
}

// StaticRepositories returns the concrete repository paths listed directly in
// the configuration that are visible to list_repositories. Literal patterns are
// collected in first-appearance order and deduplicated.
func (p *Policy) StaticRepositories() []string {
	seen := make(map[string]bool)
	var out []string
	for _, rule := range p.rules {
		for _, pattern := range rule.Repositories {
			if !isLiteralPattern(pattern) || seen[pattern] {
				continue
			}
			seen[pattern] = true
			if p.listable(pattern) {
				out = append(out, pattern)
			}
		}
	}
	return out
}

// isLiteralPattern reports whether p is a concrete repository path rather than
// a glob pattern. A literal contains none of the glob metacharacters *, ?, [,
// { or \.
func isLiteralPattern(p string) bool {
	return !strings.ContainsAny(p, `*?[{\`)
}

// GrantsAnywhere reports whether any allow rule grants capability c, regardless
// of repository. It is used to decide whether a provider is eligible for
// repository listing.
func (p *Policy) GrantsAnywhere(c Capability) bool {
	for _, rule := range p.rules {
		if rule.Effect != EffectAllow {
			continue
		}
		if _, ok := rule.Capabilities[c]; ok {
			return true
		}
	}
	return false
}

// HasFilter reports whether a reachable rule carries an active filter (tags,
// paths or branches) for capability c. It is used by repository listing to
// decide whether topics must be fetched. It stops at a scope-only deny and at a
// rule that decides without any filter (an unconditional allow or deny), so it
// never fetches for a capability that is already decided.
func (p *Policy) HasFilter(repo string, c Capability) bool {
	for _, rule := range p.rules {
		if !ruleMatches(rule, repo) {
			continue
		}
		if rule.Effect == EffectDeny && len(rule.Capabilities) == 0 {
			return false
		}
		filter, listed := rule.Capabilities[c]
		if !listed {
			continue
		}
		if !filter.IsZero() {
			return true
		}
		// The rule applies (or denies) without any filter, so no later rule can
		// change the outcome.
		return false
	}
	return false
}

// HasTagConstraint reports whether tag information is required to evaluate
// capability c for repo at path. It walks the rules in order. A reachable rule
// with an active tag constraint always needs tag information, because the tag
// dimension is evaluated before the path dimension (an unknown tag set fails
// closed there). A rule without a tag constraint decides without tags when it
// has no path filter or the path matches; otherwise it falls through and the
// search continues. It decides whether metadata must be fetched; a path- or
// branch-only filter does not require it.
func (p *Policy) HasTagConstraint(repo string, c Capability, path string) bool {
	for _, rule := range p.rules {
		if !ruleMatches(rule, repo) {
			continue
		}
		if rule.Effect == EffectDeny && len(rule.Capabilities) == 0 {
			return false
		}
		filter, listed := rule.Capabilities[c]
		if !listed {
			continue
		}
		if len(filter.Require) > 0 || len(filter.Exclude) > 0 {
			return true
		}
		if filter.Paths.IsZero() || (path != "" && globPass(filter.Paths, path)) {
			return false
		}
	}
	return false
}

// NoAIExemptAll reports whether every allow rule that lists capability c and
// whose scope matches repo exempts it from the .noai overlay, and at least one
// such rule exists. It is conservative: a caller that must decide the overlay
// once per repository (list_merge_requests) uses it so a per-item fall-through
// to a non-exempt grant can never skip the marker check.
func (p *Policy) NoAIExemptAll(repo string, c Capability) bool {
	found := false
	for _, rule := range p.rules {
		if rule.Effect != EffectAllow || !ruleMatches(rule, repo) {
			continue
		}
		filter, listed := rule.Capabilities[c]
		if !listed {
			continue
		}
		found = true
		if !filter.NoAIExempt {
			return false
		}
	}
	return found
}

// ListSearchPrefixes returns the literal project search prefixes derived from
// every allow rule that grants capability c. A prefix is the part of a
// repository pattern before its first glob metacharacter, with any trailing
// slash trimmed; patterns that begin with a metacharacter (e.g. "**") yield no
// prefix. Empty prefixes are skipped and the result is deduplicated and sorted.
func (p *Policy) ListSearchPrefixes(c Capability) []string {
	seen := make(map[string]bool)
	var out []string
	for _, rule := range p.rules {
		if rule.Effect != EffectAllow {
			continue
		}
		if _, ok := rule.Capabilities[c]; !ok {
			continue
		}
		for _, pattern := range rule.Repositories {
			prefix := literalPrefix(pattern)
			if prefix == "" || seen[prefix] {
				continue
			}
			seen[prefix] = true
			out = append(out, prefix)
		}
	}
	sort.Strings(out)
	return out
}

// literalPrefix returns the substring of pattern before its first glob
// metacharacter, with any trailing slash trimmed.
func literalPrefix(pattern string) string {
	if i := strings.IndexAny(pattern, `*?[{\`); i >= 0 {
		pattern = pattern[:i]
	}
	return strings.TrimSuffix(pattern, "/")
}

// Rules returns a deep copy of the compiled rules for introspection.
func (p *Policy) Rules() []Rule {
	out := make([]Rule, len(p.rules))
	for i, rule := range p.rules {
		caps := make(map[Capability]CapabilityFilter, len(rule.Capabilities))
		for c, filter := range rule.Capabilities {
			caps[c] = cloneFilter(filter)
		}
		out[i] = Rule{
			Repositories: append([]string(nil), rule.Repositories...),
			Effect:       rule.Effect,
			Capabilities: caps,
		}
	}
	return out
}

// SortedCapabilities returns the rule's granted capabilities with their filters
// in stable name order.
func (r Rule) SortedCapabilities() []CapabilityGrant {
	grants := make([]CapabilityGrant, 0, len(r.Capabilities))
	for c, filter := range r.Capabilities {
		grants = append(grants, CapabilityGrant{Name: c, Filter: cloneFilter(filter)})
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].Name < grants[j].Name })
	return grants
}

func cloneFilter(f CapabilityFilter) CapabilityFilter {
	return CapabilityFilter{
		Require: append([]string(nil), f.Require...),
		Exclude: append([]string(nil), f.Exclude...),
		Paths: PathFilter{
			Include: append([]string(nil), f.Paths.Include...),
			Exclude: append([]string(nil), f.Paths.Exclude...),
		},
		Branches: PathFilter{
			Include: append([]string(nil), f.Branches.Include...),
			Exclude: append([]string(nil), f.Branches.Exclude...),
		},
		NoAIExempt: f.NoAIExempt,
	}
}

func ruleMatches(rule Rule, repo string) bool {
	for _, pattern := range rule.Repositories {
		ok, err := doublestar.Match(pattern, repo)
		if err != nil {
			// Patterns are validated at build time; treat an unexpected match
			// error as a match so deny rules stay fail-closed.
			return true
		}
		if ok {
			return true
		}
	}
	return false
}
