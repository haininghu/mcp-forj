# 0005. Token and secret handling

## Status

Accepted.

## Context

The server needs a provider token to call GitLab. Tokens must never be written to
the repository, logged, or returned to the MCP client, and a configuration dump
must not leak them.

## Decision

- The provider config field is `token`. It accepts a **literal** secret or a
  **whole-string** `${NAME}` environment reference. Embedded mixing
  (`"prefix-${VAR}"`) is not supported, which lets literal secrets contain `$`.
- Resolution happens at config load (`config.Parse`): surrounding whitespace is
  trimmed; whitespace-only is rejected; a `${NAME}` reference is expanded from the
  environment and an unset/empty variable is a hard error. Error messages name the
  variable, never its value.
- The resolved value is held in `config.Secret`, whose `String`, `GoString`,
  `MarshalText` and `LogValue` all return `[REDACTED]`. The raw value is reachable
  only via `Secret.Value()`. JSON/YAML marshaling of a `Config` is therefore safe.
- The token is passed to the GitLab client at construction and never stored on the
  server, never logged, and never included in tool output.
- Literal secrets are discouraged: prefer `${NAME}` and keep `configs/config.yaml`
  git-ignored.

## Consequences

- `fmt`/`slog`/JSON/YAML dumps of the config are safe by construction.
- A missing environment variable fails startup loudly, before any request.
- Operators must ensure the token can read repository files (`Repository: Read`)
  for the `.noai` check; otherwise `read_file` fails closed (see ADR 0003/0009).
