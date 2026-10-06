# rebase_merge_request

## Purpose

Trigger an asynchronous rebase of a merge request's source branch onto its target
branch.

## Inputs

| Field    | Type  | Required | Notes                       |
|----------|-------|----------|-----------------------------|
| provider | string| yes      | Logical provider name.      |
| repo     | string| yes      | Canonical `namespace/project`.|
| number   | int64 | yes      | Positive MR number (`iid`). |

## Required capability

`mr:rebase` (accepts MR tag filters).

## GitLab endpoint(s)

`PUT /projects/:id/merge_requests/:iid/rebase`

Fine-grained permission: **Merge Request: Update**, plus a project role allowed to
push to the source branch (at least Developer). GitLab returns `202 Accepted`.

## Authorization

1. Resolve the provider.
2. Policy-only pre-check: `Guard.AuthorizeRepoCapability(mr:rebase)`.
3. Fetch the MR via `GetMergeRequest` for tag evaluation; a fetch failure denies the
   rebase (fail-closed).
4. `Guard.AuthorizeWithTags(mr:rebase, {labels})`.
5. Request the rebase.

`.noai` default-deny applies: the rebase is denied on a `.noai` repository unless
the `mr:rebase` grant is exempted with `noai: allow`.

## Behavior / limits

- Asynchronous: the result is `{provider, repo, number, status: "rebase requested"}`.
  The outcome appears later via `get_merge_request` (`rebase_in_progress`,
  `merge_error`, `has_conflicts`, `detailed_merge_status`).
- Not rebaseable (e.g. 400/405/409) maps to `the merge request is not in a rebaseable
  state`.

## Errors

- `not found (HTTP 404)`;
  `forbidden: rebasing needs the Merge Request: Update permission and a role allowed
  to push to the source branch (HTTP 403)`;
  invalid state; generic provider failure with `(HTTP <n>)`.
