# 0004. MR note endpoints require the Work Item permission, not Merge Request

## Symptom

Creating or listing merge-request notes returned `403 Forbidden` even though the
fine-grained token had Merge Request permissions. The comment/list failure looked
like a policy or repository-scope problem.

## Impact

`add_merge_request_note` and `list_merge_request_notes` were unusable with tokens
provisioned using only Merge Request permissions. The generic forbidden message
("write scope, merge-request Update permission, or sufficient project role") sent
operators down the wrong path.

## Root Cause

GitLab maps the noteable notes endpoints
(`GET/POST /projects/:id/merge_requests/:iid/notes`) to the fine-grained **Work
Item** resource (Read/Create), not the Merge Request resource. A token with only
Merge Request permissions therefore gets 403 from the notes endpoints. The server's
fixed forbidden message did not mention Work Item.

## Fix

- Kept the endpoints; corrected the guidance and diagnostics.
- `mapProviderError` now takes an operation-specific hint. Note operations pass:
  - `add_merge_request_note`: `Work Item: Create` (+ project in scope)
  - `list_merge_request_notes`: `Work Item: Read`
  Other operations name their own resource permission (see ADR 0009).
- README's GitLab token permission table lists Work Item: Read/Create (plus Merge
  Request: Read for the metadata fetch).

## Lesson

For fine-grained GitLab tokens, the resource permission is per endpoint, and note
operations belong to Work Item. When surfacing 403s, name the specific permission
the failed call needed, not a generic list. See ADR 0009.
