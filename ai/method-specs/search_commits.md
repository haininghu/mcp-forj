# search_commits

## Purpose

Search commits by one author across the repositories the token is allowed to search: the literal
`commit:read` grants plus the provider-side discovery enabled by `repo:list` (cross-repo
"where and when did I commit?"), newest first by committer date.

## Inputs

| Field    | Type   | Required | Notes                                                        |
|----------|--------|----------|--------------------------------------------------------------|
| provider | string | yes      | Logical provider name; must be registered.                   |
| author   | string | no       | Provider-side name/email search; defaults to the current user.|
| since    | string | no       | Inclusive lower bound on the committer date, RFC3339 or `YYYY-MM-DD`. |
| until    | string | no       | Inclusive upper bound, RFC3339 or `YYYY-MM-DD` (whole day UTC). |
| query    | string | no       | Case-insensitive substring matched against the commit message.|
| limit    | int    | no       | Max commits; default 100, capped 100.                        |

## Required capability

Two independent axes:

- **Enumeration** (`repo:list`, for provider-side discovery only, exactly as `list_repositories`):
  literal `commit:read` repositories are always candidates without it (subject to a listing deny and
  an active `repo:list` filter).
- **Content** (`commit:read`): also granted by a `repo:read` grant (same filter: topics and
  `noai: allow` carry over, an active `paths` filter fails `commit:read` closed, and an explicit
  `commit:read` entry wins). Tag filters match project topics. `paths`/`branches` are rejected by
  config validation for this capability.

## GitLab endpoint(s)

- `GET /projects/:id/repository/commits` (default branch; `ref_name` never set)
- `GET /user` (only when `author` is omitted, to resolve the current identity)

Fine-grained permission: **Repository: Read** (commits); `read_user` for `/user`.

## Authorization

Candidate enumeration mirrors `list_repositories`:

1. Literal repositories from allow grants of `commit:read` (directly or via `repo:read`) are always
   candidates, subject to a listing deny (`Guard.Listable`) and to an active `repo:list` filter
   (`Guard.HasFilter(repo:list)`): in the latter case their topics are fetched and
   `Guard.EvaluateWithTags(repo:list)` must allow, otherwise the literal is omitted (fail-closed).
2. Provider-side discovery runs only when an allow rule grants `repo:list` (`Guard.AuthorizeList`).
   Search prefixes come from `Guard.ListSearchPrefixes(repo:list)`. Every discovered repository is
   evaluated with `Guard.EvaluateWithTags(repo:list, topics)` and `Guard.CheckNoAI` for the
   `repo:list` grant; a discovery failure is fatal (fail-closed).

Only the provider-side discovery is capped at 50 (`maxSearchRepos`); literal `commit:read`
repositories are never capped.

The content axis then runs per candidate repository, in order:

1. `Guard.AuthorizeRepoCapability(commit:read)` — policy-only pre-check.
2. Topic fetch when `Guard.HasTagConstraint(commit:read)` and the topics are not already known from
   enumeration; a fetch error omits the repository (fail-closed).
   `Guard.EvaluateWithTags(commit:read, topics)`.
3. `Guard.CheckNoAI` unless the matched grant is exempted with `noai: allow`.

A candidate that fails any content step is omitted and counted server-side only. `Guard.Listable` is
not checked separately: literal candidates already passed it and discovered candidates passed the
`repo:list` evaluation.

## Behavior / limits

- Enumeration seeds from literal repositories whose allow rules grant `commit:read` (directly or via
  `repo:read`), then provider-side discovery through the `repo:list` search prefixes (only when
  `repo:list` is granted); only the discovery is capped at 50 (`maxSearchRepos`), literal
  `commit:read` repositories are not. A discovery failure is fatal (fail-closed).
- Only the default branch is searched. `since`/`until` bound the committer date
  inclusively; a date-only `until` is extended to 23:59:59Z.
- The default author resolves via `GET /user`; if neither email nor name is known
  the request fails closed (`pass author explicitly`) — it never returns all
  authors. The provider prefilter is a fuzzy substring match, so an exact identity
  is enforced case-insensitively client-side by the provider.
- Up to 200 commits are inspected per repository (`maxCommitsPerRepo`, used as both the match limit
  and the scan budget): at most 200 matched commits are returned and the number of provider calls is
  bounded even when the server-side author prefilter over-matches. The textual
  `query` is applied client-side, and results are deduplicated on `(repo, sha)`.
- Output commits carry `provider, repo, sha, short_sha, title, message, author,
  authored_at, committed_at, web_url` (UTC `2006-01-02T15:04:05Z`); the title and
  message are each capped at 1 KiB. Emails, topics and labels are never returned.
  `truncated` is set when any budget or the result cap is reached.

## Errors

Only two failure classes propagate to the client as a tool error:

- **Identity** (default author, resolved via `GET /user`): `could not determine your commit identity;
  pass author explicitly` (fail-closed) or `forbidden: reading your account needs the read_user scope
  (HTTP 403)`.
- **Discovery**: `repository listing needs the Project: Read permission ... (HTTP 403)`, or the
  generic `not found (HTTP 404)`/`(HTTP <n>)` mapped provider error when known.

A per-repository `ListCommits` failure is **not** propagated: the repository is omitted, counted
server-side only, and never named in the output (error hygiene). A search whose candidates all fail
per-repository therefore returns an empty (or partial) result, not an error.
