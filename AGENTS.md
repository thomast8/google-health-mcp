# ghealth — Development Guide

## What is this

ghealth is a CLI wrapping the Google Health API v4, plus an MCP server that exposes the same reads to chat clients. Primary users are AI agents (OpenClaw, Claude Code, Codex, coding agents) that need structured health data access — steps, heart rate, sleep, exercise, weight, SpO2, HRV. Human developers use it too.

The CLI handles OAuth, pagination, response simplification, and contextual hints so agents can focus on the health question, not API plumbing.

## The MCP server is a wrapper, not a second implementation

`pkg/mcpserver` runs the CLI as a child process and returns its stdout unchanged. That is deliberate: filter construction, pagination, rollup range caps, simplification, `_hints` and the error envelope have one home, and an MCP tool result is byte-for-byte what the equivalent CLI command prints. It is also what makes concurrent tool calls safe, since the cobra tree keeps flag state in package-level variables and writes to `os.Stdout`.

Consequences to respect when changing things:

- Improve behaviour in `cmd/` or `pkg/output` and both surfaces get it. Never reimplement CLI logic inside `pkg/mcpserver`.
- A tool handler's only jobs are validating input against the registry and building argv. Flags are gated by operation so the child never receives a flag its subcommand has not registered.
- The MCP surface is read-only, and stays that way. It is built to be reachable over the network, where the blast radius of a bad call is someone's health record.
- HTTP mode fails closed without `GHEALTH_MCP_TOKEN`. Do not add a way to serve unauthenticated.
- Renaming a CLI flag or subcommand breaks the MCP layer silently — the argv is only checked at runtime. `pkg/mcpserver`'s tests assert the exact argv, and the end-to-end tests drive the real binary against a stub API; run them after any change to `cmd/`.

## Progressive disclosure

Information is layered so agents load only what they need:

1. `ghealth --help` → commands overview
2. `ghealth data --help` → list vs daily-rollup guidance, all 40 types
3. `ghealth data <type> --help` → available operations
4. `ghealth data <type> <op> --help` → flags for that operation
5. `ghealth schema types` → machine-readable type registry
6. `ghealth schema type <name>` → fields, parameters, scope for one type
7. `_hints` in responses → contextual next-step suggestions
8. `skills/ghealth/SKILL.md` → non-obvious patterns, gotchas (load once)

## Build

Go 1.25 or later (the MCP SDK requires it).

```bash
go build -o ghealth .
go vet ./...
go test ./...
```

Verify against the live API: `ghealth auth status`, then `ghealth data steps daily-rollup --from 2026-03-22 --to 2026-03-29`.

Verify the MCP server the same way — a passing test suite does not prove a client can talk to it:

```bash
npx @modelcontextprotocol/inspector ghealth mcp          # stdio
GHEALTH_MCP_TOKEN=$(openssl rand -hex 32) ghealth mcp --http   # then curl /healthz and /mcp
```

## Workflow

- Feature branches + PRs — never commit to main directly
- Only register data types verified against the live API, not from docs alone
- Test new types with curl before adding to the registry

## Where to make changes

| Task | Start here |
|------|-----------|
| Add/remove a data type | `pkg/types/registry.go` |
| Change response format | `pkg/output/simplify.go` |
| Add a contextual hint | `pkg/output/hints.go` |
| Change CLI flags or help text | `cmd/root.go` (globals), `cmd/data.go` (operations) |
| OAuth or auth flow | `pkg/auth/auth.go` |
| Add/change an MCP tool | `pkg/mcpserver/tools.go` |
| MCP transports, bearer auth, health check | `pkg/mcpserver/server.go` |
| MCP command flags and env handling | `cmd/mcp.go` |
| Headless credential bootstrap | `pkg/mcpserver/bootstrap.go` |
| Container or Railway deployment | `Dockerfile`, `railway.json` |

## Documentation to keep updated

| When you... | Update |
|-------------|--------|
| Add/remove a data type | `README.md` types table, `skills/ghealth/SKILL.md` |
| Change flags or commands | `README.md`, `skills/ghealth-shared/SKILL.md` |
| Add/change an MCP tool | `README.md` MCP tools table, the tool's own description string |
| Add an MCP environment variable | `README.md` MCP environment variables table, `cmd/mcp.go` help text |

## Reference

- [Google Health API docs](https://developers.google.com/health)
- [Health API setup guide](https://developers.google.com/health/setup)
- [skills/](skills/) — agent skill files
