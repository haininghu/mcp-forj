# ai/ — agent documentation

This directory holds the durable, human-readable rationale for `mcp-forj`: why the
system is shaped the way it is, which bugs were fixed and what they taught us, and
the exact contract of every MCP tool. Prefer these documents over `git log`.

## Contents

| Folder             | Purpose                                                                 |
|--------------------|-------------------------------------------------------------------------|
| `adr/`             | Architecture Decision Records: one decision per file, immutable once accepted. |
| `bug-analysis/`    | Symptom -> root cause -> fix -> lesson for real defects.                 |
| `method-specs/`    | Specification of each MCP tool and the shared authorization pipeline.    |

## How to add

- **A decision** (`adr/NNNN-short-title.md`): copy the Status/Context/Decision/Consequences
  shape from `adr/0001-record-architecture-decisions.md`. ADRs are numbered sequentially
  and are **not edited** after acceptance; supersede them with a new ADR instead.
- **A bug** (`bug-analysis/NNNN-short-title.md`): follow the template in
  `bug-analysis/README.md` (Symptom, Impact, Root cause, Fix, Lesson).
- **A tool** (`method-specs/<tool_name>.md`): follow the template in
  `method-specs/README.md` (Purpose, Inputs, Required capability, GitLab endpoint(s),
  Authorization, Behavior/limits, Errors).

## Ground rules

- English, concise, factual. Markdown lines <= 120 characters.
- No secrets, tokens, response bodies, or invented features.
- Keep the docs in sync with the code: if behavior changes, update the matching spec
  or ADR (or add a bug analysis) in the same change.

Start with `../AGENTS.md` for the repo map, commands and invariants.
