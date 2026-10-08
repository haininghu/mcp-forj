// Package policy implements capability-based authorization rules and the
// fail-closed .noai marker guard used by the MCP server.
package policy

// Capability names a single permitted action. Capabilities are opaque strings
// that are validated at configuration-load time.
type Capability string

// Known capabilities. CapRepoList, CapRepoRead, CapMRRead, CapMRDiff,
// CapMRComment, CapRebase, CapMRMerge and CapPolicyRead are used by the tools;
// the remaining values are reserved so the configuration vocabulary stays
// stable.
const (
	// CapRepoList permits discovering repositories matching the configured
	// patterns.
	CapRepoList Capability = "repo:list"
	// CapRepoRead permits reading repository files.
	CapRepoRead Capability = "repo:read"
	// CapMRRead permits listing and viewing merge request metadata and notes.
	CapMRRead Capability = "mr:read"
	// CapMRDiff permits reading merge request diff content (reserved).
	CapMRDiff Capability = "mr:diff"
	// CapMRComment permits creating comments on merge requests.
	CapMRComment Capability = "mr:comment"
	// CapRebase permits triggering a merge request rebase.
	CapRebase Capability = "mr:rebase"
	// CapMRMerge permits merging a merge request.
	CapMRMerge Capability = "mr:merge"
	// CapMRWrite permits creating, updating or closing merge requests
	// (reserved).
	CapMRWrite Capability = "mr:write"
	// CapRepoWrite permits modifying repository content (reserved).
	CapRepoWrite Capability = "repo:write"
	// CapPolicyRead permits exposing the configured access rules
	// (list_configured_rules). It is provider-scoped, not repository-scoped.
	CapPolicyRead Capability = "policy:read"
)

var knownCapabilities = []Capability{
	CapRepoList,
	CapRepoRead,
	CapMRRead,
	CapMRDiff,
	CapMRComment,
	CapRebase,
	CapMRMerge,
	CapMRWrite,
	CapRepoWrite,
	CapPolicyRead,
}

// KnownCapabilities returns a copy of all capabilities known to the server.
func KnownCapabilities() []Capability {
	return append([]Capability(nil), knownCapabilities...)
}

// IsKnownCapability reports whether s names a known capability.
func IsKnownCapability(s string) bool {
	for _, c := range knownCapabilities {
		if string(c) == s {
			return true
		}
	}
	return false
}
