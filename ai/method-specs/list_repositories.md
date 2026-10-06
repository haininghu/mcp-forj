# list_repositories

## Purpose

List repositories explicitly configured for a provider (always) plus repositories
discovered through the provider API when `repo:list` is granted.

## Inputs

| Field    | Type   | Required | Notes                                              |
|----------|--------|----------|----------------------------------------------------|
| provider | string | yes      | Logical provider name; must be registered.         |
| search   | string | no       | Provider-side search term; passed verbatim.        |
| limit    | int    | no       | Bounds **discovered** repos; default 100, max 1000.|

## Required capability

`repo:list` for discovery only. Literal (non-glob) repositories listed in the config
are returned without any capability or provider call.

## GitLab endpoint(s)

- Group-first: `GET /groups/:id/projects?include_subgroups=true`
- Fallback / no-search: `GET /projects` (with `search` and `search_namespaces=true`
  when a search term is present)

Fine-grained permission: **Project: Read** (group/project must be in token scope).

## Authorization

- Literal repositories: returned subject to the first matching rule being `allow`
  (a `deny` rule hides them). No capability, provider call or `.noai` check; an
  explicitly configured repository is always listed even if it is `.noai`.
  Exception: an active `repo:list` topic filter also applies to literal entries;
  their topics are fetched and a failure/non-match omits them and increments
  `omitted` (fail-closed).
- Discovery: eligible when at least one allow rule grants `repo:list`
  (`Guard.AuthorizeList`). Each candidate is filtered with the policy-only
  `Guard.EvaluateWithTags(repo:list, topics)`; a `.noai` candidate is then omitted
  (counted in `omitted`) unless the `repo:list` grant is exempted with
  `noai: allow`, and a marker-check failure also omits it (fail-closed). Missing
  `repo:list` is not an error; only literal repositories are returned.
- Only discovered candidates are marker-checked; literal repositories are not.

## Behavior / limits

- When `search` is omitted, prefixes are derived from `repo:list` patterns and one
  call is made per prefix; an explicit `search` is one call.
- Results are deduplicated by provider+path (literal wins) and sorted by provider
  then path.
- `omitted` counts candidates that matched a rule but were blocked or failed a topic
  filter; no-rule candidates are filtered silently. `truncated` is set when a call
  offers more distinct candidates than the remaining capacity or the discovered cap
  is reached.
- Labels/topics are never returned.

## Errors

- `not found (HTTP 404)`, `forbidden: repository listing needs the Project: Read
  permission (and the group/project in the token scope) (HTTP 403)`, generic provider
  failure with `(HTTP <n>)` when known.
