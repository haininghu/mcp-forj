// Package policy implements capability-based authorization rules and the
// fail-closed .noai marker guard used by the MCP server.
package policy

// Capability names a single permitted action. Capabilities are opaque strings
// that are validated at configuration-load time.
type Capability string

// Known capabilities. CapRepoList, CapRepoRead, CapMRRead and CapMRComment are
// used by the first iteration's tools; the remaining values are reserved so the
// configuration vocabulary stays stable.
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
	// CapMRWrite permits creating, updating, merging or closing merge requests
	// (reserved).
	CapMRWrite Capability = "mr:write"
	// CapRepoWrite permits modifying repository content (reserved).
	CapRepoWrite Capability = "repo:write"
)

var knownCapabilities = []Capability{
	CapRepoList,
	CapRepoRead,
	CapMRRead,
	CapMRDiff,
	CapMRComment,
	CapMRWrite,
	CapRepoWrite,
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
