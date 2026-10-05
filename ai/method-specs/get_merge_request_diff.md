# get_merge_request_diff

## Purpose

Fetch the file diffs of a merge request.

## Inputs

| Field    | Type  | Required | Notes                       |
|----------|-------|----------|-----------------------------|
| provider | string| yes      | Logical provider name.      |
| repo     | string| yes      | Canonical `namespace/project`.|
| number   | int64 | yes      | Positive MR number (`iid`). |

## Required capability

`mr:diff` (separately grantable from `mr:read`; accepts MR tag filters).

## GitLab endpoint(s)

`GET /projects/:id/merge_requests/:iid/diffs`

Fine-grained permission: **Merge Request: Read**.

## Authorization

1. Resolve the provider.
2. Policy-only pre-check: `Guard.AuthorizeRepoCapability(mr:diff)`.
3. Fetch the MR via `GetMergeRequest` for tag evaluation; a fetch failure denies
   (fail-closed).
4. `Guard.AuthorizeWithTags(mr:diff, {labels})`.
5. List diffs (paginated) and map them.

`.noai` does **not** apply: diffs are merge-request metadata, not a
repository-content read.

## Behavior / limits

- Output: `{number, files, truncated}` with per file
  `{old_path, new_path, new_file, renamed_file, deleted_file, generated_file,
  too_large, diff, truncated}`.
- Max 100 files; each diff capped at 128 KiB (marker appended, file `truncated`
  set); total diff budget 512 KiB, after which collection stops and the top-level
  `truncated` is set.
- Labels are never returned.

## Errors

- `not found (HTTP 404)`;
  `forbidden: reading merge request diffs needs the Merge Request: Read permission (HTTP 403)`;
  generic provider failure with `(HTTP <n>)`.
