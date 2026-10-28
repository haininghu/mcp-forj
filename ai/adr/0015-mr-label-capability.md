# 0015. MR label capability

## Status

Accepted.

## Context

Labels are authorization tags for `mr:*` capabilities: grants use `require`/`exclude` to gate
comments, rebase, merge and reads on the merge request's current labels. Until now no MCP tool could
change labels, so an agent had to ask a human or use a broad token outside the policy. Broadening
`mr:write` to permit label changes would silently widen existing configurations and conflate label
mutation with create/update/close of core merge request fields.

Label mutation is security-sensitive in a second way: an agent that can add a label could grant
itself (or remove) the very tag that authorizes another capability. A least-privilege grant must
therefore restrict both which names may change and the resulting label state.

## Decision

- Add `mr:label` as a first-class capability, separate from `mr:write` (which remains reserved for
  create/update/close). The tool is `update_merge_request_labels`.
- The operation is **add/remove only**, never a full label replacement: the GitLab client uses
  `add_labels`/`remove_labels`, never the `labels` field. At least one of add/remove is required.
- `mr:label` accepts the normal MR tag filters (`require`/`exclude` on the current labels) and the
  `.noai` default-deny overlay like the other MR capabilities.
- Add a new `labels: {include, exclude}` argument filter, accepted only on `mr:label`. It is
  evaluated against **every requested label**, not the MR's current labels. Names are exact and
  case-sensitive (no globs), `exclude` wins over `include`, an evaluated miss falls through, and an
  active filter with no requested labels fails closed.
- After fetching the current labels and authorizing the request, the handler computes the
  post-mutation set (`current - remove + add`) and re-evaluates it with the grant's tag filter. This
  prevents creating a label state the grant would not authorize. If the current labels are unknown,
  the post-state check is skipped because an active tag filter already failed closed.
- The tool returns only `{provider, repo, number, status}`. It never returns merge request metadata or
  label names: `mr:label` does not imply `mr:read`, and labels are provider content (invariant 9).
- The GitLab call needs **Merge Request: Read** for the metadata/label fetch and **Merge Request:
  Update** for the change. Adding an unknown label may create a project label, depending on token
  permissions; operators must scope `mr:label` accordingly.
- Push options are unaffected: `merge_request.*` still needs `mr:write` (and `mr:merge` for
  auto-merge). Label mutation is not reachable through git push options.

## Consequences

- Operators can grant label changes without granting merge, rebase, comment or write access, and
  existing `mr:write` grants do not silently gain label rights.
- A mis-scoped `mr:label` grant could still add a label that satisfies a later broad `mr:*` grant.
  The `labels` filter and the post-state re-check bound this, but operators should scope `mr:label` as
  narrowly as the tag filters it could satisfy and prefer deny rules before broad allows.
- The post-state re-check runs the `.noai` marker check a second time (acceptable, fail-closed).
- **Accepted TOCTOU limitation:** the pre-state is fetched, then the `PUT` is issued. A concurrent
  label change is not re-validated against the fetched pre-state; the post-state check is computed
  from the pre-state.
- The provider interface grows one method (`UpdateMergeRequestLabels`); every implementation and test
  stub must provide it.
