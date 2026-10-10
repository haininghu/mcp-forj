# 0005. Repository path was not canonicalized and `read_file` ref bypassed `.noai`

## Symptom

A deny rule could be evaded with a non-canonical repository path, and the `.noai`
marker could be evaded by reading a non-default ref. Neither surfaced as an error.

## Impact

- Deny-rule bypass: with `deny: group/secret` followed by `allow: group/**`, a request
  for `group//secret`, `group/secret/` or `Group/Secret` did not match the deny literal
  but could match the allow glob, and a provider that normalizes the path would then
  return the protected repository.
- Log injection: the raw repository string (control characters allowed) was logged.
- `.noai` bypass: the marker was checked on the default branch only, while `read_file`
  read the caller-supplied `ref`, so a marker present only on another branch was ignored.

## Root cause

File paths were rigorously validated (`validatePath`) but the **repository path — the
primary authorization key — was passed verbatim** to both `doublestar.Match` and the
provider. `read_file` passed `in.Ref` to the provider while the marker check always used
`ref=HEAD` (`Guard.checkMarker`). Additionally, `matchesAnyPath`/`ruleMatches` treated a
`doublestar.Match` error as "no match", a fail-open direction for exclude/deny.

## Fix

- Added `validateRepo` (server): rejects absolute paths, backslashes, control
  characters, empty/relative segments and encoded traversal; returns a canonical path
  used for **both** policy matching and the provider call.
- Added `validateRef`: rejects control characters and over-long refs.
- `Guard.AuthorizeResourceRef` checks the marker on the default branch **and**, for a
  non-default ref, also at that ref (fail-closed). `read_file` uses it.
- `matchesAnyPath`/`ruleMatches` now treat an unexpected match error as a match
  (fail-closed). See ADR 0003.

## Follow-up

`validateRepo` initially canonicalized only the *syntax* (`path.Clean`); it did **not**
lowercase the path or reject a bare numeric project id, so the `Group/Secret` vector
named above and the numeric-id vector stayed open. Both were closed later by
lowercasing the returned path and requiring a `/` (see
`0009-repo-case-and-numeric-id-bypass.md`); the claim below that `Group/Secret` was
fixed by this change was therefore premature.

## Lesson

The authorization key must be canonicalized exactly once and shared by the policy and
the provider; never let two layers normalize the same input differently. A guardrail
that inspects one dimension (default branch) while the operation uses another (ref) is
bypassable. Fail-closed means the *error* direction must also deny.
