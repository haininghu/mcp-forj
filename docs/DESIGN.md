# Design: `mcp-forj` — Policy-Governed MCP Server for Code Hosting Providers

Status: **Draft v0.2** (critic-reviewed; changes from v0.1 incorporated)
Author: orchestrator
Scope: first iteration (GitLab only; MR metadata + comments)

## Changelog vs. v0.1 (critic feedback incorporated)

- **B1** Split `mr:read` (metadata only) from a new `mr:diff` capability. Diffs are
  out of scope for v1, so a "view/comment only" grant cannot leak source code.
- **B2** Removed the `.noai` result cache entirely for v1. The marker is checked on
  every operation (fail-closed), eliminating the stale-cache exposure window.
- **B3** `list_repositories` now reports **configured** capabilities, not "effective"
  ones, and documents that `.noai` may further restrict access.
- **S1** Removed `default_effect` and `fail_mode` options. Deny-by-default and
  fail-closed are hardcoded.
- **S2** Documented the token-scope requirement for the `.noai` check.
- **S3** Added `github.com/bmatcuk/doublestar/v4` for `**` glob matching.
- **S4** Precise, ordered path-traversal validation spec.
- **S5** Concrete output limits and truncation behavior.
- **S6** Renamed the provider-neutral identifier from `iid` to `number`.

## 1. Problem Statement

I want a single MCP server that gives AI agents controlled access to several code
hosting providers. The first provider is GitLab (multiple instances), later GitHub
and an internal "Forjo" system.

Access must be governed by an explicit configuration: for each provider and
repository it defines **what an agent may do**. Some repositories may only be
inspected and commented on at the merge-request level, while reading source code or
changing anything is forbidden. Repositories that contain a `.noai` marker file are
**completely off limits** — no operation of any kind is allowed.

The first iteration must stay deliberately small so the direction can change later.

## 2. Goals

- One MCP server exposing tools for common repository/merge-request operations.
- **Deny-by-default** authorization, driven by a human-readable YAML config.
- Per-provider, per-repository **capability** grants.
- Hard, non-overridable block for repositories containing `.noai`.
- A clean provider abstraction so GitHub/Forjo can be added without touching policy
  or server code.
- Good tests and English documentation.
- Minimal, best-in-class dependencies.

## 3. Non-Goals (first iteration)

- No write operations beyond merge-request comments (no pushes, merges, branch or
  file mutations).
- No merge-request **diff** content (see `mr:diff` below). Metadata only.
- No OAuth / interactive login. Tokens are read from environment variables only.
- No HTTP/SSE transport; stdio transport only.
- No caching of remote data, including the `.noai` marker.
- No GitHub or Forjo implementation yet (interface only).
- No multi-user auth or per-client identity — the config describes one trusted agent
  context.

## 4. Architecture

```
                 MCP client (e.g. opencode)
                        │  stdio / JSON-RPC
                        ▼
        ┌──────────────────────────────────────┐
        │ cmd/mcp-forj  (main)                 │
        │  load config → build registry → run  │
        └───────────────┬──────────────────────┘
                        │
                        ▼
        ┌──────────────────────────────────────┐
        │ internal/server  (MCP tools)         │
        │  tool handlers, arg validation       │
        └───────────────┬──────────────────────┘
                        │ Authorize(provider, repo, capability)
                        ▼
        ┌──────────────────────────────────────┐
        │ internal/policy  (Guard / Engine)    │
        │  rule matching + .noai check         │
        └───────┬──────────────────────┬───────┘
                │                      │
                ▼                      ▼
     internal/provider          internal/policy/noai
     (interface + registry)     (marker check, fail-closed)
                │
                ▼
     internal/provider/gitlab
     (official GitLab client)
```

Every tool handler follows the same flow:

1. Resolve provider from the registry by name.
2. Call `Guard.Authorize(ctx, provider, repo, capability)`.
3. If denied, return a structured MCP error; never call the provider operation.
4. Otherwise perform the provider operation and map the result to MCP content.

## 5. Capability Model

Capabilities are opaque strings validated at config-load time (unknown capabilities
are rejected — fail closed).

| Capability   | Meaning                                                       |
|--------------|---------------------------------------------------------------|
| `repo:read`  | Read repository files / directory listings.                   |
| `mr:read`    | List and view merge request **metadata** and notes. No diffs. |
| `mr:diff`    | Read merge request diff content. *(reserved, not in v1)*      |
| `mr:comment` | Create comments/notes on merge requests.                      |
| `mr:write`   | Create, update, merge, or close merge requests. *(reserved)*  |
| `repo:write` | Modify repository content (branches, files, pushes). *(reserved)* |

Only `repo:read`, `mr:read`, and `mr:comment` are used by v1 tools. The remaining
capabilities are defined but unused so the config vocabulary is stable.

The example from the requirements maps to:
`allow: [mr:read, mr:comment]` — inspect and comment on MRs, but no `repo:read`,
`mr:diff`, `mr:write`, or `repo:write`.

**Note:** MR descriptions and notes may contain pasted code. This is inherent to
viewing MRs and is accepted; it is not the same as granting repository read access.

**Independence:** `mr:comment` does not imply `mr:read`. Posting a note does not
require reading the merge request, so a repository may grant comment-only access.

## 6. Configuration

YAML, path passed via `-config` (default `configs/config.yaml`), overridable by
`MCP_FORJ_CONFIG`. A committed `configs/config.example.yaml` documents the format.

```yaml
server:
  name: mcp-forj
  log_level: info            # debug|info|warn|error
  noai:
    marker_file: .noai       # repository marker that disables all access

providers:
  - name: gitlab-work        # logical name used by all tools
    type: gitlab             # provider factory key
    base_url: https://gitlab.example.com
    token_env: GITLAB_WORK_TOKEN   # secret is read from the environment
    request_timeout: 30s
    rules:
      - repositories: ["team/service-a", "team/service-b"]
        effect: allow
        capabilities: [mr:read, mr:comment]
      - repositories: ["team/*"]
        effect: allow
        capabilities: [mr:read]
      - repositories: ["legacy/**"]
        effect: deny
```

### Rule semantics

- `repositories` are glob patterns matched against the canonical
  `namespace/project` path. Matching uses `doublestar`: `*` matches within a path
  segment, `**` matches across segments.
- Rules are evaluated **in order; first match wins**. This makes overrides
  predictable (put specific rules before broad ones).
- If no rule matches, access is **denied** (hardcoded).
- A rule with `effect: deny` short-circuits to deny.
- A rule with `effect: allow` grants only the listed `capabilities`.
- `default_effect` and `fail_mode` do **not** exist: deny-by-default and fail-closed
  are not configurable.

### `.noai` guard

The `.noai` check is independent of and **senior to** all rules: if the marker file
exists in the repository, every capability is denied. It cannot be enabled or
disabled by rules. For v1 there is **no cache** — the marker is checked on every
operation. If the check fails for any reason, the operation is denied (fail-closed).

The marker is checked at the repository's **default branch** (HEAD), using the
server's own token via a dedicated provider call (`FileExists`). This is a
privileged internal call and is not exposed as a capability.

> **Operational requirement:** the provider token (`token_env`) must be able to
> read repository files at least on the default branch. A token scoped to
> merge-request access only will make the `.noai` check fail, which (by design)
> denies **every** operation on that provider. Document this clearly for operators.

## 7. Provider Interface

```go
type Repository struct {
    Provider string // provider name from config
    Path     string // canonical namespace/project
    WebURL   string
}

type MergeRequest struct {
    Number       int64 // provider-native number (GitLab iid, GitHub PR number, ...)
    Title        string
    Description  string
    State        string
    Author       string
    SourceBranch string
    TargetBranch string
    WebURL       string
}

type Note struct {
    ID        int64
    Body      string
    Author    string
    CreatedAt time.Time
}

type Provider interface {
    Name() string
    Type() string

    ListMergeRequests(ctx context.Context, repo string, opts ListOptions) ([]MergeRequest, error)
    GetMergeRequest(ctx context.Context, repo string, number int64) (*MergeRequest, error)
    ListMergeRequestNotes(ctx context.Context, repo string, number int64) ([]Note, error)
    AddMergeRequestNote(ctx context.Context, repo string, number int64, body string) (*Note, error)

    ReadFile(ctx context.Context, repo, path, ref string) ([]byte, error)
    FileExists(ctx context.Context, repo, path, ref string) (bool, error)
}
```

A factory maps `type` (e.g. `gitlab`) to a constructor, so new providers only need
to implement `Provider` and register a case. `number` is provider-neutral on
purpose: GitLab uses a project-scoped `iid`, GitHub a global PR number, Forjo a
global index; each provider maps its native identifier.

## 8. MCP Tools (first iteration)

| Tool                        | Capability   | Provider operation          |
|-----------------------------|--------------|-----------------------------|
| `list_repositories`         | none         | — (from config)             |
| `list_merge_requests`       | `mr:read`    | `ListMergeRequests`         |
| `get_merge_request`         | `mr:read`    | `GetMergeRequest`           |
| `list_merge_request_notes`  | `mr:read`    | `ListMergeRequestNotes`     |
| `add_merge_request_note`    | `mr:comment` | `AddMergeRequestNote`       |
| `read_file`                 | `repo:read`  | `ReadFile`                  |

`list_repositories` is config-only (no remote call, no secrets) and returns each
configured repository together with its **configured** capabilities. It deliberately
does not require a capability. It does **not** claim the capabilities are effective:
`.noai` is enforced per operation and may further restrict access. It does not
perform `.noai` checks (N privileged calls would be out of scope for v1) and must
not reveal `.noai` state.

Tool arguments are validated with explicit bounds:
- `provider` must name a configured provider.
- `repo` must match at least one configured rule for the provider. Error messages
  distinguish "unknown repository" (no rule matches) from "denied repository"
  (a matching rule denies or lacks the capability).
- `number` must be positive.
- `body` must be non-empty and within the note size limit.
- `path` must pass the ordered traversal checks in §9.

## 9. Security Considerations

- **Deny by default**, hardcoded at provider and rule level.
- **`.noai` is authoritative** and fail-closed; checked before every operation.
- **Secrets**: tokens come from environment variables, are never logged, and are
  never returned in tool output. Config values are redacted in logs.
- **Path safety** for `read_file` and any future path input. Checks run **before**
  normalization, in this order:
  1. reject if the path is empty;
  2. reject if `path.IsAbs(p)` or the path starts with `/` or `\`;
  3. reject if it contains a NUL or other control character;
  4. reject if any `/`- or `\`-separated segment is exactly `..` or `.`;
  5. reject if `path.Clean(p)` is absolute or starts with `../`;
  6. reject URL-encoded traversal (`%2e`, `%2f`, `%5c`, case-insensitive);
  7. only then use the cleaned path.
- **Output limits**: `read_file` is capped at **1 MiB**; note bodies are capped at
  **64 KiB**; MR descriptions are capped at **64 KiB**. Oversized values are
  truncated and a trailing `\n[truncated]` marker is appended. List results are
  capped (e.g. 100 entries) with an explicit indication that more exist.
- **Audit logging**: every authorization decision is logged with structured fields
  (`provider`, `repo`, `capability`, `decision`, `reason`). The `.noai` check result
  is logged as well. Logs never contain tokens or full request bodies.
- **Error hygiene**: provider errors are mapped to safe messages; raw HTTP bodies
  are not leaked to the MCP client.

## 10. Testing Strategy

- `internal/config`: parsing, defaults, unknown capability rejection, missing
  `token_env`, duplicate provider names, empty rules.
- `internal/policy`: rule precedence, `*` vs `**` glob matching, default deny,
  capability subset, deny override.
- `internal/policy/noai`: marker present/absent, provider error (always deny).
- `internal/server`: end-to-end tool calls against a **fake provider** using the
  SDK's in-memory transports; assert allow, capability denial, `.noai` denial, and
  argument validation.
- `internal/provider/gitlab`: mapping logic plus an `httptest`-based client test.

### Security-critical tests (mandatory)

1. `.noai` present → every tool denies, even with full capabilities.
2. `.noai` check error → deny (fail-closed).
3. `add_merge_request_note` without `mr:comment` → deny; `read_file` without
   `repo:read` → deny.
4. `mr:read` granted but `repo:read` denied → `read_file` denied, MR tools allowed
   (the exact requirement).
5. MR tools never return diff content.
6. Path traversal: `../`, absolute, `a/../secret`, `%2e%2e`, backslash, NUL → all
   rejected.
7. Rule precedence: specific-before-broad, `deny` override, default deny, `*` vs
   `**`.
8. Unknown capability and missing `token_env` rejected at config load.
9. Token never appears in tool output or logs (assert on captured logs).
10. `list_repositories` reports configured capabilities and does not leak `.noai`
    state.

## 11. Dependencies

| Dependency                                   | Purpose                      |
|----------------------------------------------|------------------------------|
| `github.com/modelcontextprotocol/go-sdk`     | Official MCP server SDK      |
| `gitlab.com/gitlab-org/api/client-go`        | Official GitLab API client   |
| `github.com/bmatcuk/doublestar/v4`           | `**`-capable glob matching   |
| `gopkg.in/yaml.v3`                           | Config parsing               |
| stdlib `testing`                             | Tests (no testify dependency)|

## 12. Repository Layout

```
cmd/mcp-forj/main.go            entrypoint / wiring
internal/config/                YAML config + validation
internal/policy/                capabilities, engine, .noai guard
internal/provider/              Provider interface, types, registry/factory
internal/provider/gitlab/       GitLab implementation
internal/server/                MCP server + tool handlers
configs/config.example.yaml     documented example
docs/DESIGN.md                  this document
README.md                       usage documentation
Makefile                        build/test/lint targets
.gitignore                      ignores opencode.json and secrets
```

## 13. Future Extensibility

- **New providers**: implement `Provider`, add a factory case (`github`, `forjo`).
- **New capabilities**: extend the enum and add tools; existing configs stay valid.
  `mr:diff` is already reserved for diff support.
- **Transport**: add HTTP/streamable transport behind the same server package.
- **Policy engine**: the `Guard` interface is intentionally narrow, so the rule
  engine could later be replaced by OPA/CEL without changing tool handlers.
- **Write operations**: `mr:write` / `repo:write` are already reserved.
- **Optional `.noai` caching / listing integration**: revisit once the basics are
  stable; if added, key by `(repo, default-branch)` with a short TTL and
  negative-only caching.

## 14. Resolved Questions (from v0.1)

1. `list_repositories` stays **config-only** (no capability); returns **configured**
   capabilities and states that `.noai` may further restrict.
2. `.noai` cache **removed** for v1; marker checked on every operation.
3. `token_file` **deferred**; env-var-only for v1.
