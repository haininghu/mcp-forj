# read_file

## Purpose

Read a repository file at an optional ref.

## Inputs

| Field    | Type   | Required | Notes                                              |
|----------|--------|----------|----------------------------------------------------|
| provider | string | yes      | Logical provider name.                             |
| repo     | string | yes      | Canonical `namespace/project`.                     |
| path     | string | yes      | Repository-relative path; traversal is rejected.   |
| ref      | string | no       | Git ref; empty means the default branch (`HEAD`).  |

## Required capability

`repo:read` (accepts tag filters on project topics and path filters).

## GitLab endpoint(s)

`GET /projects/:id/repository/files/:file_path?ref=HEAD`

Fine-grained permission: **Repository: Read**.

## Authorization

1. Validate the path (ordered traversal checks) and clean it. Validate and clean the
   repository path; validate the optional `ref` (control characters and a length bound).
2. Resolve the provider.
3. Policy-only pre-check: `Guard.AuthorizeRepoCapability(repo:read)`.
4. If the matched grant has a tag constraint, fetch project topics
   (`GetRepositoryTopics`); a topic-only failure denies. Path-only filters make no
   topic call.
5. `Guard.AuthorizeResourceRef(repo:read, tags, cleanedPath, ref)` — tag and path checks
   plus the `.noai` marker check, unless the matched grant is exempted with `noai: allow`.
   The marker is checked at the default branch and, when `ref` names a different revision,
   also at that ref; present or failed denies (fail-closed).
6. Read the file and return it. The content is bounded before conversion to a string.

## Behavior / limits

- Path filters: `paths.include` empty allows all; otherwise the path must match one;
  `paths.exclude` must match none and wins; an active filter with an empty path fails
  closed. Matching is doublestar, case-sensitive.
- Content is capped at 1 MiB; a trailing `\n[truncated]` marker is appended and the
  result's `truncated` is set.
- Output: `{provider, repo, path, ref, content, truncated}`. No labels/topics.

## Errors

- `not found (HTTP 404)`;
  `forbidden: reading repository files needs the Repository: Read permission (HTTP 403)`;
  `.noai` marker messages (forbidden cause named when known); generic provider
  failure with `(HTTP <n>)`.
