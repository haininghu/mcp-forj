# 0004. Provider abstraction

## Status

Accepted.

## Context

GitLab is the first backend, but GitHub and an internal "Forgejo" system are
planned. Policy and server code must not depend on GitLab specifics, and adding a
backend should not require touching authorization logic.

## Decision

Define a narrow, provider-neutral interface in `internal/provider`:

```go
type Provider interface {
    Name() string
    Type() string
    ListRepositories(ctx, opts RepoListOptions) ([]Repository, error)
    GetRepositoryTopics(ctx, repo string) ([]string, error)
    ListMergeRequests(ctx, repo string, opts ListOptions) ([]MergeRequest, error)
    GetMergeRequest(ctx, repo string, number int64) (*MergeRequest, error)
    ListMergeRequestNotes(ctx, repo string, number int64, opts ListOptions) ([]Note, error)
    AddMergeRequestNote(ctx, repo string, number int64, body string) (*Note, error)
    ListMergeRequestDiffs(ctx, repo string, number int64) ([]DiffFile, error)
    RebaseMergeRequest(ctx, repo string, number int64) error
    ReadFile(ctx, repo, path, ref string) ([]byte, error)
    FileExists(ctx, repo, path, ref string) (bool, error)
}
```

Supporting pieces:

- Neutral types (`Repository`, `MergeRequest`, `Note`, `DiffFile`, options) and
  sentinel errors (`ErrNotFound`, `ErrForbidden`, `ErrInvalidState`, `HTTPError`).
- A `Registry` keyed by logical name and a `Factory` registered per `type`.
  Because `internal/provider/gitlab` imports `internal/provider`, the factory uses
  a registration hook (`provider.RegisterFactory`) that the gitlab package installs
  in `init`, avoiding an import cycle.
- `number` is the provider-native identifier: GitLab uses the project-scoped `iid`.

Config selects a backend with `type: gitlab`; the server only sees the interface.

## Consequences

- Adding GitHub/Forgejo means implementing `Provider` and registering a factory;
  policy and tools are unchanged.
- Providers map native errors to the neutral sentinels so the server can produce
  safe, consistent messages.
- The interface is intentionally small; new operations require an ADR/spec update.
