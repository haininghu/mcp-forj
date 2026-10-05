# 0001. Record architecture decisions

## Status

Accepted.

## Context

`mcp-forj` is a security-sensitive authorization layer. The reasoning behind its
shape (deny-by-default, `.noai` scope, provider abstraction, error hygiene) is easy
to lose: commit messages and issue threads are not a reliable memory, and future
agents need to know *why* a rule exists before changing it.

## Decision

Record every significant, hard-to-reverse decision as an **Architecture Decision
Record** under `ai/adr/`.

Format: a short Markdown file named `NNNN-short-title.md` with these sections:

- **Status** — `Proposed`, `Accepted`, `Superseded by NNNN`, or `Deprecated`.
- **Context** — the forces at play (constraints, requirements, risks).
- **Decision** — what we chose, stated in the active voice.
- **Consequences** — what becomes easier or harder, and any follow-ups.

Rules:

- Number sequentially, starting at `0001`.
- One decision per file; keep each ADR <= 60 lines.
- An accepted ADR is **immutable**. To change course, add a new ADR and mark the
  old one `Superseded by NNNN`.
- ADRs describe intent; `ai/method-specs/` describes the resulting tool behavior.

## Consequences

- The rationale survives refactors and is reviewable in one place.
- Contributors must write a new ADR for semantic changes instead of silently editing
  the old one, which keeps the history honest.
- Slightly more overhead per change; accepted because authorization correctness is
  the core of the product.
