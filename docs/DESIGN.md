# Design: `mcp-forj` — Policy-Governed MCP Server for Code Hosting Providers

Status: **Draft v0.4** (adds tag-scoped MR capabilities)
Author: orchestrator
Scope: first iteration (GitLab only; MR metadata + comments + repo listing)

## Changelog vs. v0.3

- **U1** Added optional **tag filters** to MR capabilities, written as a
  single-key mapping in the `capabilities` list (`mr:comment: {require: [...],
  exclude: [...]}`). Tags are GitLab MR labels, matched by exact, case-sensitive
  equality.
- **U2** Tag filters are only supported for `mr:read`, `mr:diff`, `mr:comment` and
  `mr:write`; filters on `repo:*` capabilities and on `deny` rules are rejected at
  config load.
- **U3** `get_merge_request`, `list_merge_request_notes` and
  `add_merge_request_note` evaluate tags against the fetched merge request.
  `list_merge_requests` does not (the list API returns no labels) and **fails
  closed** when an `mr:read` filter is active.
- **U4** Tags are an authorization input only: never cached, never returned in tool
  output. Unknown tag information fails closed.

## Changelog vs. v0.2

- **T1** Replaced `token_env` with `token`. The value may be a literal secret or
  reference an environment variable with `${NAME}`. Literal secrets are discouraged
  but supported.
- **T2** Added the `repo:list` capability and a provider `ListRepositories`
  operation. `list_repositories` now enumerates concrete repositories (requires
  `repo:list`); the previous config-only view moved to `list_configured_rules`.
- **T3** Repository enumeration filters candidates through the normal policy check
  per repository, so `.noai` repositories are excluded and marker-check failures
  fail closed per repository.

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
| `repo:list`  | Discover repositories matching the configured patterns.       |
| `repo:read`  | Read repository files / directory listings.                   |
| `mr:read`    | List and view merge request **metadata** and notes. No diffs. |
| `mr:diff`    | Read merge request diff content. *(reserved, not in v1)*      |
| `mr:comment` | Create comments/notes on merge requests.                      |
| `mr:write`   | Create, update, merge, or close merge requests. *(reserved)*  |
| `repo:write` | Modify repository content (branches, files, pushes). *(reserved)* |

`repo:list`, `repo:read`, `mr:read`, and `mr:comment` are used by v1 tools. The
remaining capabilities are defined but unused so the config vocabulary is stable.

`repo:list` is granted per pattern and gates **only dynamic discovery** through the
provider API. Concrete repository paths listed literally in the configuration (no
glob metacharacters) are always returned by `list_repositories` regardless of
`repo:list` (a `deny` rule still hides them), because they are already known
statically. For glob patterns the provider is queried and each candidate is admitted
only if the first matching rule grants `repo:list` for that repository, so a grant on
`legacy/**` lets an agent discover the repositories under `legacy/` without exposing
anything else.

The example from the requirements maps to:
`allow: [mr:read, mr:comment]` — inspect and comment on MRs, but no `repo:read`,
`mr:diff`, `mr:write`, or `repo:write`.

**Note:** MR descriptions and notes may contain pasted code. This is inherent to
viewing MRs and is accepted; it is not the same as granting repository read access.

**Independence:** `mr:comment` does not imply `mr:read`. Posting a note does not
require reading the merge request, so a repository may grant comment-only access.

### Tag-scoped capabilities (v0.4)

An MR capability may optionally carry a **tag filter** (GitLab MR labels). In the
configuration the filter is merged into the `capabilities` list as a compact
single-key mapping:

```yaml
capabilities:
  - mr:read
  - mr:comment:
      require: [ai-reviewed]      # MR must have ALL of these labels
      exclude: [do-not-touch]     # MR must have NONE of these labels
```

- Matching is **exact and case-sensitive**; there is no glob/regex support.
- Filters are only supported for MR capabilities (`mr:read`, `mr:diff`,
  `mr:comment`, `mr:write`). A filter on a `repo:*` capability is rejected at config
  load as "not supported yet", and a filter on a `deny` rule is rejected.
- A tag may not appear in both `require` and `exclude`; tags must be non-empty after
  trimming; a capability may not be listed twice in the same rule.
- Tags are an **authorization input only**: they are never cached and never returned
  in tool output. When tag information cannot be determined for an active filter,
  the decision fails closed.

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
    # Either a literal secret (discouraged) or an environment reference:
    token: "${GITLAB_WORK_TOKEN}"
    request_timeout: 30s
    rules:
      - repositories: ["team/service-a", "team/service-b"]
        effect: allow
        capabilities:
          - mr:read
          # Compact form: capability name mapping to an optional tag filter.
          - mr:comment:
              require: [ai-reviewed]
              exclude: [do-not-touch]
      - repositories: ["team/*"]
        effect: allow
        capabilities: [mr:read]
      - repositories: ["archive/**"]
        effect: allow
        capabilities: [repo:list, mr:read]
      - repositories: ["legacy/**"]
        effect: deny
```

The `capabilities` entries accept two forms: a plain scalar (`mr:read`) or a
single-key mapping whose value is `null`/omitted or a mapping with only the optional
keys `require` and `exclude`. Unknown filter keys and mappings with more than one
capability key are rejected. See §5 for the tag-filter rules.

### Token handling

- `token` is the only secret field and is represented by a `config.Secret` type
  whose `String`/`GoString` return `[REDACTED]`, so accidental `%+v`/slog dumps of
  the configuration cannot leak it. The raw value is reachable only via
  `Secret.Value()`.
- The value is **whole-string only**: after trimming surrounding whitespace, if it
  matches `^\$\{[A-Za-z_][A-Za-z0-9_]*\}$` the named environment variable is
  expanded; otherwise the value is used as a literal. Embedded mixing
  (`"prefix-${VAR}"`) is intentionally not supported in v1, which removes the need
  for an escape mechanism and lets literal secrets contain `$`.
- If the referenced environment variable is unset or empty, configuration loading
  fails. A missing or empty resolved token is a validation error.
- The resolved token is never logged and never returned in tool output. Error
  messages name only the environment variable, never its value.
- **Literal secrets are discouraged**: prefer `${...}` and keep the real config
  (`configs/config.yaml`) git-ignored.

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

> **Operational requirement:** the provider token (`token`) must be able to
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

type RepoListOptions struct {
    Search string // optional provider-side search term (usually a namespace prefix)
    Limit  int
}

type Provider interface {
    Name() string
    Type() string

    ListRepositories(ctx context.Context, opts RepoListOptions) ([]Repository, error)
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

`ListRepositories` is **membership-scoped** (only repositories the token can see as
a member) and **paginated**: the GitLab implementation maps `Limit` to `PerPage`
(default 100) and follows pages until `Limit` is reached or the provider is
exhausted. If more repositories exist than the limit allows, the server reports
`truncated: true`. A single page is never silently treated as complete.

## 8. MCP Tools (first iteration)

| Tool                        | Capability   | Provider operation          |
|-----------------------------|--------------|-----------------------------|
| `list_configured_rules`     | none         | — (from config)             |
| `list_repositories`         | `repo:list`  | `ListRepositories`          |
| `list_merge_requests`       | `mr:read`    | `ListMergeRequests`         |
| `get_merge_request`         | `mr:read`¹   | `GetMergeRequest`           |
| `list_merge_request_notes`  | `mr:read`¹   | `ListMergeRequestNotes`     |
| `add_merge_request_note`    | `mr:comment`¹| `AddMergeRequestNote`       |
| `read_file`                 | `repo:read`  | `ReadFile`                  |

¹ **Tag filters** (§5) are evaluated against the fetched merge request.
`get_merge_request` and `list_merge_request_notes` enforce an active `mr:read`
filter; `add_merge_request_note` enforces an active `mr:comment` filter. These tools
perform a policy-only pre-check (`Guard.AuthorizeRepoCapability`), fetch the merge
request metadata, then call `Guard.AuthorizeWithTags` with the MR labels. For
`add_merge_request_note` the metadata fetch is an internal authorization input: if it
fails, the post is denied (fail-closed). `list_merge_requests` does **not** evaluate
labels (the list API returns none) and therefore **fails closed** whenever an
`mr:read` tag filter is active.

`list_configured_rules` is config-only (no remote call, no secrets) and returns each
configured rule with its **configured** capabilities, including any tag filters as
`{"name": ..., "require": [...], "exclude": [...]}`. It requires no capability and
is the discoverability entry point. It does **not** claim the capabilities are
effective and does not reveal `.noai` state.

`list_repositories` returns the repositories explicitly configured for a provider
(always) plus repositories discovered through the provider API when `repo:list` is
granted:

- Input: `provider` (required; must be a registered provider), `search` (optional
  provider-side search term), `limit` (optional).
- **Static repositories**: concrete paths listed literally in the configuration are
  always returned, in first-appearance order and deduplicated, provided the first
  matching rule allows them (a `deny` rule hides them). This requires no `repo:list`
  capability, no provider API call and no `.noai` check. Static entries have no
  `web_url`.
- **Dynamic discovery**: if at least one allow-rule grants `repo:list`
  (`Guard.AuthorizeList`), the server issues one `ListRepositories` call with the
  caller's `search` (or none) and filters candidates client-side using the
  policy-only `Guard.Evaluate(..., repo:list)`. If `repo:list` is not granted, the
  call still succeeds and returns only the static repositories (possibly none); it is
  **not** an error. There is no all-providers mode: `provider` is mandatory.
- **`.noai` does not affect listing.** Listing never checks the marker, so a `.noai`
  repository (static or discovered) still appears. `.noai` only blocks operations via
  `Guard.Authorize`.
- The `omitted` counter counts **only** discovered candidates that matched a rule but
  were not allowed for `repo:list` (e.g. a `deny` rule or a rule that lacks the
  capability). Candidates that match no rule at all are simply filtered out and are
  **not** counted. Static repositories never contribute to `omitted`.
- Results are deduplicated by provider+path (static wins). `limit` bounds only the
  number of **discovered** repositories; static repositories are always returned and
  may push the total above `limit`. The server requests `limit+1` candidates and
  stops paginating there, so receiving more than `limit` candidates is itself
  reported as `truncated`, as is dropping an allowed candidate because the dynamic
  cap was reached. `truncated` therefore means the discovered result may be
  incomplete; a filtered-out candidate does not by itself suppress `truncated`.

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
- **`.noai` is authoritative** and fail-closed; checked before every **operation**
  via `Guard.Authorize`. It does **not** affect `list_repositories`: listing never
  checks the marker, so a `.noai` repository may appear in a listing. Listing only
  reflects configured visibility; the marker still blocks every attempt to operate on
  that repository.
- **Tags are an authorization input only.** Tag filters (§5) are evaluated against
  merge request labels fetched per operation; labels are never cached and never
  returned in tool output (the server's JSON output structs deliberately omit them).
  When labels cannot be determined for an active filter, the decision fails closed
  (`tag information unavailable`). `list_merge_requests` cannot evaluate labels and
  fails closed under an active `mr:read` filter.
- **`list_configured_rules` exposes the policy** (patterns and effects) to the
  caller. This is intentional in the single-trusted-agent model and reveals no
  secrets; revisit if per-client identities are ever added.
- **Secrets**: the token may be a literal or a `${NAME}` environment reference; the
  resolved value is never logged and never returned in tool output. Errors name only
  the environment variable, not its value. Literal secrets are discouraged and the
  real config is git-ignored.
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
  `token`, literal token (including one containing `$`), whole-string `${VAR}`
  resolution, unset/empty `${VAR}` rejection, whitespace trimming, duplicate
  provider names, empty rules, and a redaction assertion that `fmt.Sprintf("%+v",
  cfg)` / `Secret.String()` never contains the resolved value. Capability-grant
  parsing: scalar form, compact mapping form (require/exclude), null value, and
  rejection of unknown filter keys, more than one capability key, a tag in both
  lists, empty tags, duplicate capabilities, filters on `repo:*`, and filters on
  `deny` rules.
- `internal/policy`: rule precedence, `*` vs `**` glob matching, default deny,
  capability subset, deny override, `EvaluateWithTags` (require-all, exclude-any,
  fail-closed on unknown tags, zero filter unaffected), `CapabilityGranted`
  semantics, and `GrantsAnywhere` with filters.
- `internal/policy/noai`: marker present/absent, provider error (always deny).
- `internal/server`: end-to-end tool calls against a **fake provider** using the
  SDK's in-memory transports; assert allow, capability denial, `.noai` denial, repo
  listing filtering, argument validation, and tag enforcement (allowed for a matching
  label, denied for an excluded label, denied when labels are unknown, denied when an
  active `mr:read` filter blocks `list_merge_requests`, and a metadata-fetch error
  denying a note).
- `internal/provider/gitlab`: mapping logic plus an `httptest`-based client test;
  `GetMergeRequest` maps labels and sets `LabelsKnown`.

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
8. Unknown capability and missing `token` rejected at config load; an unset `${VAR}`
   reference is rejected.
9. Token never appears in tool output or logs (assert on captured logs), including
   when it is supplied literally.
10. `list_configured_rules` reports configured capabilities and does not leak `.noai`
    state or the token.
11. `list_repositories`: literal (static) repositories are returned without
    `repo:list` and without a provider call; a static repository denied by a `deny`
    rule is excluded; a static `.noai` repository is still returned; with `repo:list`
    on `archive/**` only matching discovered repositories are returned; a discovered
    `.noai` repository is included (listing ignores the marker); missing `repo:list`
    is not an error and yields static repositories only; unknown provider is an error
    and an omitted `provider` is rejected by the schema; duplicates between static and
    dynamic lists are removed; `truncated` is set when the cap or fetch window is hit.
12. Tag filters: a matching label allows, an excluded/missing label denies, unknown
    labels fail closed; `get_merge_request` and `list_merge_request_notes` enforce
    `mr:read` filters, `add_merge_request_note` enforces `mr:comment` filters and
    denies when the metadata fetch fails, `list_merge_requests` fails closed under an
    active `mr:read` filter, and `list_configured_rules` exposes the filters.

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

1. The config-only view moved to `list_configured_rules` (no capability); it returns
   **configured** capabilities and states that `.noai` may further restrict.
2. `.noai` cache **removed** for v1; marker checked on every operation.
3. `token_file` **deferred**; `token` accepts a literal or a `${VAR}` reference.

## 15. Resolved Questions (v0.3)

1. **Token**: `token` is a whole-string literal or a single `${NAME}` reference;
   resolution happens at load time and an unset/empty reference is a hard error.
   Literal secrets are supported but discouraged. The value is held in a redacting
   `Secret` type.
2. **`repo:list` scope**: granted per pattern and gates only dynamic provider-API
   discovery. Concrete paths listed literally in the configuration are always
   returned regardless of `repo:list` (subject to `deny` rules). Dynamic candidates
   are filtered through the policy-only per-repository `repo:list` check, so grants
   cannot leak beyond their pattern. Listing ignores `.noai`.
3. **Listing pagination**: the provider maps the limit to `PerPage` and follows pages
   up to the limit, reporting `truncated`. Derived-prefix querying was cut; `search`
   is a caller-controlled passthrough.

## 16. Resolved Questions (v0.4)

1. **Tag syntax**: filters are merged into the `capabilities` list as a compact
   single-key mapping (`mr:comment: {require: [...], exclude: [...]}`). Scalars remain
   valid. Tag equality is exact and case-sensitive.
2. **Tag scope**: only MR capabilities may carry filters; `repo:*` filters and
   filters on `deny` rules are rejected at config load.
3. **Enforcement**: `get_merge_request`, `list_merge_request_notes` and
   `add_merge_request_note` evaluate tags after fetching the MR. `list_merge_requests`
   cannot (list API returns no labels) and fails closed under an active `mr:read`
   filter. Unknown labels fail closed everywhere.
4. **Tags are never returned**: labels are an authorization input only and are never
   included in tool output.
