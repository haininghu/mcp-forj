# merge_merge_request

## Purpose

Merge a merge request and return the merged merge request.

## Inputs

| Field    | Type   | Required | Notes                          |
|----------|--------|----------|--------------------------------|
| provider | string | yes      | Logical provider name.         |
| repo     | string | yes      | Canonical `namespace/project`. |
| number   | int64  | yes      | Positive MR number (`iid`).    |

## Required capability

`mr:merge` (accepts MR tag filters).

## GitLab endpoint(s)

`PUT /projects/:id/merge_requests/:iid/merge`

Fine-grained permission: **Merge Request: Update**, plus a project role allowed to
merge (typically Developer). GitLab returns `200 OK` with the merged merge request.

## Authorization

1. Resolve the provider.
2. Policy-only pre-check: `Guard.AuthorizeRepoCapability(mr:merge)`.
3. Fetch the MR via `GetMergeRequest` for tag evaluation; a fetch failure denies the
   merge (fail-closed).
4. `Guard.AuthorizeWithTags(mr:merge, {labels})`.
5. Merge the MR.

`.noai` default-deny applies: the merge is denied on a `.noai` repository unless
the `mr:merge` grant is exempted with `noai: allow`.

## Behavior / limits

- Synchronous: returns the merged merge request in the `get_merge_request` JSON
  shape (`number`, `title`, `description`, `state`, `author`, `source_branch`,
  `target_branch`, `web_url`, `rebase_in_progress`, `merge_error`,
  `has_conflicts`, `detailed_merge_status`). Labels are never returned.
- No merge options (squash, `remove_source_branch`, merge commit message) and no
  SHA pinning in v1.
- Not idempotent: after an ambiguous failure, check `get_merge_request` for
  `state: merged` before retrying.
- Not mergeable (e.g. 400/405/409: already merged/closed, draft, pending pipeline,
  conflicts) maps to `the merge request is not in a mergeable state`.

## Errors

- `not found (HTTP 404)`;
  `forbidden: merging needs the Merge Request: Update permission and a role allowed
  to merge (typically Developer) (HTTP 403)`;
  invalid state; generic provider failure with `(HTTP <n>)`.
