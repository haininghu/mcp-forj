# 0002. Deny-by-default capability policy

## Status

Accepted.

## Context

The server exposes tools that read source code, comments on merge requests and
rebases branches. A misconfiguration must not silently grant access. We need an
authorization model that is simple enough to audit by reading the config and that
defaults to "no access" when the config is incomplete.

## Decision

Authorize with a per-provider, ordered list of **capability** rules:

- Capabilities are opaque strings validated at config load:
  `repo:list`, `repo:read`, `mr:read`, `mr:diff`, `mr:comment`, `mr:rebase`
  (plus reserved `mr:write`, `repo:write`).
- Rules match `repositories` glob patterns (doublestar) against the canonical
  `namespace/project` path.
- Rules are evaluated **in order; first match wins**. Put specific rules before
  broad ones.
- A matched `deny` rule denies the repository outright. A `deny` rule must **not**
  list `capabilities`.
- A matched `allow` rule grants only the capabilities it lists; anything else is
  denied. If no rule matches, access is denied.
- There is no configurable `default_effect` or `fail_mode`: deny-by-default and
  fail-closed are hardcoded.

The engine lives in `internal/policy` (`Policy`, `Build`, `Evaluate`); config
validation lives in `internal/config`.

## Consequences

- Reading the ordered config is enough to know what is granted; there is no hidden
  default allow.
- A rule that matches but omits a capability denies it, so capability lists must be
  explicit.
- Reserving unused capabilities keeps the vocabulary stable for future tools.
- Operators must order rules carefully; a broad `allow` above a specific `deny`
  would hide the denial (first match wins).
