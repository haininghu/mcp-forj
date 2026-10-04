# Design: `mcp-forj` — Policy-Governed MCP Server for Code Hosting Providers

Status: **Draft v0.1** (for review)
Author: orchestrator
Scope: first iteration (GitLab only, read + MR comment)

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
- No OAuth / interactive login. Tokens are read from environment variables only.
- No HTTP/SSE transport; stdio transport only.
- No remote data caching beyond the `.noai` marker check.
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
        │  rule matching + .noai guard         │
        └───────┬──────────────────────┬───────┘
                │                      │
                ▼                      ▼
     internal/provider          internal/policy/noai
     (interface + registry)     (marker cache, fail-closed)
                │
                ▼
     internal/provider/gitlab
     (official GitLab client)
```

Every tool handler follows the same flow:

1. Resolve provider from the registry by name.
2. Call `Guard.Authorize(ctx, provider, repo, capability)`.
3. If denied, return a structured MCP error; never call the provider.
4. Otherwise perform the provider operation and map the result to MCP content.

## 5. Capability Model

Capabilities are opaque strings validated at config-load time (unknown capabilities
are rejected — fail closed).

| Capability   | Meaning                                                        |
|--------------|----------------------------------------------------------------|
| `repo:read`  | Read repository files / directory listings.                    |
| `mr:read`    | List and view merge requests, including diffs and notes.       |
| `mr:comment` | Create comments/notes on merge requests.                       |
| `mr:write`   | Create, update, merge, or close merge requests. *(reserved)*   |
| `repo:write` | Modify repository content (branches, files, pushes). *(reserved)* |

`mr:write` and `repo:write` are defined but no tool uses them yet; this keeps the
config vocabulary stable for the next iteration.

The example from the requirements maps to:
`allow: [mr:read, mr:comment]` — inspect and comment on MRs, but no `repo:read`,
`mr:write`, or `repo:write`.

## 6. Configuration

YAML, path passed via `-config` (default `configs/config.yaml`), overridable by
`MCP_FORJ_CONFIG`. A committed `configs/config.example.yaml` documents the format.

```yaml
server:
  name: mcp-forj
  log_level: info            # debug|info|warn|error
  noai:
    marker_file: .noai
    cache_ttl: 5m
    fail_mode: closed        # closed: deny if the marker cannot be checked

providers:
  - name: gitlab-work        # logical name used by all tools
    type: gitlab             # provider factory key
    base_url: https://gitlab.example.com
    token_env: GITLAB_WORK_TOKEN   # secret is read from the environment
    request_timeout: 30s
    default_effect: deny     # deny|allow, applies when no rule matches
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
  `namespace/project` path (`*` matches within a path segment, `**` across).
- Rules are evaluated **in order; first match wins**. This makes overrides
  predictable (put specific rules before broad ones).
- If no rule matches, `default_effect` applies (default: `deny`).
- A rule with `effect: deny` short-circuits to deny.
- A rule with `effect: allow` grants only the listed `capabilities`.

### `.noai` guard

The `.noai` check is independent of and **senior to** all rules: if the marker file
exists in the repository, every capability is denied. It cannot be enabled or
disabled by rules. Results are cached per repository for `cache_ttl`. With
`fail_mode: closed`, an error while checking the marker denies the operation.

> Note: the marker is checked with the server's own token via a dedicated provider
> call (`FileExists`). This is a privileged internal call and is not exposed as a
> capability — a repository with `mr:read` only is still protected.

## 7. Provider Interface

```go
type Repository struct {
    Provider string // provider name from config
    Path     string // canonical namespace/project
    WebURL   string
}

type MergeRequest struct {
    IID          int64
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
    GetMergeRequest(ctx context.Context, repo string, iid int64) (*MergeRequest, error)
    ListMergeRequestNotes(ctx context.Context, repo string, iid int64) ([]Note, error)
    AddMergeRequestNote(ctx context.Context, repo string, iid int64, body string) (*Note, error)

    ReadFile(ctx context.Context, repo, path, ref string) ([]byte, error)
    FileExists(ctx context.Context, repo, path, ref string) (bool, error)
}
```

A factory maps `type` (e.g. `gitlab`) to a constructor, so new providers only need
to implement `Provider` and register a case.

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
configured repository together with its **effective** capabilities, so an agent can
discover what it is allowed to do. It deliberately does not require a capability.

Tool arguments are validated with explicit bounds:
- `repo` must match at least one configured rule for the provider.
- `iid` must be positive.
- `body` must be non-empty and within a size limit.
- `path` must be repository-relative: no absolute paths, no `..` segments.

## 9. Security Considerations

- **Deny by default**, at both provider and rule level.
- **`.noai` is authoritative** and fail-closed; checked before every operation.
- **Secrets**: tokens come from environment variables, are never logged, and are
  never returned in tool output. Config values are redacted in logs.
- **Path safety** for `read_file`: reject absolute paths and `..`; normalize with
  `path.Clean`.
- **Output limits**: cap file and note sizes returned to the client.
- **Audit logging**: every authorization decision (allow/deny + reason) is logged
  with structured fields (`provider`, `repo`, `capability`, `decision`, `reason`).
- **Error hygiene**: provider errors are mapped to safe messages; raw HTTP bodies
  are not leaked to the MCP client.

## 10. Testing Strategy

- `internal/config`: parsing, defaults, unknown capability rejection, missing
  `token_env`, duplicate provider names.
- `internal/policy`: rule precedence, glob matching (`*` vs `**`), default effect,
  capability subset, deny override.
- `internal/policy/noai`: marker present/absent, provider error with `closed` vs
  `open`, cache TTL behaviour.
- `internal/server`: end-to-end tool calls against a **fake provider** using the
  SDK's in-memory transports; assert allow, capability denial, `.noai` denial, and
  argument validation.
- `internal/provider/gitlab`: mapping logic plus an `httptest`-based client test.

Target: meaningful coverage of policy and server packages (the security-critical
parts), not a blanket percentage.

## 11. Dependencies

| Dependency                                   | Purpose                      |
|----------------------------------------------|------------------------------|
| `github.com/modelcontextprotocol/go-sdk`     | Official MCP server SDK      |
| `gitlab.com/gitlab-org/api/client-go`        | Official GitLab API client   |
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
- **Transport**: add HTTP/streamable transport behind the same server package.
- **Policy engine**: the `Guard` interface is intentionally narrow, so the rule
  engine could later be replaced by OPA/CEL without changing tool handlers.
- **Write operations**: `mr:write` / `repo:write` are already reserved.

## 14. Open Questions

1. Should `list_repositories` require any capability, or stay config-only?
   (Current proposal: config-only for discoverability.)
2. `.noai` cache TTL default of 5m — acceptable, or shorter for safety?
3. Token handling: env vars only for now; should a `token_file` option be added?
