// Package policy implements capability-based authorization rules and the
// fail-closed .noai marker guard used by the MCP server.
package policy

// Capability names a single permitted action. Capabilities are opaque strings
// that are validated at configuration-load time.
type Capability string

// Known capabilities. CapRepoList, CapRepoRead, CapMRRead, CapMRDiff,
// CapCommitRead, CapMRComment, CapRebase, CapMRMerge and CapPolicyRead are used
// by the tools, CapRepoWrite and CapMRWrite by the git smart-HTTP proxy.
const (
	// CapRepoList permits discovering repositories matching the configured
	// patterns.
	CapRepoList Capability = "repo:list"
	// CapRepoRead permits reading repository files and serving git
	// fetch/clone through the git smart-HTTP proxy.
	CapRepoRead Capability = "repo:read"
	// CapCommitRead permits reading commit metadata and messages of a
	// repository's default branch (search_commits). Commit metadata is
	// repository content, so a repo:read grant also grants commit:read (an
	// explicit commit:read entry wins, and a repo:read deny denies it too). An
	// active grant is .noai-protected unless it is exempted with `noai: allow`.
	// Tag filters match project topics; paths and branches are not supported.
	CapCommitRead Capability = "commit:read"
	// CapMRRead permits listing and viewing merge request metadata and notes.
	CapMRRead Capability = "mr:read"
	// CapMRDiff permits reading merge request diff content.
	CapMRDiff Capability = "mr:diff"
	// CapMRComment permits creating comments on merge requests.
	CapMRComment Capability = "mr:comment"
	// CapRebase permits triggering a merge request rebase.
	CapRebase Capability = "mr:rebase"
	// CapMRMerge permits merging a merge request. The git smart-HTTP proxy also
	// requires it for push options that arm auto-merge.
	CapMRMerge Capability = "mr:merge"
	// CapMRWrite permits creating, updating or closing merge requests. No MCP
	// tool uses it yet; it gates merge-request creation via git push options in
	// the git smart-HTTP proxy.
	CapMRWrite Capability = "mr:write"
	// CapRepoWrite permits modifying repository content: it authorizes the
	// git smart-HTTP push through the git proxy. It is the only capability
	// that accepts a branches filter.
	CapRepoWrite Capability = "repo:write"
	// CapPolicyRead permits exposing the configured access rules
	// (list_configured_rules). It is provider-scoped, not repository-scoped.
	CapPolicyRead Capability = "policy:read"
)

var knownCapabilities = []Capability{
	CapRepoList,
	CapRepoRead,
	CapCommitRead,
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
