# 0008. `list_repositories` disclosed the number of hidden repositories

## Symptom

`list_repositories` returned an `omitted` field next to `repositories` and
`truncated`, counting candidates that matched a rule but were blocked by a tag filter,
the `.noai` marker, or a marker-check failure.

## Impact

The count is an aggregate existence oracle: it tells the client that more repositories
exist than it can see, even though their names are not revealed. It complements the
`.noai` existence oracle fixed in bug analysis 0007.

## Root cause

The handler accumulated `omitted` across tag filtering, marker omission and marker-check
failures and serialized it in `listRepositoriesOutput` (`server.go`).

## Fix

Removed `omitted` from the `list_repositories` and `list_merge_requests` results. The
counters are kept internally and logged server-side (provider, returned, omitted,
truncated). The method specs and README now state that hidden items are not counted in
the output. `truncated` stays: it reflects the caller's own limit/window, not policy
omissions.

## Lesson

Do not return aggregate counts of data the caller is not allowed to see. Server-side logs
are the right place for "how many were filtered". This applies to any `omitted`-style
field, not just repository listing.
