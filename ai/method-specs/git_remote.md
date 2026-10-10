# git_remote

## Purpose

Return the credential-free git smart-HTTP proxy clone URL for a repository, together with the
client authentication mode.

## Inputs

| Field    | Type   | Required | Notes                          |
|----------|--------|----------|--------------------------------|
| provider | string | yes      | Logical provider name.         |
| repo     | string | yes      | Canonical `namespace/project`. |

## Required capability

`repo:read` — authorized with the full guard: unknown tags (so tag-constrained grants fail
closed) and the `.noai` overlay unless the grant is exempted with `noai: allow`.

## GitLab endpoint(s)

None (local URL resolution). The only provider call is the `.noai` marker check when the matched
`repo:read` grant is not exempted.

## Authorization

1. Validate and clean the repository path; reject absolute paths, backslashes, control characters.
2. Resolve the provider from the registry (`provider` is required).
3. `Guard.Authorize(repo:read)` — repository-level and branch-less, tags unknown, `.noai` checked
   on the default branch. The tool sees exactly what the proxy's own fetch gate would allow.
4. Only after authorization: `gitproxy.Server.RemoteURL(provider, repo)` builds the URL from the
   configured `public_url` as `<public_url>/git/<provider>/<repo>.git`, validating both name
   segments with the proxy's route rules.

## Behavior / limits

- Registered only while `server.git_proxy.enabled` is set (no resolver, no tool).
- Output: `{provider, repo, remote_url, auth}` with `auth = {type, username?, note}`:
  - `type: "http-basic"`, `username: "any"` when a git proxy token is configured; the password is
    that token and **the token is never returned** by this tool (nor by any other tool).
  - `type: "none"` in the token-less mode: loopback-only proxy, no client authentication.
- `remote_url` is credential-free; resolver validation details are logged server-side only.

## Errors

- `unknown provider "<p>"`; `access denied for repository "<r>"`; a `.noai` denial stays
  indistinguishable: `unknown repository "<r>" for provider "<p>"`.
- Marker-check failure: safe message `could not verify the .noai marker ...; access denied` with
  the numeric HTTP status (forbidden names the Repository: Read permission).
- Resolver failure: `could not determine the git remote URL`.
