# authorization

## Purpose

The shared evaluation pipeline used by every capability-gated tool: it turns a
provider, repository, capability and (optional) tags/path into an allow/deny
decision, with the `.noai` marker layered on top for content operations.

## Inputs

| Field      | Type                | Notes                                              |
|------------|---------------------|----------------------------------------------------|
| provider   | string              | Logical name; must be registered.                  |
| repo       | string              | Canonical `namespace/project` path.                |
| capability | `policy.Capability` | Must be known at config load.                      |
| tags       | `policy.TagSet`     | `Known=false` means tags unavailable.              |
| path       | string              | Repository-relative path for `repo:read`/`write`.  |

## Required capability

N/A — this is the evaluator used by all tools.

## GitLab endpoint(s)

- `FileExists(repo, ".noai", "HEAD")` — the marker check for content operations.

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
   For marker-protected capabilities only, this also runs the `.noai` check.
6. On allow, perform the provider call; map errors with `mapProviderError`.

Decision reasons (all fail closed unless allowed):

- `no matching rule` — no rule matched the repository (unknown repository).
- `denied by rule` — a `deny` rule matched.
- `capability <c> not granted` — the matched allow rule lacks the capability.
- `tag information unavailable` — active tag filter, tags unknown.
- `tag requirement not met` / `excluded tag present` — exact, case-sensitive.
- `path required` — active path filter, empty path.
- `path not allowed` / `path excluded` — doublestar; exclude wins over include.
- `.noai` — marker present or marker check failed, for `repo:read`/`repo:write`.

## Behavior / limits

- Rules are ordered; the first matching rule wins.
- Deny-by-default: no match denies.
- Tags/paths are never cached or returned.
- `.noai` protects only `repo:read` and `repo:write`; it does not affect `mr:*` or
  `repo:list`.

## Errors

`ErrUnknownProvider`, `ErrUnknownRepository`, `ErrDenied`, `ErrNoAI`,
`ErrMarkerCheck` (wrapping the underlying provider error), all via `errors.Is`.
