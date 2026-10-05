# Design: `mcp-forj` — Policy-Governed MCP Server for Code Hosting Providers

Status: **Draft v0.10** (status-based provider errors; MR rebase status exposed)
Author: orchestrator
Scope: first iteration (GitLab only; MR metadata + comments + repo listing + rebase)

## Changelog vs. v0.9

- **AA1** Provider errors are mapped to safe, **status-based** sentinels:
  `ErrForbidden` (401/403), `ErrInvalidState` (400/405/409), `ErrNotFound` (404).
  The server turns these into actionable messages instead of a generic
  "provider request failed", so a missing scope/role (403) or a non-rebaseable MR
  (405/409) is distinguishable. Raw response bodies are never surfaced.
- **AA2** `get_merge_request` now exposes `rebase_in_progress`, `merge_error`,
  `has_conflicts` and `detailed_merge_status` (the rebase outcome). Labels are still
  never returned.

## Changelog vs. v0.8

- **Z1** The GitLab merge-request **list** endpoint returns `labels`. The provider
  now maps them and sets `LabelsKnown`, so `list_merge_requests` enforces an active
  `mr:read` tag filter **client-side** per MR instead of failing closed. MRs whose
  labels are unknown (field absent/null) are still omitted (fail-closed) and counted
  in the new `omitted` field.

## Changelog vs. v0.7

- **Y1** `.noai` protects all operations on repository **contents**: both
  `repo:read` and `repo:write` (in addition to the existing `read_file` behavior).
  It still does **not** affect merge-request capabilities (`mr:*`) or repository
  listing (`repo:list`). This is a forward-looking guarantee: `repo:write` has no
  tool yet, but a future write tool will be blocked by the marker automatically.

## Changelog vs. v0.6

- **X1** The `.noai` marker now protects **only** repository file reads
  (`repo:read` / `read_file`). Every other capability, when granted by policy,
  works on `.noai` repositories. `IsMarkerProtected` centralizes the exemption so
  more capabilities can be added later. Marker behavior remains fail-closed for
  `repo:read`.
- **X2** `deny` rules must not list `capabilities` at all (previously only tag
  filters were rejected). A `deny` rule only hides matching repositories.

## Changelog vs. v0.5

- **W1** Added the `mr:rebase` capability and the `rebase_merge_request` tool. It
  triggers an asynchronous GitLab rebase of an MR's source branch onto its target
  branch. `mr:write` stays reserved.
- **W2** Rebase is a write operation and requires push access on the source branch;
  the tool fetches MR metadata first as an internal authorization input (fail-closed)
  and supports MR tag filters like the other MR tools.

## Changelog vs. v0.4

- **V1** Tag filters are now accepted on **any** known capability. For repo
  capabilities the filter matches GitLab project **topics** (for MR capabilities it
  still matches MR labels).
- **V2** Added `Repository.Topics`/`TopicsKnown` (tri-state) and the provider
  `GetRepositoryTopics` operation; `ListRepositories` populates topics.
- **V3** `read_file` (`repo:read`) and `list_repositories` (`repo:list`) now enforce
  repo topic filters, exact and case-sensitive, fail-closed on unknown topics.
- **V4** Active `repo:list` topic filters also apply to repositories listed literally
  in the configuration; if no such filter is active, literal repositories are still
  returned with no provider call. `repo:write` filters are accepted but inert.

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
changing anything is forbidden. Repositories that contain a `.noai` marker file
cannot have their **contents read or written** (`repo:read` / `repo:write`);
merge-request operations and repository listing still follow their granted
capabilities.

The first iteration must stay deliberately small so the direction can change later.

## 2. Goals

- One MCP server exposing tools for common repository/merge-request operations.
- **Deny-by-default** authorization, driven by a human-readable YAML config.
- Per-provider, per-repository **capability** grants.
- Hard, non-overridable block on repository-content operations (read/write) for
  repositories containing `.noai`.
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
| `mr:rebase`  | Trigger an asynchronous merge request rebase.                 |
| `mr:write`   | Create, update, merge, or close merge requests. *(reserved)*  |
| `repo:write` | Modify repository content (branches, files, pushes). *(reserved)* |

`repo:list`, `repo:read`, `mr:read`, `mr:comment` and `mr:rebase` are used by tools.
The remaining capabilities are reserved so the config vocabulary is stable.

`repo:list` is granted per pattern and gates **only dynamic discovery** through the
provider API. Concrete repository paths listed literally in the configuration (no
glob metacharacters) are returned by `list_repositories` regardless of `repo:list` (a
`deny` rule still hides them), because they are already known statically. **Exception
(see §8):** if the matched allow rule carries an active `repo:list` topic filter, that
filter also applies to these literal repositories, so they are topic-checked and may
be omitted. For glob patterns the provider is queried and each candidate is admitted
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

### Tag-scoped capabilities (v0.5)

Any capability may optionally carry a **tag filter**. For MR capabilities the tags
are GitLab MR **labels**; for repo capabilities they are project **topics**. In the
configuration the filter is merged into the `capabilities` list as a compact
single-key mapping:

```yaml
capabilities:
  - mr:read
  - mr:comment:
      require: [ai-reviewed]      # MR must have ALL of these labels
      exclude: [do-not-touch]     # MR must have NONE of these labels
  - repo:list:
      require: [ai-ok]            # project must have this topic
  - repo:read:
      exclude: [confidential]     # project must not have this topic
```

- Matching is **exact and case-sensitive**; there is no glob/regex support.
- Filters are accepted on any known capability, but a filter on a `deny` rule is
  rejected at config load. `repo:write` filters are accepted but inert (no tool).
- A tag may not appear in both `require` and `exclude`; tags must be non-empty after
  trimming; a capability may not be listed twice in the same rule.
- Tags are an **authorization input only**: they are never cached and never returned
  in tool output. When tag information cannot be determined for an active filter,
  the decision fails closed.
- Because a `repo:list` filter would otherwise be bypassed by literal configuration
  entries, it also applies to **static** repositories; see §8.

## 6. Configuration

YAML, path passed via `-config` (default `configs/config.yaml`), overridable by
`MCP_FORJ_CONFIG`. A committed `configs/config.example.yaml` documents the format.

```yaml
server:
  name: mcp-forj
  log_level: info            # debug|info|warn|error
  noai:
    marker_file: .noai       # repository marker that blocks file reads (read_file)

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
      - repositories: ["team/ai-service", "archive/**"]
        effect: allow
        # Repo filters match project topics; the repo:list filter also applies
        # to the literal "team/ai-service" entry.
        capabilities:
          - repo:list:
              require: [ai-ok]
          - mr:read
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
- A rule with `effect: deny` short-circuits to deny. It **must not list
  `capabilities`** (rejected at config load); a deny rule only hides the matching
  repositories.
- A rule with `effect: allow` grants only the listed `capabilities`.
- `default_effect` and `fail_mode` do **not** exist: deny-by-default and fail-closed
  are not configurable.

### `.noai` guard

The `.noai` marker protects operations on repository **contents**: both
`repo:read` (`read_file`) and `repo:write` (no tool yet). It is independent of and
senior to the rules: if the marker file exists in the repository, those operations
are denied regardless of the grant, and it cannot be enabled or disabled by rules.
It does **not** affect merge-request capabilities (`mr:*`) or repository listing
(`repo:list`, which is discovery/metadata); those work on `.noai` repositories when
granted by policy. The set of protected capabilities is centralized in
`policy.IsMarkerProtected`.

For v1 there is **no cache** — the marker is checked on every content operation. If
the check fails for any reason, the operation is denied (fail-closed).

The marker is checked at the repository's **default branch** (HEAD), using the
server's own token via a dedicated provider call (`FileExists`). This is a
privileged internal call and is not exposed as a capability.

> **Operational requirement:** the provider token (`token`) must be able to
> read repository files at least on the default branch. A token scoped to
> merge-request access only will make the `.noai` check fail, which (by design)
> denies **`read_file`** (fail-closed). Merge-request operations are unaffected.
> Document this clearly for operators.

## 7. Provider Interface

```go
type Repository struct {
    Provider    string // provider name from config
    Path        string // canonical namespace/project
    WebURL      string
    Topics      []string // GitLab project topics
    TopicsKnown bool     // false = topics unavailable -> fail closed
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
    Labels       []string // GitLab MR labels
    LabelsKnown  bool     // false = labels unavailable -> fail closed
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
    GetRepositoryTopics(ctx context.Context, repo string) ([]string, error)
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

The GitLab implementation maps each project's `topics` into `Repository.Topics` and
sets `TopicsKnown = true`. `GetRepositoryTopics` fetches a single project (via the
project endpoint) and returns its `topics`; errors map to `ErrNotFound`/a safe
provider error. Topics are an authorization input and are never returned in tool
output.

## 8. MCP Tools (first iteration)

| Tool                        | Capability   | Provider operation          |
|-----------------------------|--------------|-----------------------------|
| `list_configured_rules`     | none         | — (from config)             |
| `list_repositories`         | `repo:list`² | `ListRepositories`          |
| `list_merge_requests`       | `mr:read`¹   | `ListMergeRequests`         |
| `get_merge_request`         | `mr:read`¹   | `GetMergeRequest`           |
| `list_merge_request_notes`  | `mr:read`¹   | `ListMergeRequestNotes`     |
| `add_merge_request_note`    | `mr:comment`¹| `AddMergeRequestNote`       |
| `rebase_merge_request`      | `mr:rebase`³ | `RebaseMergeRequest`        |
| `read_file`                 | `repo:read`² | `ReadFile`                  |

¹ **Tag filters** (§5) are evaluated against merge request labels.
`get_merge_request` and `list_merge_request_notes` enforce an active `mr:read`
filter; `add_merge_request_note` enforces an active `mr:comment` filter. These tools
perform a policy-only pre-check (`Guard.AuthorizeRepoCapability`), fetch the merge
request metadata, then call `Guard.AuthorizeWithTags` with the MR labels. For
`add_merge_request_note` the metadata fetch is an internal authorization input: if it
fails, the post is denied (fail-closed). `list_merge_requests` enforces an active
`mr:read` filter **client-side**: the GitLab list endpoint returns `labels`, so each
returned MR is evaluated with its own labels (`Guard.EvaluateWithTags`). MRs that do
not match are skipped and counted in `omitted`; MRs whose labels are unknown
(`LabelsKnown=false`) are omitted (fail-closed) and also counted.

² **Repo topic filters** are evaluated against project topics. `read_file` fetches
the repository's topics (`GetRepositoryTopics`) only when an active `repo:read`
filter is configured, then evaluates them; a topic-fetch error denies the read
(fail-closed). `list_repositories` applies an active `repo:list` filter to both
static and discovered repositories (see below). `repo:write` has no tool, so filters
on it are accepted but inert.

³ **`rebase_merge_request`** is a **write** operation. It requires the `mr:rebase`
capability and triggers an **asynchronous** GitLab rebase of the source branch onto
the target branch; the call returns a `"rebase requested"` acknowledgement and the
outcome is visible later via `get_merge_request` (`merge_error`,
`rebase_in_progress`, `has_conflicts`, `detailed_merge_status`). The provider token
must have write access (write scope, MR Update permission, or a sufficient project
role); a 401/403 is surfaced as an actionable forbidden message. MR **tag filters**
apply: the server fetches the MR metadata first as an internal authorization input
(`Guard.AuthorizeRepoCapability` pre-check → `GetMergeRequest` →
`Guard.AuthorizeWithTags`), and a metadata-fetch failure denies the rebase
(fail-closed).

**Provider errors** are mapped to safe, status-based messages and never include raw
response bodies: 401/403 → "forbidden: the provider token lacks the required
permission …", 400/405/409 → "the merge request is not in a rebaseable state",
404 → "not found", anything else → a generic "provider request failed".
`get_merge_request` returns the MR metadata including `rebase_in_progress`,
`merge_error`, `has_conflicts` and `detailed_merge_status`; it never returns labels.

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
  returned, in first-appearance order and deduplicated, provided the first matching
  rule allows them (a `deny` rule hides them). **Conditional guarantee:** if the
  matched allow rule carries an active `repo:list` topic filter, the repository's
  topics are fetched and evaluated; a fetch error or a non-match omits it and
  increments `omitted` (fail-closed). If **no** `repo:list` topic filter is active for
  that path, the repository is returned with **no provider API call** (and no
  capability or `.noai` check). Static entries have no `web_url`.
- **Dynamic discovery**: if at least one allow-rule grants `repo:list`
  (`Guard.AuthorizeList`), the server issues one `ListRepositories` call with the
  caller's `search` (or none) and filters candidates client-side using the
  policy-only `Guard.EvaluateWithTags(..., repo:list, {topics})`. If `repo:list` is
  not granted, the call still succeeds and returns only the static repositories
  (possibly none); it is **not** an error. There is no all-providers mode: `provider`
  is mandatory.
- **`.noai` does not affect listing.** Listing never checks the marker, so a `.noai`
  repository (static or discovered) still appears. `.noai` only blocks operations via
  `Guard.Authorize`.
- The `omitted` counter counts candidates that matched a rule but were not allowed or
  could not be evaluated: discovered candidates filtered out by the policy (a `deny`
  rule, a missing `repo:list` capability, a failed/unknown topic filter), and static
  repositories omitted by an active `repo:list` topic filter. Candidates that match no
  rule at all are simply filtered out and are **not** counted.
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
- **Write operations** are limited to creating MR comments (`mr:comment`) and
  triggering MR rebases (`mr:rebase`). A rebase is asynchronous and requires push
  access to the source branch; it is authorized like any other MR operation,
  including tag filters, and its MR metadata is fetched first (fail-closed).
- **`.noai` protects repository contents.** It is authoritative and fail-closed for
  `repo:read` and `repo:write`: `read_file` (and any future content-write tool)
  checks the marker via `Guard.AuthorizeWithTags` and is denied if it is present or
  the check fails. It does **not** affect `repo:list` or any merge-request capability
  (`mr:read`, `mr:comment`, `mr:rebase`); those work on `.noai` repositories when
  granted by policy. `IsMarkerProtected` is the single place that defines this.
- **Tags are an authorization input only.** Tag filters (§5) are evaluated against
  merge request labels or project topics fetched per operation; neither labels nor
  topics are cached or returned in tool output (the server's JSON output structs
  deliberately omit them). When tag information cannot be determined for an active
  filter, the decision fails closed (`tag information unavailable`); a repo topic
  fetch error fails closed. `list_merge_requests` evaluates labels from the list
  endpoint per MR (client-side) and omits non-matching or unknown-label MRs. A
  `repo:list` topic filter also applies to static config repositories so it cannot be
  bypassed by listing them literally.
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
- **Error hygiene**: provider errors are mapped to safe, status-based messages
  (401/403 forbidden, 400/405/409 invalid state, 404 not found); raw HTTP bodies are
  never leaked to the MCP client. Actionable diagnostics (e.g. a missing scope or
  role) come from these status classes, not from response content.

## 10. Testing Strategy

- `internal/config`: parsing, defaults, unknown capability rejection, missing
  `token`, literal token (including one containing `$`), whole-string `${VAR}`
  resolution, unset/empty `${VAR}` rejection, whitespace trimming, duplicate
  provider names, empty rules, and a redaction assertion that `fmt.Sprintf("%+v",
  cfg)` / `Secret.String()` never contains the resolved value. Capability-grant
  parsing: scalar form, compact mapping form (require/exclude), null value, and
  rejection of unknown filter keys, more than one capability key, a tag in both
  lists, empty tags, duplicate capabilities, any capabilities on `deny` rules (plain
  or filtered), acceptance of filters on repo capabilities, and acceptance of
  `mr:rebase` (plain and filtered).
- `internal/policy`: rule precedence, `*` vs `**` glob matching, default deny,
  capability subset, deny override, `EvaluateWithTags` (require-all, exclude-any,
  fail-closed on unknown tags, zero filter unaffected, repo capabilities),
  `CapabilityGranted` semantics, `HasTagFilter` (true only for a matched allow grant
  with a non-zero filter), and `GrantsAnywhere` with filters.
- `internal/policy/noai`: `.noai` present/absent and provider error deny the
  content capabilities `repo:read` and `repo:write` (fail-closed), while
  `repo:list` and `mr:read`/`mr:comment`/`mr:rebase` are unaffected.
- `internal/server`: end-to-end tool calls against a **fake provider** using the
  SDK's in-memory transports; assert allow, capability denial, `.noai` denial of
  `read_file` only (MR tools succeed on `.noai` repos), repo listing filtering,
  argument validation, MR label enforcement, repo topic enforcement (`read_file`
  allowed/denied on topics, topic-fetch error denies, no topic call without a filter;
  `list_repositories` static and discovered filtering, omitted counting, and `.noai`
  still denying `read_file` when topics pass), rebase enforcement (allowed with
  `mr:rebase`, denied without it, tag filter matching/excluded/unknown, metadata-fetch
  error denies with no rebase call, `ErrForbidden` yields an actionable message,
  rebase allowed on a `.noai` repo), and `get_merge_request` exposing
  `rebase_in_progress`/`merge_error`/`has_conflicts`/`detailed_merge_status` without
  labels.
- `internal/provider/gitlab`: mapping logic plus `httptest`-based client tests;
  `GetMergeRequest` maps labels (and sets `LabelsKnown`) and the rebase/merge status
  fields, `ListRepositories` maps topics (and sets `TopicsKnown`),
  `GetRepositoryTopics` returns topics / maps 404, `RebaseMergeRequest` issues a
  `PUT .../rebase`, and `mapError` maps 401/403 → `ErrForbidden`, 400/405/409 →
  `ErrInvalidState`, 404 → `ErrNotFound`, else generic.

### Security-critical tests (mandatory)

1. `.noai` present → repository-content operations (`repo:read`, and `repo:write`
   at the guard level) deny even with the grant; every other tool (MR operations,
   `repo:list`) succeeds with its capability.
2. `.noai` check error → content operations deny (fail-closed); other tools are
   unaffected.
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
    denies when the metadata fetch fails, `list_merge_requests` enforces `mr:read`
    filters client-side from list-endpoint labels (`require: [renovate]` returns the
    renovate MRs; excluded and unknown-label MRs are omitted and counted), and
    `list_configured_rules` exposes the filters.
13. Repo topic filters: `read_file` allows a matching topic and denies a
    missing/excluded topic or a topic-fetch error, and makes no topic call when no
    filter is active; `list_repositories` filters both static and discovered
    repositories by topic (a static repo with an active filter is omitted on error or
    non-match and counted in `omitted`; a static repo with no filter is returned with
    no call), and `.noai` still denies `read_file` when the topic filter passes.
14. Rebase: `rebase_merge_request` requires `mr:rebase`; a matching MR label allows,
    an excluded or unknown label denies, a metadata-fetch error denies without
    calling the provider, a provider error maps to a safe message, and `.noai` does
    **not** block it.
15. Diagnostics: `mapError` maps HTTP 401/403 → `ErrForbidden`, 400/405/409 →
    `ErrInvalidState`, 404 → `ErrNotFound` (errors.Is, no raw bodies); the server
    returns actionable messages for these and `get_merge_request` exposes
    `rebase_in_progress`/`merge_error`/`has_conflicts`/`detailed_merge_status`.

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
2. `.noai` cache **removed** for v1; marker checked on every `read_file`.
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
2. **Tag scope** (superseded by v0.5): v0.4 restricted filters to MR capabilities;
   v0.5 extends them to repo capabilities (project topics). Filters on `deny` rules
   remain rejected at config load.
3. **Enforcement** (superseded by v0.9): v0.4 assumed the list API omitted labels and
   made `list_merge_requests` fail closed. v0.9 maps list-endpoint labels and enforces
   the filter client-side. Unknown labels fail closed everywhere.
4. **Tags are never returned**: labels are an authorization input only and are never
   included in tool output.

## 17. Resolved Questions (v0.5)

1. **Repo filters**: tag filters are allowed on any known capability. For repo
   capabilities they match GitLab project **topics**; matching is exact and
   case-sensitive. `repo:write` filters are accepted but inert.
2. **Topic source**: `ListRepositories` populates `Repository.Topics`/`TopicsKnown`
   from `ListProjects`; a single repository's topics come from `GetRepositoryTopics`
   (project endpoint). Topics are never cached or returned.
3. **Static guarantee**: an active `repo:list` topic filter also applies to static
   config repositories (topics fetched, omitted on error/non-match and counted in
   `omitted`). Without an active filter, static repositories are returned with no
   provider call.
4. **Fail-closed**: unknown topics (`TopicsKnown=false`) or a topic-fetch error deny
   `read_file` / omit the repository from `list_repositories`.

## 18. Resolved Questions (v0.6)

1. **Rebase capability**: a dedicated `mr:rebase` capability (not `mr:write`) gates
   `rebase_merge_request`, so a grant to comment cannot implicitly push/rebase.
2. **Asynchronous**: GitLab rebases asynchronously; the tool acknowledges the request
   and the outcome appears later on the merge request. The token needs push access to
   the source branch (403 otherwise).
3. **Authorization**: the MR metadata is fetched first as an internal authorization
   input; MR tag filters apply, and a metadata-fetch failure denies the rebase
   (fail-closed).

## 19. Resolved Questions (v0.7)

1. **`.noai` scope** (superseded by v0.8): v0.7 restricted the marker to
   `repo:read`; v0.8 extends it to both repository-content operations
   (`repo:read` and `repo:write`).
2. **Deny rules**: `deny` rules must not list `capabilities` at all; a `deny` rule
   only hides the matching repositories. Capabilities on deny rules (plain or
   filtered) are rejected at config load.

## 20. Resolved Questions (v0.8)

1. **`.noai` scope**: the marker protects repository-content operations
   (`repo:read`, `repo:write`); `IsMarkerProtected` is the single source of truth.
   It does **not** affect `repo:list` (discovery/metadata) or merge-request
   capabilities (`mr:read`, `mr:comment`, `mr:rebase`), which work on `.noai`
   repositories when granted. The marker check stays fail-closed for content
   operations.
2. **Forward-looking**: `repo:write` has no tool yet; the guard already enforces
   the marker for it so a future write tool inherits the protection automatically.

## 21. Resolved Questions (v0.9)

1. **`list_merge_requests` labels**: the GitLab list endpoint returns `labels`, so
   the provider maps them and sets `LabelsKnown`. `list_merge_requests` evaluates an
   active `mr:read` filter client-side per MR and no longer fails closed. It reports
   `omitted` for MRs that matched a rule but were filtered out or had unknown labels.
2. **Fail-closed on unknown labels**: a MR whose `labels` field is absent or `null`
   has `LabelsKnown=false`; under an active filter it is omitted (and counted).

## 22. Resolved Questions (v0.10)

1. **Status-based errors**: providers map HTTP status to sentinels
   (`ErrForbidden` 401/403, `ErrInvalidState` 400/405/409, `ErrNotFound` 404). The
   server maps these to safe, actionable messages; raw bodies are never surfaced.
2. **Rebase status**: `get_merge_request` exposes `rebase_in_progress`,
   `merge_error`, `has_conflicts` and `detailed_merge_status`, so callers can see
   the asynchronous rebase outcome. Labels remain an authorization input only.
