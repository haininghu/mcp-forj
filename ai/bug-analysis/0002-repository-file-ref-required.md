# 0002. `read_file` omitted `ref`, so the `.noai` check failed on every call

## Symptom

Every `read_file` without an explicit `ref` failed. The error was
`could not verify the .noai marker for repository ...; access denied`, even on
repositories with no `.noai` file and a token that could read files.

## Impact

`read_file` was unusable in its default form (no `ref`). Because the marker check
is part of `repo:read` authorization, the failure looked like a policy problem
rather than a request-shape problem.

## Root Cause

GitLab's repository-files endpoint requires a `ref` query parameter; there is no
server-side default branch resolution. The provider built `GetFileOptions` and
omitted `Ref` when the caller passed an empty ref, so GitLab answered `400 Bad
Request`. `mapError` folded that into a marker-check failure, which is fail-closed.

The correct way to ask for the default branch is the special ref value `HEAD`.

## Fix

- `internal/provider/gitlab/gitlab.go`: `refOptions` maps an empty ref to `HEAD`
  for both `ReadFile` and `FileExists`, so the marker check and file reads always
  send a ref.
- The behavior is covered by provider tests and by `read_file`/marker tests.

## Lesson

Verify provider endpoint request requirements (required query params, default
semantics) against the API and the client, not just the happy path. A fail-closed
path can mask a simple malformed request for a long time. See ADR 0003.
