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

The `tags` dimension uses MR labels for `mr:*` and project topics for `repo:*` and `commit:*`.

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
   MR labels for `mr:*`; project topics for `repo:*` and `commit:*`. A fetch failure denies.
5. Final decision:
   - `Guard.AuthorizeWithTags` — tags only (no path). Used by MR tools.
   - `Guard.AuthorizeResource` — tags + path. Used by `read_file`.
   - `Guard.AuthorizeBranch` — tags + branch (no path). Used by the git proxy per
     push target branch; a grant without a `branches` filter is a hard fail-closed
     deny for a named branch.
   Unless the matched grant is `noai`-exempt, this also runs the `.noai` check.

The evaluation is **firewall-style and symmetric** (ADR 0012): a rule **applies** when its
`repositories` scope matches AND (a) it is a `deny` rule without capabilities, or (b) one of its
capability entries matches the requested capability and all of that entry's filters pass. The first
applicable rule wins; `allow` grants and `deny` denies. An **evaluated** filter miss (tag
requirement not met, excluded tag present, path/branch not matched) means the rule does not apply and
evaluation continues (fall-through); a filter whose input is **unavailable** fails closed. `deny`
rules may list capabilities and filters; `noai: allow` on a `deny` rule is rejected at config load.

**Capability implication.** Granting `repo:read` also authorizes `commit:read`: commit metadata is
repository content and a fetch/clone over `repo:read` already exposes the history. The implication
is symmetric — a `repo:read` `deny` denies `commit:read` too. An explicit entry for the requested
capability wins over an implied one, the implying entry's filters (topics, `noai: allow`) carry
over, and an active `paths` filter on `repo:read` fails `commit:read` closed (a commit search has no
path). `commit:read` remains grantable on its own.

6. On allow, perform the provider call; map errors with `mapProviderError`.

Tools that evaluate tags per item or per candidate use the policy-only
`Evaluate`/`EvaluateWithTags` and then `Guard.CheckNoAI(ctx, provider, repo, exempt)`.
Repository discovery takes `exempt` from the same per-candidate decision (`decision.NoAIExempt`),
while `list_merge_requests` evaluates the repository-level overlay once and therefore uses the
conservative `Guard.NoAIExemptAll` (the marker is skipped only when every `mr:read` grant is exempt).
`search_commits` is a per-candidate content user whose enumeration mirrors `list_repositories`:
literal `commit:read` repositories come from `Guard.StaticRepositoriesFor` (which applies the listing
deny; the active `repo:list` filter is applied separately), and provider-side discovery runs only behind
`Guard.AuthorizeList`, evaluating each discovered repository with
`Guard.EvaluateWithTags(repo:list)` and the `repo:list` `.noai` overlay. It then authorizes the
`commit:read` content axis per candidate (`Guard.AuthorizeRepoCapability`, `Guard.HasTagConstraint`,
`Guard.EvaluateWithTags`, `Guard.CheckNoAI`), reusing topics already known from enumeration. It
therefore never searches a repository a listing deny hides from `list_repositories` and never exposes
one discovery did not yield.

Decision reasons (all fail closed unless allowed):

- `no matching rule` — no rule's scope matched the repository (unknown repository).
- `no applicable rule` — a rule's scope matched but no rule applied (capability unlisted and/or an
  evaluated filter miss) and no more specific miss reason below outranks it. Maps to access denied.
- `denied by rule` — an applicable `deny` rule won.
- `capability <c> not granted` — a rule did not list the requested capability (fall-through).
- Evaluated filter misses, reported as the most specific reason when no rule applies:
  `tag requirement not met` / `excluded tag present` (exact, case-sensitive),
  `path not allowed` / `path excluded` (doublestar; exclude wins over include),
  `branch not allowed` / `branch excluded` (doublestar; exclude wins over include).
- Fail-closed (hard deny, never a fall-through):
  - `tag information unavailable` — active tag filter, tags unknown.
  - `path required` — active path filter, empty path.
  - `branch filter required` — named branch, grant has no `branches` filter (no push).
  - `branch required` — branch filter active but no branch supplied to a branch-context evaluation
    (`EvaluateResourceBranch`/`AuthorizeBranch`). Branch-less entry points
    (`Evaluate`/`Authorize`/`AuthorizeResource`) never apply the branch dimension.
- `.noai` — marker present or marker check failed, for any capability whose matched grant is not
  `noai`-exempt.

## Behavior / limits

- Rules are ordered; the first applicable rule wins (scope + capability + filters).
- A `deny` rule may list capabilities and filters; without capabilities it denies every capability
  in its scope. An evaluated filter miss falls through to a later rule; unavailable filter
  information fails closed.
- Deny-by-default: no applicable rule denies.
- Tags/paths are never cached or returned.
- `.noai` is a capability-level default-deny overlay: every capability is denied on
  a `.noai` repository unless the matched grant is exempted with `noai: allow`.
  Literal (explicitly configured) repositories are still listed; discovered `.noai`
  repositories are omitted unless the `repo:list` grant is exempt. The marker is
  checked on the default branch and is fail-closed.
- `commit:read` is capability-level `.noai`-protected like any other and is checked on
  the repository's default branch only; `search_commits` searches that branch alone, so
  a broader ref range would make the marker check non-fail-closed and is not offered.
- `list_merge_requests` evaluates the repository-level `.noai` overlay once, before per-item tag
  evaluation, so it skips the marker only when **every** `mr:read` grant is `noai`-exempt
  (`Guard.NoAIExemptAll`); otherwise a per-item tag fall-through to a non-exempt grant could not
  bypass the marker.

## Errors

`ErrUnknownProvider`, `ErrUnknownRepository`, `ErrDenied`, `ErrNoAI`,
`ErrMarkerCheck` (wrapping the underlying provider error), all via `errors.Is`.

At the tool boundary, `ErrNoAI` is mapped to the **same response as
`ErrUnknownRepository`**, so a `.noai` denial does not confirm that the repository
exists. The precise reason is logged server-side by the guard.
