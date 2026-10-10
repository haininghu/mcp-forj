# authorization

## Purpose

The shared evaluation pipeline used by every capability-gated tool: it turns a
provider, repository, capability and (optional) tags/path into an allow/deny
decision, with the `.noai` default-deny overlay layered on top for every
capability.

## Inputs

| Field      | Type                | Notes                                              |
|------------|---------------------|----------------------------------------------------|
| provider   | string              | Logical name; must be registered.                  |
| repo       | string              | Canonical `namespace/project` path.                |
| capability | `policy.Capability` | Must be known at config load.                      |
| tags       | `policy.TagSet`     | `Known=false` means tags unavailable.              |
| path       | string              | Repository-relative path for `repo:read`/`write`.  |
| ref        | string              | Git ref for the marker check (`read_file` only).   |

The repository path is structurally validated by `validateRepo` (provider-neutral) and
then normalized to the provider's canonical authorization key by
`Provider.CanonicalRepository`; that canonical value is used for both policy matching
and the provider call, so the two cannot diverge. `validateRepo` rejects absolute paths,
backslashes, control characters, empty/relative segments and encoded traversal.
`CanonicalRepository` rejects an identifier the backend would interpret differently from
a namespaced path (the GitLab implementation rejects a bare numeric project id and
lowercases, because GitLab resolves paths case-insensitively) and returns a stable
canonical form; configured repository patterns must match that form. The git proxy
applies the same provider method in `parseRoute`/`RemoteURL`. The file path is
canonicalized by `validatePath` and the `ref` by `validateRef` (control characters,
length bound).

## Required capability

N/A — this is the evaluator used by all tools.

## GitLab endpoint(s)

- `FileExists(repo, ".noai", "HEAD")` — the marker check for non-exempt
  capabilities.

## Authorization

1. Resolve the provider from the registry.
2. Validate arguments (bounds; path traversal for `read_file`).
3. Optional policy-only pre-check: `Guard.AuthorizeRepoCapability` (ignores tags and
   marker) before a privileged metadata fetch.
4. If the matched grant has a tag constraint, fetch metadata:
   MR labels for `mr:*`; project topics for `repo:*`. A fetch failure denies.
5. Final decision:
   - `Guard.AuthorizeWithTags` — tags only (no path). Used by MR tools.
   - `Guard.AuthorizeResource` — tags + path. Used by `read_file`.
   - `Guard.AuthorizeBranch` — tags + branch (no path). Used by the git proxy per
     push target branch; a grant without a `branches` filter denies the push.
   Unless the matched grant is `noai`-exempt, this also runs the `.noai` check.
6. On allow, perform the provider call; map errors with `mapProviderError`.

Tools that evaluate tags per item or per candidate (repository discovery,
`list_merge_requests`) use the policy-only `Evaluate`/`EvaluateWithTags` and then
`Guard.CheckNoAI(ctx, provider, repo, decision.NoAIExempt)`.

Decision reasons (all fail closed unless allowed):

- `no matching rule` — no rule matched the repository (unknown repository).
- `denied by rule` — a `deny` rule matched.
- `capability <c> not granted` — the matched allow rule lacks the capability.
- `tag information unavailable` — active tag filter, tags unknown.
- `tag requirement not met` / `excluded tag present` — exact, case-sensitive.
- `path required` — active path filter, empty path.
- `path not allowed` / `path excluded` — doublestar; exclude wins over include.
- `branch filter required` — named branch, grant has no branches filter (no push).
- `branch required` — branch filter active but no branch supplied to a branch-context
  evaluation (`EvaluateResourceBranch`/`AuthorizeBranch`). Branch-less entry points
  (`Evaluate`/`Authorize`/`AuthorizeResource`) never apply the branch dimension.
- `branch not allowed` / `branch excluded` — doublestar; exclude wins over include.
- `.noai` — marker present or marker check failed, for any capability whose
  matched grant is not `noai`-exempt.

## Behavior / limits

- Rules are ordered; the first matching rule wins.
- Deny-by-default: no match denies.
- Tags/paths are never cached or returned.
- `.noai` is a capability-level default-deny overlay: every capability is denied on
  a `.noai` repository unless the matched grant is exempted with `noai: allow`.
  Literal (explicitly configured) repositories are still listed; discovered `.noai`
  repositories are omitted unless the `repo:list` grant is exempt. The marker is
  checked on the default branch and is fail-closed.

## Errors

`ErrUnknownProvider`, `ErrUnknownRepository`, `ErrDenied`, `ErrNoAI`,
`ErrMarkerCheck` (wrapping the underlying provider error), all via `errors.Is`.

At the tool boundary, `ErrNoAI` is mapped to the **same response as
`ErrUnknownRepository`**, so a `.noai` denial does not confirm that the repository
exists. The precise reason is logged server-side by the guard.
