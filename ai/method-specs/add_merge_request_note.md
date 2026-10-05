# add_merge_request_note

## Purpose

Create a comment (note) on a merge request.

## Inputs

| Field    | Type   | Required | Notes                                |
|----------|--------|----------|--------------------------------------|
| provider | string | yes      | Logical provider name.               |
| repo     | string | yes      | Canonical `namespace/project`.       |
| number   | int64  | yes      | Positive MR number (`iid`).          |
| body     | string | yes      | Non-empty, at most 64 KiB.           |

## Required capability

`mr:comment` (independent of `mr:read`; accepts MR tag filters).

## GitLab endpoint(s)

`POST /projects/:id/merge_requests/:iid/notes`

Fine-grained permissions: **Work Item: Create** (note), plus **Merge Request: Read**
for the metadata fetch used for tag evaluation.

## Authorization

1. Resolve the provider.
2. Policy-only pre-check: `Guard.AuthorizeRepoCapability(mr:comment)`.
3. Fetch the MR via `GetMergeRequest` for tag evaluation; a fetch failure denies the
   post (fail-closed).
4. `Guard.AuthorizeWithTags(mr:comment, {labels})`.
5. Post the note and map the result.

`.noai` does not apply.

## Behavior / limits

- `body` must be non-empty after trimming and at most 64 KiB.
- Returns the created note: `{id, body, author, created_at}`; `body` capped at 64 KiB.
- Labels are never returned.

## Errors

- `not found (HTTP 404)`;
  `forbidden: writing a merge request comment needs the Work Item: Create permission
  (and the project in scope) (HTTP 403)`;
  generic provider failure with `(HTTP <n>)`.
