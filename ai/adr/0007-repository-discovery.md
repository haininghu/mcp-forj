# 0007. Repository discovery

## Status

Accepted.

## Context

`list_repositories` must show both configured repositories and ones the token can
reach, without over-fetching or hiding repositories. Early attempts used GitLab's
`membership=true` and a single unfiltered project list, which missed accessible
repositories and filled the fetch window with non-matching projects.

## Decision

- **Literal repositories win.** Concrete paths listed literally in the config are
  always returned (subject to `deny`), with no `repo:list` capability, no provider
  call, and no `.noai` check. Exception: an active `repo:list` topic filter also
  applies to literal entries (topics are fetched; on error/non-match they are
  omitted and counted in `omitted`).
- **Discovery requires `repo:list`.** One call per derived search prefix.
- **Prefix derivation:** `Policy.ListSearchPrefixes` takes the literal part of each
  `repo:list` pattern before the first glob metacharacter, deduplicates and sorts
  (`devops/platform/**` -> `devops/platform`). An explicit `search` is used verbatim.
- **Group-first:** each prefix is tried via the Group-boundary endpoint
  `GET /groups/:id/projects?include_subgroups=true` (works for group-scoped
  fine-grained tokens), falling back to `GET /projects?search=...&search_namespaces=true`
  when the term is not a group. `search_namespaces=true` is required because GitLab's
  default search matches only project path/name/description.
- **Scope:** `project_scope` is `accessible` (default; all projects the token can
  see, `membership=false`) or `membership` (`membership=true`).
- **Limits:** discovery is capped at 1000 (default 100); literal repositories are not
  capped. Results are deduplicated, sorted by provider+path, and report `omitted`
  (matched-but-blocked or failed-filter) and `truncated` (cap or fetch window hit).

## Consequences

- Fine-grained/group-scoped tokens can discover their namespaces.
- Multiple provider calls may occur (one per prefix); the result is deduplicated.
- `list_repositories` never checks the `.noai` marker, so `.noai` repos may appear.
