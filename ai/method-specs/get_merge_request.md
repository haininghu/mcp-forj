# get_merge_request

## Purpose

Fetch a single merge request by number, including rebase/merge status fields.

## Inputs

| Field    | Type  | Required | Notes                       |
|----------|-------|----------|-----------------------------|
| provider | string| yes      | Logical provider name.      |
| repo     | string| yes      | Canonical `namespace/project`.|
| number   | int64 | yes      | Positive MR number (`iid`). |

## Required capability

`mr:read`.

## GitLab endpoint(s)

`GET /projects/:id/merge_requests/:iid`

Fine-grained permission: **Merge Request: Read**.

## Authorization

1. Resolve the provider.
2. Policy-only pre-check: `Guard.AuthorizeRepoCapability(mr:read)`.
3. Fetch the MR via `GetMergeRequest` (metadata needed to evaluate tags). A fetch
   failure denies (fail-closed).
4. `Guard.AuthorizeWithTags(mr:read, {labels})`.

`.noai` default-deny applies: the fetch is denied on a `.noai` repository unless the
`mr:read` grant is exempted with `noai: allow`.

## Behavior / limits

- Returns metadata: `number, title, description, state, author, source_branch,
  target_branch, web_url, rebase_in_progress, merge_error, has_conflicts,
  detailed_merge_status`.
- `description` is capped at 64 KiB (`\n[truncated]` marker appended).
- Labels are never returned.

## Errors

- `not found (HTTP 404)`;
  `forbidden: reading merge requests needs the Merge Request: Read permission (HTTP 403)`;
  generic provider failure with `(HTTP <n>)`.
