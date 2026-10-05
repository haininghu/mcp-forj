# 0001. `list_merge_requests` failed closed under an `mr:read` label filter

## Symptom

With a rule such as `mr:read: {require: [renovate]}`, `list_merge_requests`
returned an access-denied error (or no MRs) even though matching merge requests
existed. The tool refused to list anything.

## Impact

Feature blocked: a legitimate, common grant ("list only renovate MRs") was
unusable. It also broke listing for any `mr:read` tag filter, not just `renovate`.

## Root cause

Two linked mistakes in the v1/v2 code:

- The server assumed the GitLab merge-request **list** endpoint did not return
  labels, so labels could not be evaluated for list results.
- `list_merge_requests` therefore authorized via `Guard.Authorize` with unknown
  tags. An active tag filter fails closed on unknown tags, so the whole call was
  denied whenever a filter was configured.

The assumption was wrong: the list endpoint returns `labels`, and the client's
`gitlab.BasicMergeRequest` exposes `Labels gitlab.Labels` (`json:"labels"`).

## Fix

- `internal/provider/gitlab/gitlab.go`: `mapBasicMergeRequest` maps `m.Labels` and
  sets `LabelsKnown = m.Labels != nil`, so list results carry labels (a JSON `[]`
  yields a known empty set; `null`/absent stays unknown).
- `internal/server/server.go`: `listMergeRequests` now pre-checks with
  `AuthorizeRepoCapability`, fetches the list, and evaluates each MR with
  `Guard.EvaluateWithTags` using its own labels. Non-matching or unknown-label MRs
  are skipped and counted in `omitted`.
- Output gains an `omitted` field.

## Lesson

Do not assume a provider omits data without verifying the client type. When a
filter depends on data a list endpoint *does* return, evaluate it client-side
rather than failing the whole request closed. See ADR 0008.
