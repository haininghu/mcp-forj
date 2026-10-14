# 0012. Firewall-style rule matching

## Status

Accepted. Supersedes the rule-matching parts of ADR 0002 and the deny-rule restriction of AGENTS.md
invariant 3. Supersedes the "filters on `deny` rules are rejected" note in ADR 0006 and the
"rejected on `deny` rules" note in ADR 0003. Ported from mcp-keel ADR 0011.

## Context

mcp-forj evaluated a rule by matching the **container** (a repository) and then looking up the
capability; a `deny` rule was scope-only and had to **not** list capabilities. Two needs could not be
expressed:

- "forbid every operation matching a predicate" (a capability, a tag/topic, a path, a branch);
- a robust, non-repetitive exclusion that a broader `allow` cannot defeat.

An `exclude` filter on an `allow` rule is not enough: in a fall-through model a later, broader
`allow` overrides it, and the same exclude would have to be repeated on every capability.

## Decision

Adopt a **firewall-style, symmetric** rule model.

- A rule has a **scope** (`repositories`), an **effect** (`allow` / `deny`) and a list of
  **capabilities**, each optionally with a **filter** (`require`/`exclude` tags, `paths`,
  `branches`, `noai`).
- A rule **applies** iff the scope matches AND one of:
  - the rule is a `deny` rule with no capabilities (it denies every capability in its scope); or
  - at least one capability entry matches the requested capability AND all of that entry's filters
    pass.
- Rules are evaluated in order; the **first applicable rule wins**. `allow` grants, `deny` denies.
  If no rule applies, access is denied (deny-by-default).
- A `deny` rule **may list capabilities**; with none it denies every capability in its scope.
- The boundary between fall-through and fail-closed is deliberate:
  - a filter that is **evaluated** against known input and does not match means the rule **does not
    apply** (fall-through), not a hard deny. Exclusions are therefore written as `deny` rules placed
    **before** broader `allow` rules.
  - a filter whose **input is unavailable** fails closed (a hard deny): unknown tags/topics, an
    empty path for an active `paths` filter, and, in a branch-context evaluation, the structural
    branch guards (a named branch without a `branches` filter, or an active `branches` filter
    without a branch). Fail-closed is never defeated by a later, broader rule.
- `noai` remains a **capability-level default-deny overlay**, unchanged: on a `.noai` repository
  every capability is denied unless the matched grant is exempted with `noai: allow`. `noai: allow`
  on a `deny` rule is rejected at config load and in `policy.Build` (a deny refuses regardless of the
  marker). Because a per-item tag filter can fall through to a non-exempt grant, the repository-level
  overlay in `list_merge_requests` is conservative: the marker is skipped only when **every** allow
  grant of the capability is exempt (`Policy.NoAIExemptAll`).
- Static (literal) repositories are visible when the first scope-matching rule exposes the listing:
  an `allow` rule exposes it (no `repo:list` grant is needed), while a `deny` rule hides it when it
  has no capabilities or lists `repo:list`. A `deny` rule that lists other capabilities falls
  through, so a later `allow` still exposes the repository; with no exposing rule at all the
  repository is not listed.
- The git proxy uses the same `Guard`; push target branches are authorized per branch
  (`Guard.AuthorizeBranch`). An evaluated branch-filter miss falls through to a later grant, while a
  `repo:write` grant without a `branches` filter remains a hard fail-closed deny for a named branch.

### Divergences from mcp-keel (deliberate)

- **`Decision.Matched` means "a rule's repository scope matched"**, not "a rule applied". mcp-forj
  uses it to map a scope match to `ErrDenied` and no match to `ErrUnknownRepository`; changing it
  would alter the client-visible existence-hiding behavior of every tool. The firewall model
  changes *which* rule wins, not the error taxonomy.
- **No wildcard capability or verb.** mcp-forj capabilities are opaque, validated strings with no
  `*` form, so the mcp-keel wildcard rules (and their effect-dependence) are not ported.
- **Unavailable filter information fails closed** (see above). mcp-keel evaluates labels that always
  ship with the object; mcp-forj fetches tags/topics separately, so "unknown" is a real state.

### Rejected alternative

An **asymmetric** model (an `allow` rule is authoritative for the capabilities it lists, while a
`deny` rule is conditional) was rejected: it cannot express "forbid every operation matching a
predicate" as a first-class rule, it forces exclusions to be repeated per capability, and it makes a
broad `allow` able to defeat a conditional `deny` in a way that is hard to audit.

## Consequences

- "Forbid every operation matching X" is a first-class `deny` rule; exclusions are DRY.
- **Rule order is security-critical**: a broad `allow` above a specific `deny` hides the denial
  (first-match-wins). This is documented; there is no config-load shadowing warning in v1.
- This is a **breaking change** for existing configurations (see Migration).
- A `deny` rule carrying a `paths` or `branches` filter denies in path-/branch-less contexts too
  (for example git fetch/push discovery, where there is no path/branch), fail-closed. This is the
  safe direction and matches invariant 8 ("an active `paths` filter means no git access").
- `Policy.Build` and `config` reject `noai: allow` on a `deny` rule.
- The policy-only pre-check `Policy.EvaluatePreCheck` ignores filters and denies early only on an
  unconditional deny (a `deny` without capabilities, or a `deny` listing the capability with no
  filter); a filtered `deny` is not conclusive, so the caller fetches the metadata and evaluates the
  filters.

## Migration

Existing configs keep working but the **allow semantics change**:

1. **An unlisted capability is now fall-through.** Previously an `allow` rule that matched a
   repository but omitted a capability denied it. Now evaluation continues; a later rule may grant
   it, otherwise deny-by-default still denies. To keep a capability denied, no later `allow` may
   grant it.
2. **A failed filter is now fall-through.** Previously a tag/path/branch filter miss denied hard.
   Now a later, broader `allow` can grant the operation. A filter on an `allow` rule is therefore a
   *preference*, not a block. To exclude reliably, put a `deny` rule (optionally with the same
   filter) **before** the broad `allow`.
3. **Shadowing.** A broad `allow` above a specific `deny` hides the denial. Put specific `deny` rules
   first. There is no automatic warning.
4. **`deny` rules with capabilities are now valid** and capability-scoped; a `deny` without
   capabilities still denies everything in its scope. `noai: allow` on a `deny` rule is rejected.
