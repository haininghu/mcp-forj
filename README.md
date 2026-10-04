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

| Tool                       | Required capability | Description                          |
|----------------------------|---------------------|--------------------------------------|
| `list_repositories`        | –                   | List configured repositories.        |
| `list_merge_requests`      | `mr:read`           | List merge requests.                 |
| `get_merge_request`        | `mr:read`           | Fetch one merge request.             |
| `list_merge_request_notes` | `mr:read`           | List comments on a merge request.    |
| `add_merge_request_note`   | `mr:comment`        | Comment on a merge request.          |
| `read_file`                | `repo:read`         | Read a repository file.              |

## Configuration

See `configs/config.example.yaml`. Capabilities:

- `repo:read` – read repository files.
- `mr:read` – view merge request metadata and notes (no diffs).
- `mr:comment` – comment on merge requests.
- `mr:diff` – read merge request diffs (reserved, not used in v1).
- `mr:write` – reserved.
- `repo:write` – reserved.

Tokens are supplied via environment variables named by `token_env`; they are never
written to the config file and never logged. The token must be able to read
repository files on the default branch so the `.noai` check works; otherwise all
access is denied (fail-closed).

## License

TBD.
