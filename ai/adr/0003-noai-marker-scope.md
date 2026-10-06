# 0003. `.noai` marker scope and semantics

## Status

Accepted. Supersedes the previous revision of this ADR.

## Context

Some repositories must be off limits to the agent except where an operator explicitly allows
a specific operation. The marker is repository-side and its effect must be auditable from the
configuration. The previous design hardcoded a protected set (`repo:read`, `repo:write`), which
was inconsistent: it blocked reading (the most explicit, bounded operation) while allowing
`mr:diff` (a full content exposure) and `mr:rebase` (an unreviewed branch mutation). It also
prevented operators from granting a narrow, reviewable operation such as proposing a merge
request on a `.noai` repository.

## Decision

- `.noai` is a **capability-level default-deny overlay**: on a `.noai` repository every
  capability is denied unless the matching capability grant is explicitly exempted.
- The exemption is expressed per capability as `noai: allow`:

  ```yaml
  capabilities:
    - repo:list              # not exempt -> .noai repos are hidden from discovery
    - repo:read:
        noai: allow
    - repo:propose:
        noai: allow
    - mr:rebase              # not exempt -> denied on .noai repos
  ```

- Evaluation order: the ordered policy runs first (first-match-wins). If the matched grant is
  **not** `noai`-exempt, the marker is checked via `FileExists(".noai", ref=HEAD)`; a present
  marker denies with `ErrNoAI` and a check failure denies fail-closed. An exempt grant skips the
  marker check entirely.
- The hardcoded `policy.IsMarkerProtected` set is replaced by the config-driven `NoAIExempt`
  property of the matched grant.
- The marker is checked on the repository's **default branch** (GitLab: `ref=HEAD`). There is
  **no cache**: it is checked on every non-exempt operation.
- Repository listing:
  - Literal (explicitly configured) repositories are always listed, even if `.noai`, regardless
    of `repo:list`. A `deny` rule can still hide them.
  - Discovered repositories are marker-checked. `.noai` candidates are omitted and counted in
    `omitted` unless the `repo:list` grant is `noai`-exempt. A check failure omits the candidate
    (fail-closed).
- `list_configured_rules` is unaffected; it reports configuration only.
- Config validation accepts `noai` only on capability grants and only the value `allow`. It is
  rejected on `deny` rules (which carry no capabilities).

## Consequences

- The provider token must be able to read repository files on the default branch. Otherwise the
  fail-closed marker check denies every non-exempt capability on every repository, not only
  `read_file`.
- Every non-exempt operation costs one extra provider call, and discovery costs one per candidate.
  Accepted for correctness; no cache by design.
- This is a **breaking change**: `.noai` repositories lose merge-request and listing access unless
  the configuration adds `noai: allow` to the affected capabilities.
- `.noai` is an **integrity/governance control, not a confidentiality control**. It prevents
  non-exempt operations; it does not prevent content from reaching the agent through exempted
  reads (for example `mr:diff`, `mr:read`, `repo:read`). Confidentiality is enforced by the
  capability policy, not by the marker.
- Exempting `repo:propose` allows new-branch-plus-MR proposals on a `.noai` repository, which is
  reviewable by construction.

## History

- Previous revision: `.noai` protected only `repo:read` and `repo:write`; merge-request
  capabilities and repository listing were unaffected. That scope is superseded by this decision.
