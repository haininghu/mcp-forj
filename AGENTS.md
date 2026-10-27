# AGENTS.md

Instructions for AI agents (and humans) working on **mcp-forj**.

## What this project is

A policy-governed [Model Context Protocol](https://modelcontextprotocol.io) server written in Go.
It exposes tools that let an AI agent inspect and (limitedly) act on code hosting providers.

- Transport: MCP over **stdio** only. All logging goes to **stderr**.
- Providers: **GitLab first**; [GitHub](https://github.com) and [Forgejo](https://forgejo.org) are
  planned. The provider
  abstraction (`internal/provider`) exists so new backends do not touch policy or server code.
- Authorization: **deny-by-default**, driven by a human-readable YAML config. Capabilities span
  repository content (`repo:*`), merge requests (`mr:*`) and commits (`commit:*`; `commit:read`
  backs the cross-repo `search_commits` tool and is also granted by `repo:read`).
- `.noai`: a repository marker that blocks repository-content operations.

## Repository map

| Path                            | Contents                                                        |
|---------------------------------|-----------------------------------------------------------------|
| `cmd/mcp-forj`                  | Entrypoint and wiring (`main.go`).                              |
| `internal/config`               | YAML config loading, defaults, validation, `Secret` redaction.  |
| `internal/policy`               | Capabilities, ordered rule engine, `.noai` guard.              |
| `internal/provider`             | Provider interface, neutral types, registry and factory.        |
| `internal/provider/gitlab`      | GitLab implementation (official `client-go`).                   |
| `internal/gitproxy`             | Authenticated git smart-HTTP reverse proxy (pkt-line, policy).  |
| `internal/server`               | MCP server, tool handlers, argument validation, limits.         |
| `ai/`                           | Agent docs: architecture overview, ADRs, bug analyses, method specs.  |
| `configs/config.example.yaml`   | Documented example configuration.                               |
| `README.md`, `AGENTS.md`        | User-facing overview and this file.                            |

## Commands

```bash
make build     # build bin/mcp-forj
make test      # go test ./...
make test-race # go test -race ./...
make test-e2e  # go test -tags e2e ./...  (end-to-end tests)
make cover     # coverage report
make vet       # go vet ./...
make fmt       # gofmt -s -w .
make fmt-check # fail if gofmt -s would change any file (writes nothing)
make tidy      # go mod tidy
make lint      # golangci-lint run
make check     # fmt-check + vet + test (writes nothing)
make run       # go run ./cmd/mcp-forj -config ${CONFIG:-configs/config.yaml}
```

Equivalent direct commands:

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
go test -tags e2e ./...   # e2e tests (gitproxy ones need the git binary)
gofmt -l .                # must print nothing
```

### End-to-end tests

Tests behind the `e2e` build tag exercise the two entry points end to end and are
excluded from `go test ./...`:

- `internal/gitproxy/e2e_test.go` runs a **real `git` client** against the proxy with a
  local bare repository served by `git http-backend`; it covers clone, push branch
  policy, `.noai` on the default/target branch and push-option gating. It skips when
  the git binary is missing or under `-short`.
- `internal/server/gitlab_e2e_test.go` drives the MCP tools through the **real GitLab
  client** against an `httptest` mock of the GitLab REST API (no external service).

### Pre-finish checklist

Run these before declaring work done (all must pass):

1. `gofmt -s -w .` then confirm `gofmt -l .` prints nothing.
2. `go vet ./...`
3. `go test -count=1 ./...`
4. `go build ./...`

## Conventions

- Documentation and code comments are in **English**.
- Every **exported** identifier has a doc comment starting with its name.
- Markdown lines are at most **120 characters**.
- No secrets in the repo. `configs/config.yaml` and `opencode.json` are git-ignored. Tokens are read
  from config (literal or `${NAME}` env reference) and never logged or returned.
- Use [Conventional Commits](https://www.conventionalcommits.org/):
  `<type>(<optional scope>): <description>`, types `feat`, `fix`, `refactor`, `docs`, `test`, `chore`.
  Imperative mood, lowercase after the colon, no trailing period; mark breaking changes with `!` and a
  `BREAKING CHANGE:` footer. Keep commits small and focused.
- Prefer Go **1.27 stdlib** helpers: `slices`, `maps`, `min`/`max`, `errors.AsType`, `strings.Cut`.
- Do not add dependencies without a clear need; the pinned deps are the MCP SDK, the GitLab client,
  `doublestar/v4` and `yaml.v3`.
- Do not change `.noai` scope or authorization semantics silently; update `ai/adr` instead.

## Invariants (must not break)

Authorization is the product. These rules are security-critical:

1. **Deny by default.** No rule match means access is denied.
2. **Rules are ordered and first-match-wins.** Put specific rules before broad ones.
3. **Rules are symmetric (firewall-style, ADR 0012).** A rule applies when its `repositories` scope
   matches AND (a) it is a `deny` rule without `capabilities`, or (b) a capability entry matches the
   requested capability and all of that entry's filters pass. `deny` rules **may** list
   `capabilities` and filters; without `capabilities` a `deny` rule denies every capability in its
   scope. An **evaluated** filter miss makes the rule fall through (no hard deny), so a broad
   `allow` above a specific `deny` hides the denial; unavailable filter information (unknown
   tags/topics, empty path, missing branch) fails closed. **Capabilities imply capabilities:**
   `repo:read` also authorizes `commit:read` (with the same filter); an explicit entry for the
   requested capability wins over an implied one, and the implication is symmetric — a `repo:read`
   `deny` denies `commit:read`. An active `paths` filter on a `repo:read` grant fails `commit:read`
   closed.
4. **Capabilities are validated at config load.** Unknown capabilities and bad patterns are rejected.
5. **Authorize before the provider call.** Every capability-gated tool authorizes first and fails
   closed on any error (including unknown tags/topics and metadata-fetch failures).
6. **`.noai` scope.** It is a **capability-level default-deny overlay**: on a `.noai` repo every
   capability is denied unless the matching grant is explicitly exempted with `noai: allow`. It is
   checked on the repository's **default branch** via a file read with `ref=HEAD` (and, for a
   non-default content read, also at the requested ref), is fail-closed, and runs after the policy
   decision (first-match-wins); an exempt grant skips the check. Literal
   config repositories stay listed; discovered `.noai` repos are omitted unless `repo:list` is exempt.
   A `.noai` denial is reported to the client indistinguishably from an unknown repository.
7. **Tag filters** match exact, case-sensitive values: GitLab MR **labels** for `mr:*`, project
   **topics** for `repo:*` and `commit:*`. When the information is unknown, the decision fails
   closed.
8. **Path filters** (doublestar) apply only to `repo:read`/`repo:write`; `paths.exclude` wins over
   `paths.include`, and an active filter with an empty path fails closed. Git traffic cannot
   enforce path filters, so an active `paths` filter means **no git access** (fetch/clone/push) for
   that capability (fail-closed).
9. **Data hygiene.** Labels, topics, commit author emails and tokens are never returned in tool
   output or logs.
10. **Error hygiene.** Provider errors are mapped to safe messages with the numeric HTTP status;
    forbidden errors name the resource permission the operation needs.
11. **`.noai` is an integrity control, not a confidentiality control.** It blocks non-exempt
    operations; it does **not** stop content from reaching the agent through exempted reads
    (`repo:read`, `mr:read`, `mr:diff`). Confidentiality is enforced by the capability policy.
12. **Branch control lives in the `repo:write` rule.** `branches` (doublestar, exclude wins over
    include) is accepted **only** on `repo:write` (config error anywhere else). A `repo:write`
    grant **without** a `branches` filter allows **no push** (fail-closed): a named branch is a hard
    fail-closed deny for that grant and does **not** fall through to a later grant (an evaluated
    branch-filter miss does fall through). Every unique push
    target branch is authorized with `Guard.AuthorizeBranch`, which also checks `.noai` on the
    default branch and the target branch. Branch-less evaluations (`Evaluate`, `Authorize`,
    `AuthorizeResource`, push discovery) do not apply the branch dimension. The proxy additionally
     hard-blocks the default branch (not configurable), non-`refs/heads/` refs, deletes and
     non-fast-forwards. There is **no** global `git_proxy.branches` allowlist.
13. **Git push options are policy-gated.** The proxy parses the negotiated push-options section and
     authorizes every option before forwarding: `merge_request.*` requires `mr:write` (so `mr:write`
     gates merge-request creation via git push), auto-merge options additionally `mr:merge`. A
     cross-project target (`merge_request.target_project`, an escape from the authorized repository),
     `ci.*` and any unknown option are denied **fail-closed**. Guard checks include the `.noai`
     overlay and fail closed on unknown tags; no option content or ref ever reaches the client.

## Authorization flow

Every capability-gated tool follows the same pipeline:

1. Resolve the provider from the registry by name (`provider` is required).
2. Validate arguments (bounds, path traversal, etc.).
3. Optional **policy-only pre-check**: `Guard.AuthorizeRepoCapability` (ignores tags, no marker).
4. **Fetch metadata needed for tags** when a rule that could apply carries a tag constraint
   (`Guard.HasTagConstraint`, conservative): MR metadata (`tags = labels`) for `mr:*`; project
   topics for `repo:*` and `commit:*`. A fetch failure denies the operation (fail-closed).
5. **Authorize with tags**: `Guard.AuthorizeWithTags` (tags) or `Guard.AuthorizeResource`
   (tags + path). Unless the matched grant is `noai`-exempt, this also runs the `.noai` check.
6. Perform the provider operation and map the result. Map errors with `mapProviderError`.

`list_merge_requests` filters client-side from list-endpoint labels (the drop count is logged, not returned);
`list_repositories` lists literal repositories without a marker check and marker-checks discovered
candidates (omitting `.noai` ones unless the `repo:list` grant is `noai`-exempt); `git_remote` is
registered only while the git proxy is enabled, gates `repo:read` via `Guard.Authorize` (unknown tags
fail closed, `.noai` overlay included) before calling the proxy resolver, and never returns the
proxy token. The proxy token is **optional**: without it the proxy binds loopback only and performs
no authentication (`git_remote` then reports `auth.type: "none"`); with it clients use HTTP Basic.
Fetch and push discovery use `Guard.Authorize` (branch-less context); each unique push target
branch is authorized with `Guard.AuthorizeBranch` against the `repo:write` `branches` filter —
a grant without one is a hard fail-closed deny for every push, and a path-filtered grant gets no git
access at all. Push
options are parsed and gated last, before forwarding: `merge_request.*` needs `mr:write`, auto-merge
additionally `mr:merge`, everything else denies (see invariant 13).

`search_commits` separates enumeration from content. An optional `repos` list restricts the search to
those exact repositories with content-only authorization (no `repo:list` and no discovery, exactly
like `read_file`, so a repository hidden by a listing deny is still searchable by name); each entry
is validated and canonicalized, the list is deduplicated and capped at `maxSearchRepos`. With `repos`
omitted it enumerates candidates exactly like
`list_repositories`: literal `commit:read` repositories via `Guard.StaticRepositoriesFor` (which
applies the listing deny; the active `repo:list` filter is applied separately), and provider-side
discovery only behind
`Guard.AuthorizeList`, evaluating each discovered repository with
`Guard.EvaluateWithTags(repo:list, topics)` and the `repo:list` `.noai` overlay. It then authorizes
the `commit:read` content axis per candidate itself: the `commit:read` pre-check, topics when
`Guard.HasTagConstraint` applies (reusing topics already known from enumeration),
`Guard.EvaluateWithTags` and `Guard.CheckNoAI`; only the default branch is searched and a default
author resolves via `CurrentUser` (fail-closed). `commit:read` is authorized by a `commit:read` grant
or by a `repo:read` grant (same filter; an active `paths` filter fails closed). Enumeration is seeded
from literal grants (direct `commit:read` or implied by `repo:read`) plus `repo:list` provider search
prefixes, bounded by fixed budgets; the omitted/limit counters are logged server-side only.
Fine-grained tokens need the **Commit: Read** project permission for the commits endpoint; the
**Repository: Read** permission used by `read_file` (and the `.noai` marker read) does not cover it.

## Where to look

- **System overview / flows**: `ai/architecture.md` — the two entry points (MCP tools, git proxy)
  and the shared `policy.Guard` core, with component and sequence diagrams.
- **Architecture decisions**: `ai/adr/` (start at `0001`).
- **Bugs and lessons**: `ai/bug-analysis/`.
- **MCP tool/operation specifications**: `ai/method-specs/`.
- **Docs index**: `ai/README.md`.

When you change behavior, update the relevant ADR/spec or add a bug analysis; do not rely on
`git log` alone.
