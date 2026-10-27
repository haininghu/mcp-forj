# 0013. Commit search

## Status

Accepted.

## Context

The tools let an agent read files and merge requests, but not answer "where and when did I
commit?" across repositories. Commits are repository content: a commit message can carry as much
detail as a file. A cross-repository search therefore needs its own capability, its own
authorization and a discovery rule that cannot expose repositories the agent may not list.

## Decision

### Capability

- A new capability `commit:read` (object group `commit:*`, analogous to `mr:*` and `repo:*`).
- `repo:read` **implies** `commit:read`: commit metadata is repository content, and a fetch/clone
  over `repo:read` already exposes the full history. The implication is symmetric for `allow` and
  `deny` (ADR 0012): a `deny` of `repo:read` also denies `commit:read`, so a later
  `allow repo:read` cannot defeat the denial. An explicit `commit:read` entry in a rule wins over
  the implied `repo:read` entry, and the implying entry's filters (topics, `noai: allow`) carry
  over. An active `paths` filter on a `repo:read` grant fails `commit:read` closed (there is no
  path in a commit search), exactly as it disables git access.
- `commit:read` remains grantable on its own and accepts tag filters (matched against project
  topics, as for `repo:*`) and `noai: allow`. It rejects `paths` and `branches` at config load (a
  commit search has neither a file path nor a branch dimension).

### Tool

`search_commits` searches commits by one author across repositories:

- **Author.** Defaults to the authenticated user (`CurrentUser`); an explicit `author` is a
  provider-side name/email search.
- **Time range.** `since`/`until` bound the committer date inclusively (RFC3339 or `YYYY-MM-DD`).
- **Text.** An optional `query` substring is matched case-insensitively against the commit
  message, client-side.
- **Scope.** An optional `repos` list restricts the search to exactly those repositories. Naming them
  bypasses the enumeration axis (no `repo:list`), because no repository name has to be discovered;
  only the `commit:read` content axis applies, exactly as `read_file` authorizes one repository. The
  list is validated and canonicalized per entry, deduplicated on the canonical path and capped at
  `maxSearchRepos`.

### Visibility gate

The candidate repository set is enumerated exactly like `list_repositories`; the content gate is a
separate, second step (axis separation):

- **Enumeration (`repo:list`).** Literal repositories whose allow rules grant `commit:read`
  (directly or via `repo:read`) are always candidates, unless a listing deny hides them or an active
  `repo:list` filter rejects them. Provider-side discovery requires an allow rule that grants
  `repo:list`; discovered repositories are evaluated against `repo:list` (project topics) and the
  `repo:list` `.noai` overlay, exactly as in `list_repositories`. `repo:list` therefore governs which
  repositories can be found at all, and `search_commits` can never expose a repository that
  `list_repositories` hides.
- **Content (`commit:read`).** Each candidate is then authorized for `commit:read` (policy
  pre-check, tag evaluation against project topics, `.noai` overlay) before its commits are fetched.
- **Explicitly named repositories.** An optional `repos` list bypasses the enumeration axis
  entirely: no `repo:list` is required (and no discovery runs), because the caller already supplies
  the names and nothing has to be found. Only the `commit:read` content axis applies, exactly like
  `read_file`'s single-repository content authorization. A repository hidden from listing by a
  `deny repo:list` rule is therefore still searchable when named explicitly; the denial still applies
  to the unnamed, discovery-based search.

The two axes are independent: `repo:read` implies `commit:read` for the content axis, but it does not
enable discovery. A glob-only `commit:read`/`repo:read` grant searches only its literal repositories
unless `repo:list` is also granted. Enumeration seeds from literal `commit:read` grants (direct or
implied) plus the `repo:list` provider search prefixes.

### Default branch only

Only each repository's default branch is searched. Searching every ref would make the `.noai`
overlay non-fail-closed: the marker could not be checked at refs the client never names.

### Author identity

The provider matches the author exactly and case-insensitively; the email is never returned to the
client or logged. It does pass through the provider layer internally (it is the matching key and,
for the default author, travels via `provider.User` from the provider to the server), but it never
appears in tool output. When no `author` is given and `CurrentUser` has neither an email nor a name, the
request fails closed — it never degrades into returning all authors. Known limits: no-reply and
private addresses, multiple git identities per person, bots, and `committer` vs `author` mean the
identity is not guaranteed complete; an explicit `author` is a provider-side fuzzy filter.

### Time dimension

GitLab's `since`/`until` are committer-date filters; the tool filters and sorts by committer date,
and additionally shows `authored_at`.

### Budgets

At most 100 results, 50 discovered candidate repositories (literal candidates are not capped, as in
`list_repositories`; an explicit `repos` list is capped at the same number after deduplication) and
200 commits inspected per repository. The per-repository budget is passed as
both a match limit and a scan limit (`CommitListOptions.ScanLimit`): the match limit caps matched
commits, while the scan limit caps how many raw commits are inspected across pages, so a fuzzy
server-side author prefilter that over-matches cannot page without bound. `truncated` is set only
when a limit or budget is exhausted, never when repositories are skipped or denied.

### Naming

`search_commits`, not `list_commits`, so a future per-repository commit-list tool can keep that
name. The capability is `commit:read`.

### Data hygiene

Emails, topics and labels are never returned in tool output or logs.

## Consequences

- `commit:read` is no longer a separate trust boundary from `repo:read` for reading content: a
  `repo:read` grant now also grants commit access. Enumeration stays governed by `repo:list`, so a
  `repo:read` holder only searches repositories configured literally (or discovered through an
  existing `repo:list` grant); a deployment that wants to prevent even that adds a `deny` rule for
  `commit:read`. `commit:read` can still be granted on its own.
- A `commit:read` grant without `noai: allow` is denied on `.noai` repositories, and a repository
  hidden from listing is omitted (and counted server-side only), so `.noai` repositories stay
  indistinguishable from unknown ones.
- Fine-grained tokens need the **Commit: Read** project permission for the commits endpoint. The
  **Repository: Read** permission used by `read_file` (and the `.noai` marker read) does not cover
  it, so a token can read files and still get HTTP 403 on commit search.
- The cross-repo search costs one topic fetch (only under a tag constraint) and one commit
  enumeration per candidate repository; the candidate set is capped and the counts are not
  returned to the client.
