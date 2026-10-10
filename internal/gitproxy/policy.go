package gitproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hvo/mcp-forj/internal/policy"
	"github.com/hvo/mcp-forj/internal/provider"
)

// errPushDenied marks a push rejected by policy (HTTP 403). Every provider
// metadata failure inside the check wraps it as well: the decision is
// fail-closed.
var errPushDenied = errors.New("gitproxy: push denied")

// errMalformedPush marks a request whose pkt-line command section cannot be
// parsed (HTTP 400). It is a protocol error, not a policy denial.
var errMalformedPush = errors.New("gitproxy: malformed push request")

// errRepoDenied marks a repository or capability denial (HTTP 403). A fetch,
// discovery or push authorization denial wraps it so all three answer the one
// identical "repository is not accessible" body, keeping a .noai repository or
// branch indistinguishable from an unknown or policy-denied one (invariant 6).
var errRepoDenied = errors.New("gitproxy: repository not accessible")

// isZeroOID reports whether s is an all-zero object id (creation when it is
// the old id, deletion when it is the new one). Callers receive only values
// validated as hex object ids by ReadReceivePack.
func isZeroOID(s string) bool {
	return s != "" && strings.Trim(s, "0") == ""
}

// checkPush applies the push policy to every ref update. All updates must
// pass; the first rejection denies the whole push. The wrapped detail is for
// logs only and never reaches the client.
func (s *Server) checkPush(ctx context.Context, p provider.Provider, repo string, updates []RefUpdate) error {
	for _, u := range updates {
		if err := s.checkRefUpdate(ctx, p, repo, u); err != nil {
			return err
		}
	}
	return nil
}

// checkRefUpdate validates one ref update: only branches under refs/heads/, a
// syntactically sane branch name, never the default branch, never a delete, and
// only updates that stay within the advertised history: the advertised old
// object id must equal the provider's current branch tip and the new object id
// must contain that tip (verified with the provider's merge base). A branch
// whose advertised base differs is a stale push (a non-fast-forward/force
// attempt) and denies. Which branches may be pushed at all is decided by the
// repo:write branches filter in the policy (Guard.AuthorizeBranch); the
// default-branch lock below is a hard-coded proxy guardrail on top of that
// decision and is not configurable.
func (s *Server) checkRefUpdate(ctx context.Context, p provider.Provider, repo string, u RefUpdate) error {
	if !strings.HasPrefix(u.Ref, "refs/heads/") {
		return fmt.Errorf("%w: ref %q is not a branch under refs/heads/", errPushDenied, u.Ref)
	}
	branch := strings.TrimPrefix(u.Ref, "refs/heads/")
	if !validBranchName(branch) {
		return fmt.Errorf("%w: branch name %q is invalid", errPushDenied, branch)
	}
	defaultBranch, err := p.DefaultBranch(ctx, repo)
	if err != nil {
		return fmt.Errorf("%w: cannot determine the default branch: %v", errPushDenied, err)
	}
	if branch == defaultBranch {
		return fmt.Errorf("%w: branch %q is the default branch", errPushDenied, branch)
	}
	if isZeroOID(u.NewSHA) {
		return fmt.Errorf("%w: deleting branch %q is not allowed", errPushDenied, branch)
	}
	if isZeroOID(u.OldSHA) {
		// Creating a new branch: nothing to fast-forward from.
		return nil
	}

	// Update of an existing branch, checked against the provider's current
	// state: (a) the advertised old id must equal the current tip — a stale
	// advertised base means the client lost the race and the update would
	// rewrite history (non-fast-forward/force), and (b) the new id must
	// contain that tip, i.e. MergeBase(tip, new) == tip exactly. Unknown
	// provider state fails closed.
	tip, err := p.ResolveRef(ctx, repo, branch)
	if err != nil {
		// A missing branch with a non-zero old id is inconsistent; ErrNotFound
		// and every other error deny alike.
		return fmt.Errorf("%w: cannot resolve branch %q: %v", errPushDenied, branch, err)
	}
	// Object ids are lowercase hex on the wire and from the provider, so an
	// exact comparison is the fail-closed choice for both checks below.
	if u.OldSHA != tip {
		return fmt.Errorf("%w: advertised base of branch %q is stale: pushed from %s but the tip is %s",
			errPushDenied, branch, u.OldSHA, tip)
	}
	mergeBase, err := p.MergeBase(ctx, repo, tip, u.NewSHA)
	if err != nil {
		return fmt.Errorf("%w: cannot compute merge base for branch %q: %v", errPushDenied, branch, err)
	}
	if mergeBase != tip {
		return fmt.Errorf("%w: update of branch %q is not a fast-forward", errPushDenied, branch)
	}
	return nil
}

// Push-option namespaces. A push option changes provider state beyond the ref
// update itself (it can open a merge request, arm auto-merge or steer CI), so
// only the explicitly handled namespaces ever pass and everything else is
// denied fail-closed.
const (
	// optionMergeRequest is the GitLab prefix for merge-request options, gated
	// by mr:write.
	optionMergeRequest = "merge_request."
	// optionMergeRequestTargetProject retargets the merge request to a different
	// project, which escapes the authorized repository, so it is always denied.
	optionMergeRequestTargetProject = "merge_request.target_project"
	// optionMergeRequestTarget names the target branch of the merge request; its
	// value is validated as a branch name.
	optionMergeRequestTarget = "merge_request.target"
	// optionMergeRequestAutoMerge and optionMergeRequestMergeWhenPipelineSucceeds
	// arm auto-merge, so they additionally require mr:merge.
	optionMergeRequestAutoMerge                 = "merge_request.auto_merge"
	optionMergeRequestMergeWhenPipelineSucceeds = "merge_request.merge_when_pipeline_succeeds"
	// optionCI is the GitLab prefix for CI options; they are never allowed.
	optionCI = "ci."
)

// allowedMergeRequestOptions is the exact set of merge-request push options this
// proxy forwards, mirroring GitLab's own vocabulary
// (lib/gitlab/push_options.rb). `merge_request.target_project` is deliberately
// absent: it is denied outright below. Any key outside this set — including a
// future `merge_request.*` option — is denied fail-closed, so an option GitLab
// later adds cannot silently ride the `mr:write` gate.
var allowedMergeRequestOptions = map[string]struct{}{
	"merge_request.assign":                       {},
	"merge_request.auto_merge":                   {},
	"merge_request.create":                       {},
	"merge_request.description":                  {},
	"merge_request.draft":                        {},
	"merge_request.label":                        {},
	"merge_request.merge_when_pipeline_succeeds": {},
	"merge_request.milestone":                    {},
	"merge_request.remove_source_branch":         {},
	"merge_request.squash":                       {},
	"merge_request.target":                       {},
	"merge_request.title":                        {},
	"merge_request.unassign":                     {},
	"merge_request.unlabel":                      {},
}

// checkPushOptions authorizes the GitLab push options of a push. Every option
// must be known; merge-request options require mr:write, auto-merge options
// additionally mr:merge, and anything else (ci.*, cross-project, unknown, or a
// merge_request.* key outside the allowlist) is denied fail-closed. All checks
// run against the same guard.
//
// Options arrive as "key" or "key=value" and are evaluated in two passes: the
// first applies the syntactic rules to every option and records which
// capabilities the set needs, the second asks the guard — at most once per
// capability, so a create+title+description group costs one decision. Asking
// the guard is a provider read (the .noai marker), and a rule violation such as
// a cross-project target denies before that read. A capability denial therefore
// wraps errRepoDenied (the identical generic "not accessible" 403 every other
// capability denial produces, keeping .noai indistinguishable), while an option
// the vocabulary rejects wraps errPushDenied, the same branch-policy 403 the
// other push guardrails produce. Client-visible text stays fixed either way, and
// the logged detail names only a constant vocabulary key — never a
// client-supplied key, value or ref, because option values carry titles,
// descriptions and labels that must not reach the log.
func (s *Server) checkPushOptions(ctx context.Context, rt *route, options []string) error {
	var (
		needsMRWrite  bool
		needsMRMerge  bool
		invalidBranch bool
	)
	for _, option := range options {
		key, value, _ := strings.Cut(option, "=")
		switch {
		// The denied keys below are fixed vocabulary constants, never client
		// input, so naming them leaks nothing.
		case key == optionMergeRequestTargetProject:
			return fmt.Errorf("%w: %s targets another project", errPushDenied, key)
		case strings.HasPrefix(key, optionMergeRequest):
			// Deny-by-default within the namespace: only the exact GitLab
			// vocabulary passes, so a future merge_request.* option cannot be
			// forwarded on the mr:write gate alone.
			if _, ok := allowedMergeRequestOptions[key]; !ok {
				return fmt.Errorf("%w: unrecognized push option", errPushDenied)
			}
			needsMRWrite = true
			switch key {
			case optionMergeRequestAutoMerge, optionMergeRequestMergeWhenPipelineSucceeds:
				needsMRMerge = true
			case optionMergeRequestTarget:
				// A target branch is a branch the merge request will be opened
				// against: reject a syntactically invalid name outright, so no
				// provider call ever sees it.
				if !validBranchName(value) {
					invalidBranch = true
				}
			}
		case strings.HasPrefix(key, optionCI):
			return fmt.Errorf("%w: %s* options are not allowed", errPushDenied, optionCI)
		default:
			return fmt.Errorf("%w: unrecognized push option", errPushDenied)
		}
	}
	if invalidBranch {
		return fmt.Errorf("%w: %s names an invalid branch", errPushDenied, optionMergeRequestTarget)
	}
	// Repository capability authorization for the options, through the same
	// guard as the ref updates: unknown tags fail closed and the .noai overlay
	// applies unless the matched grant is exempt.
	if needsMRWrite {
		if err := s.guard.Authorize(ctx, rt.providerName, rt.repo, policy.CapMRWrite); err != nil {
			return fmt.Errorf("%w: %w", errRepoDenied, err)
		}
	}
	if needsMRMerge {
		if err := s.guard.Authorize(ctx, rt.providerName, rt.repo, policy.CapMRMerge); err != nil {
			return fmt.Errorf("%w: %w", errRepoDenied, err)
		}
	}
	return nil
}

// validBranchName is a coarse git-check-ref-format check for the part after
// "refs/heads/": non-empty, no control characters or spaces, none of the
// forbidden characters ~^:?*[\, no "..", no leading '-', no leading or
// trailing '/' or '.', and no empty or dot-prefixed path segment.
func validBranchName(name string) bool {
	if name == "" {
		return false
	}
	if strings.ContainsAny(name, " \t~^:?*[\\") {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}
	for segment := range strings.SplitSeq(name, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") {
			return false
		}
	}
	return true
}
