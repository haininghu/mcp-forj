# 0009. Error hygiene

## Status

Accepted.

## Context

Raw GitLab errors can contain response bodies, and a generic "provider request
failed" hides whether the cause is a missing scope, a non-rebaseable MR, or a
missing `.noai` marker. We need messages that are safe to show an agent but
diagnosable by an operator.

## Decision

- Providers map native errors to neutral sentinels and attach the HTTP status:
  - 401/403 -> `ErrForbidden`
  - 400/405/409 -> `ErrInvalidState`
  - 404 -> `ErrNotFound`
  - other non-zero -> generic failure, with the status
  - unknown/no status -> generic failure
- The status travels in `provider.HTTPError{Status, Err}`; `provider.HTTPStatus`
  extracts it. `HTTPError.Error()` is `"<sentinel> (HTTP <n>)"`.
- The server maps sentinels to safe messages and appends `(HTTP <n>)` when known.
  It never includes response bodies or tokens.
- **Forbidden messages are operation-specific.** Each call site passes a hint naming
  the resource permission the operation needs, e.g.:
  - notes write -> `Work Item: Create`; notes read -> `Work Item: Read`
  - rebase -> `Merge Request: Update`
  - MR reads/diffs -> `Merge Request: Read`
  - file reads -> `Repository: Read`
  - listing -> `Project: Read`
  Without a hint the generic forbidden message is used.
- `.noai` marker-check failures wrap the underlying cause: the message names the
  forbidden repository-read/scope cause when that is it, otherwise stays generic,
  and always fails closed.

## Consequences

- Agents get actionable messages; operators can distinguish permission problems
  from state problems without seeing secrets.
- Adding a new tool should pass an accurate forbidden hint.
- Sentinel-based `errors.Is` checks keep working through the `HTTPError` wrapper.
