# Method specs

Precise contracts for the MCP tools exposed by `mcp-forj`, plus the shared
authorization pipeline (`authorization.md`). Use these to align the server code,
tests, and configuration documentation.

## Index

| File                          | Subject                                                    |
|-------------------------------|------------------------------------------------------------|
| `authorization.md`            | Shared pipeline: policy, tags, paths, `.noai` marker.      |
| `list_configured_rules.md`    | Config-only rule introspection.                            |
| `list_repositories.md`        | Configured + discovered repositories.                      |
| `list_merge_requests.md`      | List MR metadata with client-side label filtering.         |
| `get_merge_request.md`        | Fetch one MR (incl. rebase/merge status).                  |
| `list_merge_request_notes.md` | List MR notes.                                             |
| `get_merge_request_diff.md`   | Fetch MR file diffs.                                       |
| `add_merge_request_note.md`   | Create an MR note.                                         |
| `rebase_merge_request.md`     | Trigger an asynchronous MR rebase.                         |
| `read_file.md`                | Read a repository file.                                    |

## Template

```markdown
# <tool_name>

## Purpose
One sentence.

## Inputs
| Field | Type | Required | Notes |

## Required capability
Capability name, or "none".

## GitLab endpoint(s)
Method and path (e.g. `GET /projects/:id/merge_requests`).

## Authorization
Pre-check, metadata fetch for tags, tag evaluation, `.noai` (if any), fail-closed.

## Behavior / limits
Bounds, truncation, `omitted`/`truncated`, what is returned vs. never returned.

## Errors
Safe messages: not found, forbidden (+ required permission), invalid state, status.
```

Keep each spec <= 80 lines, English, factual, and free of secrets or response
bodies. If a tool changes, update its spec in the same change.
