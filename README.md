# mcp-forj

A policy-governed [Model Context Protocol](https://modelcontextprotocol.io) server
that gives AI agents controlled access to code hosting providers.

The first iteration supports **GitLab** (multiple instances) with read and
merge-request-comment tools. GitHub and the internal "Forjo" system are planned and
are already accounted for by the provider abstraction.

Access is **deny-by-default** and configured per provider and repository.
Repositories containing a `.noai` marker file are completely off limits.

See [`docs/DESIGN.md`](docs/DESIGN.md) for the full design and
[`configs/config.example.yaml`](configs/config.example.yaml) for a documented
configuration.

## Status

Early first draft. The API and configuration format may still change.

## Build

```bash
make build        # -> bin/mcp-forj
make test         # run the test suite
make cover        # coverage report
```

Requires Go 1.27+.

## Run

```bash
cp configs/config.example.yaml configs/config.yaml
export GITLAB_WORK_TOKEN="<your-token>"
./bin/mcp-forj -config configs/config.yaml
```

The server speaks MCP over stdio. Point your MCP client at the binary.

## Tools

| Tool                       | Required capability | Description                             |
|----------------------------|---------------------|-----------------------------------------|
| `list_configured_rules`    | –                   | List configured rules and capabilities. |
| `list_repositories`        | – (`repo:list`¹)    | List configured repos, plus discovered ones.|
| `list_merge_requests`      | `mr:read`           | List merge requests.                    |
| `get_merge_request`        | `mr:read`           | Fetch one merge request.                |
| `list_merge_request_notes` | `mr:read`           | List comments on a merge request.       |
| `add_merge_request_note`   | `mr:comment`        | Comment on a merge request.             |
| `rebase_merge_request`     | `mr:rebase`         | Trigger an asynchronous MR rebase.      |
| `read_file`                | `repo:read`         | Read a repository file.                 |

## Configuration

See `configs/config.example.yaml`. Capabilities:

- `repo:list` – discover repositories through the provider API. Repositories listed
  literally in the config are always returned even without this capability.
- `repo:read` – read repository files.
- `mr:read` – view merge request metadata and notes (no diffs).
- `mr:comment` – comment on merge requests.
- `mr:rebase` – trigger an asynchronous rebase of a merge request.
- `mr:diff` – read merge request diffs (reserved, not used in v1).
- `mr:write` – reserved (creating/updating/merging MRs).
- `repo:write` – reserved.

¹ `list_repositories` returns concrete repositories listed literally in the
configuration (unless a `deny` rule hides them) and needs no capability for that.
The `repo:list` capability additionally allows discovery of repositories matching
glob patterns through the provider API; without it only the configured repositories
are returned and the call still succeeds. Exception: if the matched allow rule
carries an active `repo:list` topic filter, that filter also applies to the literal
config repositories — their topics are fetched and they may be omitted (counted in
`omitted`) on error or non-match. Without such a filter, literal repositories are
returned with no provider API call. The `limit` argument bounds only the discovered
repositories; static repositories are always returned and may push the total above
`limit`. Listing does not check the `.noai` marker, so a `.noai` repository may appear
in a listing; the marker still blocks every operation on it.

### Tag filters

Any capability may optionally carry a tag filter, written as a single-key mapping in
the `capabilities` list. For MR capabilities the tags are GitLab MR **labels**; for
repo capabilities they are project **topics**:

```yaml
capabilities:
  - mr:read
  - mr:comment:
      require: [ai-reviewed]      # MR must have ALL of these labels
      exclude: [do-not-touch]     # MR must have NONE of these labels
  - repo:list:
      require: [ai-ok]            # project must have this topic
  - repo:read:
      exclude: [confidential]     # project must not have this topic
```

Tags are matched by exact, case-sensitive equality. Whenever tag information cannot
be determined for an active filter, the decision fails closed.

Limitations:

- MR filters are enforced on `get_merge_request`, `list_merge_request_notes` (both
  `mr:read`), `add_merge_request_note` (`mr:comment`) and `rebase_merge_request`
  (`mr:rebase`); these tools fetch the merge request metadata to evaluate labels.
  `list_merge_requests` does **not** evaluate labels (the list API returns none) and
  fails closed when an `mr:read` tag filter is active.
- `rebase_merge_request` is a write operation: it triggers an **asynchronous** rebase
  (the outcome appears later on the merge request) and requires push access to the
  source branch. The token must have that access; the server fetches MR metadata for
  authorization before requesting the rebase (fail-closed).
- Repo filters are enforced on `read_file` (`repo:read`) and `list_repositories`
  (`repo:list`) using project topics. A `repo:read` filter fetches topics before
  reading. A `repo:list` filter also applies to repositories listed literally in the
  config; when a literal repo has an active `repo:list` filter its topics are fetched
  and it is omitted on error or non-match. Literal repos with **no** active filter
  are returned without any provider call. `repo:write` has no tool; filters there are
  accepted but inert.
- No caching and no glob/regex matching; topics come from `ListProjects`.

The `token` field accepts either a literal secret or `${NAME}` references expanded
from the environment, e.g. `token: "${GITLAB_WORK_TOKEN}"`. The resolved token is
never logged. It must be able to read repository files on the default branch so the
`.noai` check works; otherwise all access is denied (fail-closed).

## License

TBD.
