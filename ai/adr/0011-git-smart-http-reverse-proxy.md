# 0011. Git smart-HTTP reverse proxy

## Status

Accepted.

## Context

The MCP tools give an agent structured read/write access to merge requests and files, but an
agent sometimes needs real `git clone`/`push` against allowed branches. Granting direct provider
git credentials would bypass policy entirely. The MVP therefore embeds an authenticated git
smart-HTTP **reverse proxy** in the server: a separate HTTP listener that adds capability
authorization, branch constraints and fast-forward enforcement between the git client and the
provider. Repositories and branches are filtered; **paths are not** (path filters stay a
`repo:read`/`repo:write` tool concept).

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
- **Authentication**: HTTP Basic; the username is ignored, the password must equal
  `git_proxy.token`, compared with `crypto/subtle.ConstantTimeCompare`. Failures answer 401 with
  a `WWW-Authenticate: Basic` challenge. The proxy token is never logged and never forwarded
  upstream; the upstream receives only the provider's own header from `GitAuthHeader`.
- **Clone URL discovery**: the `git_remote` MCP tool (registered only while the proxy is enabled)
  exposes `Server.RemoteURL` so an agent can learn the credential-free clone URL; the token itself
  is never returned by the tool.
- **Authorization before the provider call**, using the existing guard: fetch
  (`info/refs?service=git-upload-pack`, `POST git-upload-pack`) needs `repo:read`; push
  (`info/refs?service=git-receive-pack`, `POST git-receive-pack`) needs `repo:write`. The `.noai`
  overlay applies. Git traffic carries no tag information, so a grant with a tag constraint sees
  unknown tags and **fails closed** (same rule as invariant 7). All denials answer with one
  identical 403 body, keeping `.noai` indistinguishable from unknown repositories (invariant 6).
- **Push policy** is enforced on the pkt-line command section of `git-receive-pack`
  (`ReadReceivePackCommands`, phase 1). Every ref update must pass, or the whole push is denied
  with 403 before any byte is forwarded:
  - only `refs/heads/<branch>` (no tags/notes);
  - coarse git-style branch name validation;
  - branch must match `git_proxy.branches.allow` (doublestar globs, default `ai/**`; an empty
    list allows nothing);
  - never the default branch (`DefaultBranch`);
  - no deletes (all-zero new object id);
  - updates must be a **fast-forward**: `ResolveRef` gives the current tip and
    `MergeBase(tip, new)` must equal the tip exactly (object ids are lowercase hex, so no
    case-insensitive comparison). Any metadata failure denies (fail-closed).
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
  `public_url`, `token` (secret handling per ADR 0005, `${NAME}` env references supported),
  optional `tls_cert`/`tls_key` (both or neither), `allow_insecure` (explicit opt-in for plain
  HTTP on a non-loopback address), and `branches.allow` (validated doublestar globs; an explicit
  empty list is a config error, not a default). Plain HTTP on a non-loopback `listen` without TLS
  is a hard configuration error (validated in config and again in `gitproxy.New`); an
  unparsable listen host counts as non-loopback (fail-safe).

## Known MVP limitations

These are deliberate scope decisions, not bugs; they are listed so operators can weigh them:

- **`.noai` scope on git traffic**: the marker is checked on the repository's **default branch**
  (the guard's `ref=HEAD` file read), not on the branch being pushed. A push to a feature branch
  of a repository whose `.noai` marker exists only on that branch is still governed by the
  default-branch decision.
- **No packfile size limit**: only the pkt-line command section is bounded
  (`maxCommandSection`). The pack body is streamed untouched, so a push can transfer an
  arbitrarily large pack, as it would against the provider directly.
- **Coarse branch name validation**: `validBranchName` approximates `git check-ref-format`
  (forbidden characters, `..`, leading/trailing and empty/dot segments) but is not a full
  implementation of every Git rule.
- **Timing side channel on denials**: a `.noai` denial performs one more provider round trip
  (the marker read) than an unknown or policy-denied repository, so response **timing** can
  differ even though the answer body and status stay identical (invariant 6).
- **No Git LFS**: the LFS endpoints (`<repo>.git/info/lfs/objects/batch` and friends) are not
  part of the served smart-HTTP surface and answer 404. Cloning or pushing a repository with
  LFS objects therefore fails once the client tries to transfer them — fail-closed by
  omission. LFS support is out of scope for the MVP.
- **TOCTOU on the push check**: the fast-forward decision is made against the branch tip read
  during `checkRefUpdate`; between that check and the forwarding of the body the branch can
  move on the provider. The upstream's compare-and-swap on the advertised **old object id**
  mitigates this: a push whose base moved is rejected by the provider instead of silently
  overwriting concurrent work. Accepted as a limitation of a stateless proxy.

## Consequences

- The proxy is a second, policy-governed entry point next to the MCP tools; it reuses the same
  guard and provider abstraction, so adding a backend (GitHub, Forgejo) means implementing the
  five git-capable provider methods and nothing else.
- Push authorization is repository-level `repo:write`; the branch allowlist plus fast-forward
  rule is the additional guardrail, not a substitute for the capability.
- No path filtering on git traffic: `repo:write` grants whole-branch content. Path filters remain
  a `read_file` concept. This is deliberate for the MVP.
- `.noai` remains an integrity control (invariant 11): the proxy denies non-exempt operations on
  marked repositories but an exempt `repo:read` still serves content, as everywhere else.
- The MCP tool `git_remote` (registered only while the proxy is enabled) returns the clone URL through
  `Server.RemoteURL`; the proxy token is never part of the tool output. It authorizes `repo:read` with
  the full guard (unknown tags fail closed, `.noai` overlay included) before calling the resolver, so
  the tool sees exactly what the proxy would allow for a fetch.
