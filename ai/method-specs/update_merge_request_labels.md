# update_merge_request_labels

## Purpose

Add and/or remove labels on a merge request without replacing its full label set.

## Inputs

| Field    | Type     | Required | Notes                                                        |
|----------|----------|----------|--------------------------------------------------------------|
| provider | string   | no¹      | Logical provider name.                                       |
| repo     | string   | yes      | Repository path (namespace/project).                         |
| number   | int64    | yes      | Merge request number (> 0).                                  |
| add      | string[] | no²      | Labels to add.                                               |
| remove   | string[] | no²      | Labels to remove.                                            |

¹ Optional when exactly one provider is configured.
² At least one of `add` or `remove` is required. At most 50 labels per call. Names are exact,
case-sensitive, non-empty, unpadded, at most 255 runes, and contain no control characters or commas.
Case-insensitive duplicates are collapsed (first spelling wins); the same label may not be in both
lists.

## Required capability

`mr:label` (separate from `mr:write`).

## GitLab endpoint(s)

`PUT /projects/:id/merge_requests/:iid` with `add_labels` and/or `remove_labels`. The full-replace
`labels` field is never used.

## Authorization

1. `Guard.AuthorizeRepoCapability(mr:label)` (policy-only pre-check).
2. `GetMergeRequest` for the current labels; a fetch failure denies (fail-closed).
3. `Guard.AuthorizeLabels(mr:label, tags=current labels, labels=every requested label)`. This applies
   the grant's `require`/`exclude` tag filters (unknown labels fail closed), the `labels` argument
   filter (exact names; exclude wins; an active filter with no requested labels fails closed) and the
   `.noai` overlay unless the grant is `noai`-exempt.
4. When the current labels are known, re-evaluate the post-mutation set (`current - remove + add`)
   with `Guard.AuthorizeWithTags`. A failure denies without calling the provider.

An evaluated filter miss falls through to a later applicable rule (firewall semantics, ADR 0012).

## Behavior / limits

- Mutates only labels; core merge request fields are untouched (`mr:write` is not implied).
- Returns `{provider, repo, number, status: "labels updated"}` only. It never returns merge request
  metadata or label names: `mr:label` does not imply `mr:read` (AGENTS invariant 9).
- Adding a label that does not exist may create it as a project label, depending on token permissions.
- Accepted TOCTOU: the pre-state is fetched, then the update is issued; a concurrent label change is
  not re-validated against the fetched pre-state.

## Errors

- Validation: empty/whitespace/padded/overlong/control/comma labels, duplicate across add/remove,
  more than 50 labels, or neither list.
- Not found: `not found (HTTP 404)`.
- Forbidden: `forbidden: updating merge request labels needs the Merge Request: Update permission
  (HTTP 403)`.
- Invalid state: `the merge request labels could not be updated (HTTP 400/405/409)`.
- Authorization and `.noai` denials are reported like the other tools; a `.noai` denial is
  indistinguishable from an unknown repository.
