# list_configured_rules

## Purpose

Return the configured access rules and their **configured** capabilities, so an
agent can discover the policy without any provider call.

## Inputs

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| —     | —    | —        | No arguments. |

## Required capability

None.

## GitLab endpoint(s)

None — config only.

## Authorization

None. This tool exposes the policy intentionally (single trusted agent model). It
does not reveal `.noai` state, labels, topics or tokens.

## Behavior / limits

- Returns each configured rule, sorted by provider name; rule order within a
  provider is preserved.
- Output shape per rule:
  `{provider, repositories, effect, configured_capabilities}` where each capability
  is `{name, require?, exclude?, paths?, noai_exempt?}` (`paths` = `{include?, exclude?}`).
- "Configured" means exactly what the config grants; the `.noai` default-deny overlay
  (unless `noai_exempt`) and tag/path filters may further restrict actual access at
  operation time.

## Errors

None beyond an internal encoding failure.
