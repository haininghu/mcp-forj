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

// RuleSpec is the configuration-facing description of a rule. It is decoupled
// from config so that policy does not import config.
type RuleSpec struct {
	// Repositories are doublestar glob patterns matched against the canonical
	// namespace/project path.
	Repositories []string
	// Effect is either "allow" or "deny".
	Effect string
	// Capabilities are the capabilities granted by an "allow" rule.
	Capabilities []string
}

// Rule is a validated, compiled rule.
type Rule struct {
	// Repositories are the glob patterns the rule matches.
	Repositories []string
	// Effect is the rule outcome.
	Effect Effect
	// Capabilities is the set of granted capabilities (only meaningful for
	// EffectAllow).
	Capabilities map[Capability]bool
}

// Policy is an ordered set of rules. Rules are evaluated first-match-wins.
type Policy struct {
	rules []Rule
}

// Decision is the result of evaluating a policy for one repository and
// capability.
type Decision struct {
	// Allowed reports whether the capability is granted.
	Allowed bool
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
		caps := make(map[Capability]bool, len(spec.Capabilities))
		for _, c := range spec.Capabilities {
			if !IsKnownCapability(c) {
				return nil, fmt.Errorf("policy: rule %d: unknown capability %q", i, c)
			}
			caps[Capability(c)] = true
		}
		rules = append(rules, Rule{
			Repositories: repos,
			Effect:       effect,
			Capabilities: caps,
		})
	}
	return &Policy{rules: rules}, nil
}

// Evaluate returns the decision for repo and capability c. The first rule whose
// pattern matches repo wins. When no rule matches, access is denied.
func (p *Policy) Evaluate(repo string, c Capability) Decision {
	for _, rule := range p.rules {
		if !ruleMatches(rule, repo) {
			continue
		}
		if rule.Effect == EffectDeny {
			return Decision{Allowed: false, Matched: true, Reason: "denied by rule"}
		}
		if rule.Capabilities[c] {
			return Decision{Allowed: true, Matched: true, Reason: "capability granted"}
		}
		return Decision{
			Allowed: false,
			Matched: true,
			Reason:  fmt.Sprintf("capability %s not granted", c),
		}
	}
	return Decision{Allowed: false, Matched: false, Reason: "no matching rule"}
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
		if rule.Effect == EffectAllow && rule.Capabilities[c] {
			return true
		}
	}
	return false
}

// Rules returns a deep copy of the compiled rules for introspection.
func (p *Policy) Rules() []Rule {
	out := make([]Rule, len(p.rules))
	for i, rule := range p.rules {
		caps := make(map[Capability]bool, len(rule.Capabilities))
		for c, ok := range rule.Capabilities {
			caps[c] = ok
		}
		out[i] = Rule{
			Repositories: append([]string(nil), rule.Repositories...),
			Effect:       rule.Effect,
			Capabilities: caps,
		}
	}
	return out
}

// SortedCapabilities returns the rule's granted capabilities in stable order.
func (r Rule) SortedCapabilities() []Capability {
	caps := make([]Capability, 0, len(r.Capabilities))
	for c, ok := range r.Capabilities {
		if ok {
			caps = append(caps, c)
		}
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	return caps
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
