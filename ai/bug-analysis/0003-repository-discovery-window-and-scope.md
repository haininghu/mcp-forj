# 0003. Repository discovery missed accessible repos and namespaces

## Symptom

`list_repositories` returned too few repositories or none for namespaces that
clearly existed. `devops/platform/**` found nothing, while the GitLab UI showed
projects under `devops/platform`. Some tokens also got `forbidden`.

## Impact

Discovery was unreliable: accessible repositories were hidden, and glob patterns
appeared broken even though listing was authorized.

## Root Cause

Several assumptions were wrong:

- Discovery hardcoded `membership=true`, which lists only projects the token's user
  is a **member** of. Tokens with group/ancestor access (common for fine-grained
  tokens) saw nothing.
- The query used one unfiltered `/projects` window; non-matching projects filled it,
  so matching repositories beyond the window were never seen.
- `/projects?search=<term>` matches only project `path`/`name`/`description` by
  default, **not** ancestor namespaces, so a prefix like `devops/platform` matched
  nothing.
- Even with `search_namespaces=true`, the User-boundary `/projects` endpoint often
  returns 403 for fine-grained tokens scoped to a group.

## Fix

- Added `project_scope` (`accessible` default, or `membership`) and pass the matching
  `membership` flag (no longer hardcoded).
- Derive search prefixes from the `repo:list` patterns
  (`Policy.ListSearchPrefixes`) and issue one call per prefix.
- Try the Group-boundary endpoint first:
  `GET /groups/:id/projects?include_subgroups=true`; on 404 fall back to
  `/projects?search=...&search_namespaces=true`.

## Lesson

Match the API boundary to the token's scope (Group vs. User), and search the
namespace, not just path/name/description. Prefer targeted prefixes over a broad
window. See ADR 0007.
