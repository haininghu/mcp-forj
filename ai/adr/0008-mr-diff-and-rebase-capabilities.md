# 0008. MR diff and rebase capabilities

## Status

Accepted.

## Context

`mr:write` was too broad to be useful: triggering a rebase is a different risk from
creating/merging MRs, and reading diffs is different from reading MR metadata (diffs
can contain source code). We wanted least-privilege grants for both.

## Decision

- Add `mr:diff` (read MR file diffs) and `mr:rebase` (trigger an MR rebase) as
  first-class capabilities, separate from the reserved `mr:write`.
- `get_merge_request_diff` (`mr:diff`) uses
  `GET /projects/:id/merge_requests/:iid/diffs` and requires at least one of
  `Merge Request: Read`. Output is capped at 100 files, 128 KiB per file and 512 KiB
  total, with per-file and top-level `truncated` flags.
- `rebase_merge_request` (`mr:rebase`) uses
  `PUT /projects/:id/merge_requests/:iid/rebase` (`Merge Request: Update`, plus a
  role allowed to push to the source branch). The rebase is **asynchronous**: the
  tool acknowledges the request and the outcome appears later via
  `get_merge_request` (`rebase_in_progress`, `merge_error`, `has_conflicts`,
  `detailed_merge_status`).
- Both capabilities accept MR **tag filters** (labels), evaluated like other MR
  operations after fetching MR metadata. A metadata-fetch failure fails closed.
- Diffs are merge-request metadata, not repository-content reads, so `.noai` does
  **not** protect them.
- Labels are never returned in any output.

## Consequences

- An operator can grant diff review without granting metadata list/comment, or grant
  rebase without granting diffs.
- Rebase adds a write path; it is authorized and tag-filtered like any other MR op.
- `mr:write` remains reserved for a future create/update/merge tool.
