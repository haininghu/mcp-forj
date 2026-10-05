# list_merge_request_notes

## Purpose

List the comments (notes) on a merge request.

## Inputs

| Field    | Type  | Required | Notes                         |
|----------|-------|----------|-------------------------------|
| provider | string| yes      | Logical provider name.        |
| repo     | string| yes      | Canonical `namespace/project`.|
| number   | int64 | yes      | Positive MR number (`iid`).   |

## Required capability

`mr:read`.

## GitLab endpoint(s)

`GET /projects/:id/merge_requests/:iid/notes`

Fine-grained permissions: **Work Item: Read** (notes) and **Merge Request: Read**
(the metadata fetch used for tag evaluation).

## Authorization

1. Resolve the provider.
2. Policy-only pre-check: `Guard.AuthorizeRepoCapability(mr:read)`.
3. Fetch the MR via `GetMergeRequest` for tag evaluation; a fetch failure denies.
4. `Guard.AuthorizeWithTags(mr:read, {labels})`.
5. List notes and map them.

`.noai` does not apply.

## Behavior / limits

- Notes are returned with `id, body, author, created_at`; `body` is capped at 64 KiB.
- At most 100 notes; `truncated` is set when more remain.
- Labels are never returned.

## Errors

- `not found (HTTP 404)`;
  `forbidden: reading merge request notes needs the Work Item: Read permission (HTTP 403)`;
  generic provider failure with `(HTTP <n>)`.
