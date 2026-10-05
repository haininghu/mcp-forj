package policy

import (
	"fmt"
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

// TagFilter is a set of required and excluded tags (GitLab labels). Both lists
// are matched by exact, case-sensitive equality.
type TagFilter struct {
	// Require lists tags the subject must all carry.
	Require []string
	// Exclude lists tags the subject must not carry.
	Exclude []string
}

// IsZero reports whether the filter imposes no tag requirements.
func (f TagFilter) IsZero() bool {
	return len(f.Require) == 0 && len(f.Exclude) == 0
}

// TagSet is an observed set of tags. Known=false means the tags could not be
// determined, in which case an active filter fails closed.
type TagSet struct {
	// Known reports whether Values is authoritative.
	Known bool
	// Values are the observed tags.
	Values []string
}

// CapabilityGrant is a capability together with its optional tag filter.
type CapabilityGrant struct {
	// Name is the capability.
	Name Capability
	// Filter is the optional tag filter.
	Filter TagFilter
}

// RuleSpec is the configuration-facing description of a rule. It is decoupled
// from config so that policy does not import config.
type RuleSpec struct {
	// Repositories are doublestar glob patterns matched against the canonical
	// namespace/project path.
	Repositories []string
	// Effect is either "allow" or "deny".
	Effect string
	// Capabilities are the capabilities granted by an "allow" rule.
	Capabilities []CapabilityGrant
}

// Rule is a validated, compiled rule.
type Rule struct {
	// Repositories are the glob patterns the rule matches.
	Repositories []string
	// Effect is the rule outcome.
	Effect Effect
	// Capabilities maps each granted capability to its tag filter (only
	// meaningful for EffectAllow).
	Capabilities map[Capability]TagFilter
}

// Policy is an ordered set of rules. Rules are evaluated first-match-wins.
type Policy struct {
	rules []Rule
}

// Decision is the result of evaluating a policy for one repository and
// capability.
type Decision struct {
	// Allowed reports whether the capability may be used, including any tag
	// filter.
	Allowed bool
	// CapabilityGranted reports whether the matched allow rule grants the
	// capability, ignoring tag filters.
	CapabilityGranted bool
	// Matched reports whether any rule matched the repository.
	Matched bool
	// Reason is a short human-readable explanation of the decision.
	Reason string
}

// Build compiles and validates rule specifications. It rejects unknown effects,
// empty repository lists, invalid glob patterns and unknown capabilities.
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
		caps := make(map[Capability]TagFilter, len(spec.Capabilities))
		for _, grant := range spec.Capabilities {
			if !IsKnownCapability(string(grant.Name)) {
				return nil, fmt.Errorf("policy: rule %d: unknown capability %q", i, grant.Name)
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

// Evaluate returns the decision for repo and capability c with unknown tags.
// It is equivalent to EvaluateWithTags(repo, c, TagSet{}).
func (p *Policy) Evaluate(repo string, c Capability) Decision {
	return p.EvaluateWithTags(repo, c, TagSet{})
}

// EvaluateWithTags returns the decision for repo, capability c and the observed
// tags. The first rule whose pattern matches repo wins. An active tag filter
// fails closed when the tags are not known. When no rule matches, access is
// denied.
func (p *Policy) EvaluateWithTags(repo string, c Capability, tags TagSet) Decision {
	for _, rule := range p.rules {
		if !ruleMatches(rule, repo) {
			continue
		}
		if rule.Effect == EffectDeny {
			return Decision{Matched: true, Reason: "denied by rule"}
		}
		filter, granted := rule.Capabilities[c]
		if !granted {
			return Decision{
				Matched: true,
				Reason:  fmt.Sprintf("capability %s not granted", c),
			}
		}
		if filter.IsZero() {
			return Decision{Allowed: true, CapabilityGranted: true, Matched: true, Reason: "capability granted"}
		}
		if !tags.Known {
			return Decision{CapabilityGranted: true, Matched: true, Reason: "tag information unavailable"}
		}
		for _, required := range filter.Require {
			if !containsTag(tags.Values, required) {
				return Decision{CapabilityGranted: true, Matched: true, Reason: "tag requirement not met"}
			}
		}
		for _, excluded := range filter.Exclude {
			if containsTag(tags.Values, excluded) {
				return Decision{CapabilityGranted: true, Matched: true, Reason: "excluded tag present"}
			}
		}
		return Decision{Allowed: true, CapabilityGranted: true, Matched: true, Reason: "capability granted"}
	}
	return Decision{Matched: false, Reason: "no matching rule"}
}

func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Classify returns the effect of the first rule matching repo. matched is false
// when no rule matches.
func (p *Policy) Classify(repo string) (matched bool, effect Effect) {
	for _, rule := range p.rules {
		if ruleMatches(rule, repo) {
			return true, rule.Effect
		}
	}
	return false, ""
}

// StaticRepositories returns the concrete repository paths listed directly in
// the configuration whose first matching rule allows them. Literal patterns are
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
			if matched, effect := p.Classify(pattern); matched && effect == EffectAllow {
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

// HasTagFilter reports whether the first matching rule is an allow rule that
// grants capability c with a non-zero tag filter. It is false for a deny rule,
// no matching rule, or a missing/unfiltered capability. It is used to decide
// whether tag information must be fetched before evaluating the capability.
func (p *Policy) HasTagFilter(repo string, c Capability) bool {
	for _, rule := range p.rules {
		if !ruleMatches(rule, repo) {
			continue
		}
		if rule.Effect != EffectAllow {
			return false
		}
		filter, ok := rule.Capabilities[c]
		if !ok {
			return false
		}
		return !filter.IsZero()
	}
	return false
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
		caps := make(map[Capability]TagFilter, len(rule.Capabilities))
		for c, filter := range rule.Capabilities {
			caps[c] = cloneTagFilter(filter)
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
		grants = append(grants, CapabilityGrant{Name: c, Filter: cloneTagFilter(filter)})
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].Name < grants[j].Name })
	return grants
}

func cloneTagFilter(f TagFilter) TagFilter {
	return TagFilter{
		Require: append([]string(nil), f.Require...),
		Exclude: append([]string(nil), f.Exclude...),
	}
}

func ruleMatches(rule Rule, repo string) bool {
	for _, pattern := range rule.Repositories {
		ok, err := doublestar.Match(pattern, repo)
		if err != nil {
			continue
		}
		if ok {
			return true
		}
	}
	return false
}
