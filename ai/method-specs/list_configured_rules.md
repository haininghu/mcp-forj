# list_configured_rules

## Purpose

Return the configured access rules and their **configured** capabilities, so an
agent can discover the policy without any provider call.

## Inputs

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| —     | —    | —        | No arguments. |

## Required capability

`policy:read` (provider-scoped; granted by any allow rule of the provider).

## GitLab endpoint(s)

None — config only.

## Authorization

Per provider, `Guard.AuthorizePolicyRead` requires at least one allow rule to grant
`policy:read` (`GrantsAnywhere`). Providers without it are omitted from the output, so
the policy is not disclosed by default. The tool never reveals `.noai` state, runtime
labels/topics or tokens.

## Behavior / limits

- Returns each configured rule for providers that grant `policy:read`, sorted by
  provider name; rule order within a provider is preserved. Providers without the
  capability contribute nothing.
- Output shape per rule:
  `{provider, repositories, effect, configured_capabilities}` where each capability
  is `{name, require?, exclude?, paths?, branches?, labels?, noai_exempt?}` (`paths`,
  `branches` and `labels` = `{include?, exclude?}`). `labels` appears only on `mr:label`
  and lists the exact label names a request may change.
- `configured_capabilities` may also be present on a `deny` rule: a `deny` rule may list the
  capabilities and filters it refuses (ADR 0012); a `deny` rule without capabilities denies every
  capability in its scope and has an empty list.
- "Configured" means exactly what the config states; the `.noai` default-deny overlay
  (unless `noai_exempt`) and tag/path filters may further restrict actual access at
  operation time.

## Errors

None beyond an internal encoding failure.
