# 0006. Capability filters (tags and paths)

## Status

Accepted. Path-filter key names were later nested under `paths`.

## Context

A repository grant is sometimes too coarse. We needed per-capability constraints:
"I can comment only on MRs labelled `ai-reviewed`" and "I can read only `docs/**`".
The constraints must fail closed and must not leak the data they inspect.

## Decision

Extend the compact capability form with a single-key mapping carrying an optional
filter:

```yaml
capabilities:
  - mr:comment:
      require: [ai-reviewed]      # tags: MR labels
      exclude: [do-not-touch]
  - repo:read:
      require: [ai-ok]            # tags: project topics
      paths:
        include: ["docs/**", "*.md"]   # doublestar, repo-relative file path
        exclude: ["**/.env"]
```

`policy.CapabilityFilter` holds `Require`, `Exclude` and `Paths`
(`PathFilter{Include, Exclude}`). Semantics:

- Tags are matched by **exact, case-sensitive** equality. For `mr:*` they are MR
  labels; for `repo:*` they are project topics. Unknown tag information fails closed.
- `paths.include` empty allows all paths; otherwise the path must match one.
  `paths.exclude` must match none and **wins** over include. An active path filter
  with an empty path fails closed. Matching uses `doublestar` (case-sensitive).
- Path filters are valid only on `repo:read`/`repo:write`; other capabilities reject
  them at config load.
- Tags and paths **combine**: both must pass. Filters are accepted on any known
  capability; filters on `deny` rules are rejected (deny rules carry no capabilities).
- Tags/paths are authorization inputs only: never cached, never returned, and never
  logged.

The guard exposes `HasFilter`, `HasTagConstraint`, `EvaluateResource` and
`AuthorizeResource`; the server fetches tags only when a tag constraint is active,
and passes the already-validated cleaned path for `read_file`.

## Consequences

- Grants can be scoped precisely without new capabilities.
- Every filtered operation may incur a metadata fetch (topics); fetched on demand.
- Config validation is strict: unknown keys, duplicate keys, both-list intersections,
  empty entries and invalid globs are rejected.
