# 0003. `.noai` marker scope and semantics

## Status

Accepted. Scope later narrowed to content operations by ADR/design updates.

## Context

Some repositories must be entirely off limits to the agent. Repositories mark this
with a `.noai` file. We needed to decide what the marker protects, how it is
checked, and how failures behave.

## Decision

- The `.noai` marker protects only operations on repository **contents**:
  `repo:read` (`read_file`) and `repo:write` (reserved, no tool yet). The set is
  defined by `policy.IsMarkerProtected`.
- It does **not** affect merge-request capabilities (`mr:read`, `mr:diff`,
  `mr:comment`, `mr:rebase`) or repository listing (`repo:list`,
  `list_configured_rules`). Merge requests and listing are metadata/discovery.
- The marker is checked on the repository's **default branch** using the server's
  own token via a dedicated provider call (`FileExists` with `ref=HEAD`).
- For GitLab, an empty ref is sent as `HEAD` because the repository-files endpoint
  has no server-side default.
- The check is **fail-closed**: if the file cannot be read (including 401/403), the
  content operation is denied.
- There is **no cache**: the marker is checked on every content operation.
- The check runs inside `Guard.AuthorizeResource` after the policy (and tag/path)
  decision, so a denied request never reaches the provider.

## Consequences

- A token without repository file read access makes `read_file` fail closed even on
  repositories without a marker. The error names the forbidden cause and the HTTP
  status (see ADR 0009).
- Merge-request work and discovery keep functioning on `.noai` repositories, which
  is intentional.
- Every `read_file` costs one extra provider call; accepted for correctness.
