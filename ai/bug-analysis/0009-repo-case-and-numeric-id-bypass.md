# 0009. Repository path lacked case normalization and numeric-id rejection

## Symptom

A `deny` rule could be evaded by requesting a repository with different letter casing
or by its bare numeric GitLab project id. The operation then succeeded against the
repository the rule was meant to protect.

## Impact

- Deny-rule bypass. With `deny: team/secret` followed by a broad allow (e.g.
  `team/**` or `**`), a request for `team/SECRET` (or `Team/Secret`) did not match the
  case-sensitive deny literal but matched the allow glob; GitLab resolves project
  paths case-insensitively, so it returned `team/secret` all the same. The same held
  for a request that named the project by its numeric id under a `**` allow.
- The git smart-HTTP proxy had the identical divergence: the repository came from the
  URL path and was matched against the policy without lowercasing.

Not a data-leak *mechanism* on its own, but it defeated the central authorization
control (deny-by-default with explicit denies).

## Root cause

`validateRepo` (server) canonicalized only the path *syntax* (`path.Clean`); it never
lowercased the result, although `doublestar.Match` — and therefore every rule match —
is case-sensitive while GitLab resolves `namespace/project` case-insensitively
(`LOWER(routes.path) = LOWER(...)`) and stores it lowercase. It also accepted a
single-segment name, which GitLab's `GET /projects/:id` reads as a numeric project id
(`id | integer or string`). Bug `0005` named the `Group/Secret` vector as fixed, but
the fix did not touch casing, so the vector stayed open.

## Fix

- The canonical form is now provider-owned: `Provider.CanonicalRepository` returns it,
  and every server handler and both git-proxy call sites (`parseRoute`/`RemoteURL`) use
  its result for the policy decision and the provider call. The GitLab implementation
  lowercases and requires a namespaced path (`strings.Contains(repo, "/")`), rejecting
  the bare numeric-id form. `validateRepo` keeps only the provider-neutral structural
  checks.
- Configured repository patterns must match the provider's canonical form (lowercase for
  GitLab); a pattern that differs in case fails closed (deny), the safe direction.
- Tests: `TestCanonicalRepository` (GitLab), `TestRepoPathCanonicalization`,
  `TestRepoPathRejectsBareNumericID`, `TestRouteCanonicalizesRepoCase`,
  `TestRouteRejectsBareNumericRepo`.

## Lesson

Canonicalize an authorization key to the exact form the backend resolves *before*
matching — including case and alternative identifier forms — not merely its syntax.
See `ai/method-specs/authorization.md` and ADR 0011.
