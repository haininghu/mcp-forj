# 0006. List truncation was never reported and other provider/handler gaps

## Symptom

`list_merge_requests` and `list_merge_request_notes` returned at most 100 entries with
`truncated: false` even when more existed. An invalid-state error on a non-rebase tool
reported a rebase-specific message. `read_file` copied an oversized file in full before
truncating.

## Impact

- Silent incompleteness: a client could not tell there were more MRs or notes.
- Misleading error text ("not in a readable state") for `read_file`,
  `add_merge_request_note`, `list_merge_requests`, etc.
- `read_file` peak memory was about twice the file size for a large file (DoS surface).

## Root cause

- The handlers requested `limit+1` to detect truncation, but the GitLab provider issued a
  **single** request with `per_page = limit+1`; GitLab clamps `per_page` to 100, so the
  extra item was never returned (`ListMergeRequests`/`ListMergeRequestNotes`).
- `mapProviderError` hardcoded the rebase message for every `ErrInvalidState`
  (HTTP 400/405/409).
- `readFile` converted the whole `[]byte` to a string before slicing to the cap.

## Fix

- Paginate `ListMergeRequests` and `ListMergeRequestNotes` (follow `X-Next-Page`), so
  `limit+1` can exceed one page and truncation is detected.
- `mapProviderErrorState` takes an operation-specific invalid-state message; only
  `rebase_merge_request` passes the rebase wording, everything else gets a generic
  "not in a state that permits this operation".
- `readFile` bounds the bytes (`safeBytePrefix`) before converting to a string; added
  `validateNoteBody` to reject control characters in note bodies.

## Lesson

A truncation signal must be verified against the provider's real page-size limits, not
assumed. Error messages belong to the operation that failed. Bound data before copying
it, not after.
