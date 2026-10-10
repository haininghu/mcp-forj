# 0011. Git smart-HTTP reverse proxy

## Status

Accepted.

## Context

The MCP tools give an agent structured read/write access to merge requests and files, but an
agent sometimes needs real `git clone`/`push` against allowed branches. Granting direct provider
git credentials would bypass policy entirely. The MVP therefore embeds an authenticated git
smart-HTTP **reverse proxy** in the server: a separate HTTP listener that adds capability
authorization and fast-forward enforcement between the git client and the provider. **Which
branches may be pushed is capability policy, not proxy configuration**: it is the `branches`
filter of the `repo:write` grant (see below). Repositories and branches are filtered by the
policy; **paths are not enforceable over git** — the proxy does not parse packfiles, so a
path-filtered grant is denied fail-closed (see "One policy core" below).

## One policy core

Both entry points — the MCP tools and the git proxy — call **the same** `policy.Guard` with the
identical tuple `(provider, repository, capability, tags, ref/branch)`. The guard is the single
source of the allow/deny decision and of the `.noai` overlay; neither entry point re-implements
authorization. Entry-point-specific checks are **additive on top of** that same
repository/capability decision: the tools additionally validate paths and tool arguments, the
proxy additionally validates the pushed branch name, the default-branch lock and the
fast-forward relation. Where the git protocol cannot express a check the tools perform
(packfile paths, a per-ref fetch tag), the proxy does not relax the rule — it **fails closed**
and the limit is documented below. The proxy is therefore never more lenient than the tools for
the same configuration.

The branch decision itself lives in the policy: `Policy.EvaluateResourceBranch` (reached through
`Guard.AuthorizeBranch`) applies the `branches` filter of the matched grant, and the proxy holds
**no** branch allowlist of its own any more. The branch dimension is only evaluated where a
branch is known: operations without branch context (fetch, push discovery, repository-level
authorization) carry `branch == ""` and the branch filter imposes nothing there — otherwise a
`branches`-filtered grant could never complete discovery and no push would be possible at all.

## Decision

- New package `internal/gitproxy`. `Server` is constructed by `gitproxy.New(Config, registry,
  guard, logger)` and started from `main` when `server.git_proxy.enabled` is set. Transport is
  git smart-HTTP only: the four endpoints `<repo>.git/info/refs` (with `service=` query) and
  `<repo>.git/git-upload-pack` / `<repo>.git/git-receive-pack` (POST); everything else
  (`HEAD`, `objects/...`, service-less `info/refs`) is 404.
- **Route**: `/git/<provider>/<repo...>.git/<service...>`; the repository ends at the LAST
  `.git/`. Every path segment is validated against a deny list that includes URL-reserved
  characters (`? # @ : ; & = + $ , [ ] ' ( ) * !`, plus `\ %`, control characters and spaces):
  a decoded segment containing one would let the policy match diverge from the upstream URL
  (authorization bypass), so the route is rejected with 404 before the policy runs. Providers
  must likewise escape the repository path when building the remote URL (see below).
  The provider segment is resolved against the registry before authorization
  (unknown provider = 404, like any unsupported endpoint).
- **Authentication** has two modes, selected by `git_proxy.token`:
  - **With token**: HTTP Basic; the username is ignored, the password must equal
    `git_proxy.token`, compared with `crypto/subtle.ConstantTimeCompare`. Failures answer 401 with
    a `WWW-Authenticate: Basic` challenge. The proxy token is never logged and never forwarded
    upstream; the upstream receives only the provider's own header from `GitAuthHeader`.
  - **Without token**: the proxy authenticates nobody (`Server.AuthRequired` reports false and no
    `WWW-Authenticate` is ever sent) and therefore must bind **loopback only** — a non-loopback
    listen without a token is a configuration error (validated in config and again in
    `gitproxy.New`). Capability authorization, push policy and the `.noai` overlay still apply
    unchanged; only the client-facing credential gate is absent, so every local process on the
    host may use the proxy under the server's identity.
- **Clone URL discovery**: the `git_remote` MCP tool (registered only while the proxy is enabled)
  exposes `Server.RemoteURL` so an agent can learn the credential-free clone URL; the token itself
  is never returned by the tool. The tool's `auth` object mirrors `Server.AuthRequired`:
  `http-basic` with any username when a token is configured, `none` (loopback-only note) without.
- **Authorization before the provider call**, using the existing guard. Fetch
  (`info/refs?service=git-upload-pack`, `POST git-upload-pack`) needs `repo:read`; push discovery
  (`info/refs?service=git-receive-pack`, GET) needs `repo:write`. Both go through `Guard.Authorize`
  as **repository-level, branch-less** authorizations: the `.noai` overlay is checked on the
  **default branch** only, because the fetch protocol carries no ref the proxy could read the
  marker at before serving the whole tree, and a branch-less context applies no `branches` filter
  (see "One policy core").
- A **`POST git-receive-pack` push** is authorized **per target branch**. The order is: authenticate,
  route, resolve the provider, parse the pkt-line command section (no provider call, bounded to
  1 MiB), then authorize each unique branch whose name passes the coarse `validBranchName` check
  with `Guard.AuthorizeBranch(..., repo:write, {}, branch)`. A syntactically invalid branch name is
  **never handed to the guard** — its `.noai` marker read would hit the provider at an unvalidated
  ref — and is left to the push policy below, which rejects it with the same 403 the client saw
  before the validation moved up. Valid branches get the full check: that applies the grant's
  `branches` filter — **a `repo:write` grant without a `branches` filter denies every push**
  (fail-closed), exclude wins over include — and it checks the `.noai` marker on the **default
  branch and on the branch** and honors a `noai: allow` grant.
  Duplicate branch targets are authorized once; a non-branch ref (tag/notes) is authorized at
  repository level — without the branch dimension — and rejected later by the push policy. Only
  after authorization does the push policy read provider metadata (`DefaultBranch`/`ResolveRef`/
  `MergeBase`), so a repository the client may not write is denied before any such call.
- Git traffic carries no tag information and the proxy keeps no topic cache, so a grant with a tag
  constraint sees unknown tags and **fails closed** (same rule as invariant 7 — deliberately more
  conservative than the tools, never laxer). A grant with an active path filter is likewise denied
  fail-closed: the proxy passes an **empty path**, so the guard returns "path required". An active
  `paths` filter therefore means **no git access** for that capability (fetch, clone and push).
  Every authorization denial (fetch, discovery, push) answers with one identical 403 body, keeping
  `.noai` indistinguishable from an unknown repository (invariant 6).
- **Push policy** is enforced on the pkt-line command section of `git-receive-pack`
  (`ReadReceivePackCommands`). The section is parsed first (no provider call), each branch is then
  authorized against the `repo:write` **branches filter in the policy** (see the push flow above),
  and only afterwards does this policy run: every ref update must pass, or the whole push is
  denied with 403 before any byte is forwarded. The former global `git_proxy.branches.allow`
  allowlist is **gone** — which branches may be pushed is capability policy now. What remains here
  are the additive hard guardrails a per-capability filter cannot express:
  - only `refs/heads/<branch>` (no tags/notes);
  - coarse git-style branch name validation;
  - never the default branch (`DefaultBranch`) — hard-coded, not configurable;
  - no deletes (all-zero new object id);
  - updates must be a **fast-forward with an intact base**: `ResolveRef` gives the current tip,
    the advertised **old object id must equal that tip** (a mismatch is a stale base — a
    non-fast-forward/force push in disguise — and denies before any merge-base call), and
    `MergeBase(tip, new)` must equal the tip exactly (object ids are lowercase hex, so no
    case-insensitive comparison). Any metadata failure denies (fail-closed). The create case
    (all-zero old id) stays separate: nothing to fast-forward from, no merge-base call.
  The body is then reassembled with `io.MultiReader(bytes.NewReader(consumed), br)` so the
  upstream receives the original bytes exactly (Content-Length untouched).
- **Provider interface extension**: `GitRemoteURL`, `DefaultBranch` and `ResolveRef` join
  `Provider` (next to the phase-1 `GitAuthHeader`/`MergeBase`). `GitRemoteURL` returns the
  credential-free clone URL; it must build the URL with proper path escaping (the repository
  path is set as the parsed URL's `Path`, never concatenated as a string), so reserved characters
  cannot change the URL structure and no credential ever appears in a URL (ADR 0005 unchanged).
- **Forwarding hardening**: the `ReverseProxy` uses a dedicated transport (bounded dial, response
  header, TLS handshake and idle-connection timeouts; no whole-request timeout so packfiles can
  stream; HTTP/2 is not forced because git smart-HTTP is HTTP/1.1). Upstream 3xx responses are
  rejected in `ModifyResponse` and surface as 502: a `Location` must never reach the client, or
  the git client would resend its proxy credentials to the redirect target.
- **Config**: `server.git_proxy` with `enabled`, `listen` (default `127.0.0.1:8417`),
  `public_url`, optional `token` (secret handling per ADR 0005, `${NAME}` env references supported;
  an absent or blank token selects the unauthenticated **loopback-only** mode), optional
  `tls_cert`/`tls_key` (both or neither), and `allow_insecure` (explicit opt-in for plain HTTP on a
  non-loopback address). The former `branches.allow` section **was removed**: push branches are
  configured as the `branches` filter of the `repo:write` capability in the provider rules. A
  token-less proxy on a non-loopback `listen`, and plain HTTP
  with a token on a non-loopback `listen` without TLS, are hard configuration errors (validated in
  config and again in `gitproxy.New`); an unparsable listen host counts as non-loopback
  (fail-safe).

## Known MVP limitations

These are deliberate scope decisions, not bugs; they are listed so operators can weigh them:

- **`.noai` scope on git traffic**: a **push** checks the marker on the **default branch and on the
  target branch** (`AuthorizeBranch` per branch), so a marker present only on the pushed branch
  denies that push. A **fetch or push-discovery** request can only be checked on the **default
  branch**: the fetch protocol carries no ref the proxy could read the marker at before serving the
  whole tree, so ref-accurate enforcement there is not possible and the default branch is the
  fail-closed choice. A `noai: allow` grant skips the check on every ref.
- **Push discovery is branch-less context**: `GET info/refs?service=git-receive-pack` names no
  branch, so the `branches` filter does not apply there and the ref advertisement is served to any
  `repo:write` holder (as before this change). Only the subsequent `POST git-receive-pack` is
  authorized per unique target branch; discovery itself discloses branch names, not content.
- **Path filters are not enforceable on git traffic**: the proxy does not parse packfiles and a fetch
  is whole-tree, so `paths.include`/`paths.exclude` of a `repo:read`/`repo:write` grant cannot be
  applied. The proxy passes an **empty path** to the guard; an active path filter then matches no
  path and the request is denied **fail-closed** (the "path required" rule). An active `paths`
  filter therefore means **no git access** for that capability — it restricts the tools, not the
  proxy. Git traffic for a path-filtered grant is never more permissive than the tools.
- **Tag filters stay fail-closed in the proxy**: the proxy does not fetch repository topics or MR
  labels and keeps no cache, so any tag-constrained grant sees unknown tags and denies. This is
  deliberately **more conservative than the tools** (which fetch the tags before deciding) and is the
  chosen MVP bound; closing it would need a topic cache or a fresh per-request metadata fetch, both
  out of scope here.
- **No packfile size limit**: only the pkt-line command section is bounded
  (`maxCommandSection`). The pack body is streamed untouched, so a push can transfer an
  arbitrarily large pack, as it would against the provider directly.
- **Coarse branch name validation**: `validBranchName` approximates `git check-ref-format`
  (forbidden characters, `..`, leading/trailing and empty/dot segments) but is not a full
  implementation of every Git rule.
- **Timing side channel on denials**: a `.noai` denial performs one more provider round trip
  (the marker read) than an unknown or policy-denied repository, so response **timing** can
  differ even though the answer body and status stay identical (invariant 6).
- **Branch-policy denials are distinguishable**: a push whose capability authorization **passed**
  but that then fails an additive push guardrail (non-branch ref, invalid name, default branch,
  delete, stale base, non-fast-forward) answers "push rejected by branch policy" rather than the
  generic "repository is not accessible". This is accepted deliberately: the message can only
  ever appear for a repository the client is already allowed to write — push discovery and the
  per-branch `repo:write` authorization ran first — so it discloses a permission the caller has
  just exercised, not one it lacks, and no ref detail beyond "branch policy" is named. Every
  capability, tag, path, branch-filter and `.noai` denial still answers the one identical
  generic 403 (invariant 6).
- **No Git LFS**: the LFS endpoints (`<repo>.git/info/lfs/objects/batch` and friends) are not
  part of the served smart-HTTP surface and answer 404. Cloning or pushing a repository with
  LFS objects therefore fails once the client tries to transfer them — fail-closed by
  omission. LFS support is out of scope for the MVP.
- **TOCTOU on the push check**: the fast-forward decision (advertised base equals the tip, and
  the merge base with the new commit is the tip) is made against the branch tip read during
  `checkRefUpdate`; between that check and the forwarding of the body the branch can
  move on the provider. The upstream's compare-and-swap on the advertised **old object id**
  mitigates this: a push whose base moved is rejected by the provider instead of silently
  overwriting concurrent work. Accepted as a limitation of a stateless proxy.

## Consequences

- The proxy is a second, policy-governed entry point next to the MCP tools; it reuses the same
  guard and provider abstraction, so adding a backend (GitHub, Forgejo) means implementing the
  five git-capable provider methods and nothing else.
- Push authorization is `repo:write` **per target branch** through `Guard.AuthorizeBranch`: the
  grant's `branches` filter decides which branches may be pushed at all — **without a `branches`
  filter no push is allowed** (fail-closed) — and the marker is checked on the default branch and
  the branch (`noai: allow` honored). The default-branch lock, the delete ban and the
  fast-forward rule are the additional guardrails on top of that same capability decision, never
  a substitute for it; the former global proxy allowlist is gone.
- Path filters do not apply to git traffic and, because the proxy cannot enforce them, a
  path-filtered `repo:read`/`repo:write` grant is denied **fail-closed** rather than silently
  widened to whole-branch content. Path filtering remains a `read_file`/`write_file` concept.
- `.noai` remains an integrity control (invariant 11): the proxy denies non-exempt operations on
  marked repositories but an exempt `repo:read` still serves content, as everywhere else.
- The MCP tool `git_remote` (registered only while the proxy is enabled) returns the clone URL through
  `Server.RemoteURL`; the proxy token is never part of the tool output. It authorizes `repo:read` with
  the full guard (unknown tags fail closed, `.noai` overlay included) before calling the resolver, so
  the tool sees exactly what the proxy would allow for a fetch. The `auth` object reports `http-basic`
  when a token is configured and `none` (no authentication, loopback only) in the token-less mode.
