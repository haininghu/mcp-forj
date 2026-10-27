# 0014. Optional provider with a single configured provider

## Status

Accepted.

## Context

Every capability-gated tool takes a `provider` argument naming the logical provider. Agents often
have to infer it, and with a single configured provider there is nothing to choose. Requiring the
argument adds a guessing step that can fail even though the answer is unambiguous.

## Decision

- `provider` is optional in every tool input schema.
- At call time an empty `provider` resolves to the single registered provider. The resolved name is
  used for policy checks, provider calls, logs and output.
- With zero or multiple registered providers an empty `provider` is an error (`provider is required
  when multiple providers are configured`); an unknown name remains `unknown provider "<p>"`.
- Provider selection is not an authorization boundary: policy is keyed by the resolved provider name,
  so defaulting cannot widen access.

## Consequences

- Agents can omit `provider` in single-provider deployments; multi-provider deployments are unchanged.
- The input schema no longer lists `provider` as required; clients must tolerate an optional field.
- The default is resolved from the runtime registry, not from the raw config, so every configured
  provider counts, even if no rule grants anything for it.
