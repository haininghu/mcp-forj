# list_merge_requests

## Purpose

List merge-request **metadata** (no diffs) for a repository, enforcing an active
`mr:read` tag filter client-side.

## Inputs

| Field    | Type   | Required | Notes                                              |
|----------|--------|----------|----------------------------------------------------|
| provider | string | yes      | Logical provider name.                             |
| repo     | string | yes      | Canonical `namespace/project`.                     |
| state    | string | no       | Provider-side state filter (e.g. `opened`).        |
| limit    | int    | no       | Max results; default 100, capped 100.              |

## Required capability

`mr:read`.

## GitLab endpoint(s)

`GET /projects/:id/merge_requests`

Fine-grained permission: **Merge Request: Read**.

## Authorization

1. Resolve the provider.
2. Policy-only pre-check: `Guard.AuthorizeRepoCapability(mr:read)` (tags ignored).
3. Fetch the list.
4. Evaluate each MR with `Guard.EvaluateWithTags(mr:read, {labels})` using the
   labels returned by the list endpoint. No metadata fetch is needed; the list
   response carries `labels`.

`.noai` default-deny applies: listing is denied on a `.noai` repository unless the
`mr:read` grant is exempted with `noai: allow`. The marker is checked once per
request, after the policy pre-check and before the provider list call.

## Behavior / limits

- Active tag filters are enforced per MR; the GitLab list endpoint returns `labels`,
  so a `require: [renovate]` filter returns matching MRs (it does not fail closed).
- Non-matching MRs are skipped; MRs whose labels are unknown (`LabelsKnown=false`) are
  omitted (fail-closed). The count is logged server-side only and not returned, so the
  result does not reveal how many merge requests were hidden.
- At most 100 results; `truncated` is set when the fetch window or the cap is hit.
- Labels are never returned.

## Errors

- `not found (HTTP 404)`; `forbidden: reading merge requests needs the Merge
  Request: Read permission (HTTP 403)`; generic provider failure with `(HTTP <n>)`.
