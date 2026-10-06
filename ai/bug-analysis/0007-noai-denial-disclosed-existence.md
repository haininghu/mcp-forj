# 0007. The `.noai` denial message disclosed repository existence

## Symptom

Requesting an operation on a `.noai` repository returned
`repository "xxx" is marked .noai and is off limits`. Any other denial or an unknown
repository produced a different message.

## Impact

The message was an **existence oracle**. Because `ErrNoAI` is only produced after the
policy has matched and granted the capability, the error confirmed that the repository
exists (its `.noai` marker was read) and that a rule covers it. An agent could enumerate
repositories by probing candidates and distinguishing "unknown repository", "access
denied" and "marked .noai".

## Root cause

`mapAuthError` mapped `policy.ErrNoAI` to a marker-specific message
(`server.go`), while the guard returned distinct sentinels for unknown repository, denied
capability and marker. The three responses were distinguishable at the tool boundary.

## Fix

`mapAuthError` now maps `policy.ErrNoAI` to the **same response as
`policy.ErrUnknownRepository`** ("unknown repository … for provider …"). The distinct
sentinel is kept internally for logging and tests; the guard still logs the precise
reason server-side. The marker is thus not disclosed and the repository appears as if it
did not exist. See ADR 0003.

## Lesson

Guardrails must not leak *why* they denied when that reason reveals metadata. Deny-by-
default means an unauthorized subject should not be able to tell "does not exist" from
"exists but is off limits". Keep the precise reason in server logs, not in the client
response.
