# Bug analyses

Short, factual write-ups of real defects in `mcp-forj`: what users saw, why it
happened, how it was fixed, and the lesson that should shape future code. These are
more useful than commit messages because they capture the *wrong assumption*.

## Index

| #    | Title                                                                 |
|------|-----------------------------------------------------------------------|
| 0001 | `list_merge_requests` failed closed under an `mr:read` label filter.  |
| 0002 | `read_file` omitted `ref`, so the `.noai` check failed on every call. |
| 0003 | Repository discovery missed accessible repos and namespaces.          |
| 0004 | MR note endpoints require the Work Item permission, not Merge Request.|
| 0005 | Repository path was not canonicalized and `read_file` ref bypassed `.noai`. |
| 0006 | List truncation was never reported and other provider/handler gaps.   |
| 0007 | The `.noai` denial message disclosed repository existence.           |
| 0008 | `list_repositories` disclosed the number of hidden repositories.     |

## Template

```markdown
# NNNN. Short imperative title

## Symptom
What the user/agent observed. Include the error text if useful.

## Impact
Who was affected and how badly (fail-closed vs. wrong result vs. leak).

## Root cause
The precise wrong assumption or code path, with file/function references.

## Fix
What changed: files, functions, and the resulting behavior.

## Lesson
The general rule to avoid repeating it; link related ADR/spec.
```

Keep each analysis <= 60 lines, English, and free of secrets or response bodies.
Prefer linking to the relevant `ai/adr/` or `ai/method-specs/` document.
