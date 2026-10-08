# 0010. MR merge capability

## Status

Accepted. Supersedes the merge part of ADR 0008's `mr:write` reservation.

## Context

`mr:write` is too broad to be useful: merging is a distinct risk from creating,
updating or closing a merge request. ADR 0008 introduced `mr:rebase` as a
least-privilege capability and left `mr:write` reserved for "a future
create/update/merge tool"; this ADR resolves the merge part of that reservation.
Silently broadening `mr:write` to permit merges would also widen existing
configurations without an explicit operator decision.

## Decision

- Add `mr:merge` (merge a merge request) as a first-class capability, separate
  from the reserved `mr:write`.
- `merge_merge_request` (`mr:merge`) uses
  `PUT /projects/:id/merge_requests/:iid/merge` via the GitLab client's
  `AcceptMergeRequest`. It requires the **Merge Request: Update** permission and
  a role allowed to merge (typically **Developer**).
- The provider method is
  `MergeMergeRequest(ctx context.Context, repo string, number int64) (*provider.MergeRequest, error)`.
  The merge is **synchronous** (HTTP 200 with the merge request body); the
  returned merge request is surfaced through the existing `get_merge_request`
  JSON shape. Labels are never returned in any output.
- **No merge options** (no squash, `remove_source_branch`, merge commit message)
  and **no SHA pinning** in v1, consistent with `RebaseMergeRequest`. The tool
  input is only `{provider, repo, number}`.
- `mr:merge` accepts MR **tag filters** (labels), evaluated like other MR
  operations after fetching MR metadata. A metadata-fetch failure fails closed.
- Under the `.noai` default-deny overlay (ADR 0003) `mr:merge` is denied on a
  `.noai` repository unless the grant is exempted with `noai: allow`. There is no
  hardcoded merge-specific marker logic; being listed in `knownCapabilities` is
  sufficient.
- `mr:write` remains reserved for create/update/close only.

## Consequences

- An operator can grant merge without granting rebase, diff review or comment,
  and existing `mr:write` grants do not silently gain merge rights.
- Merge adds a write path; it is authorized and tag-filtered like any other MR
  operation. Forbidden errors name the required permission; an invalid state
  (already merged/closed, draft, pending pipeline, conflicts) maps to
  "the merge request is not in a mergeable state".
- **Accepted TOCTOU limitation:** the metadata fetch used for tag evaluation and
  the merge itself are separate API calls. Labels or state can change in
  between; v1 does not pin the merge to the fetched SHA and does not re-check
  before merging.
- The merge is not idempotent. After an ambiguous failure a caller should check
  `get_merge_request` for `state: merged` before retrying.
