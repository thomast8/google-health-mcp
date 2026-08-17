# ghealth — Development Guide

## What is this

ghealth is a CLI wrapping the Google Health API v4, plus an MCP server that exposes the same reads to chat clients. Primary users are AI agents (OpenClaw, Claude Code, Codex, coding agents) that need structured health data access — steps, heart rate, sleep, exercise, weight, SpO2, HRV. Human developers use it too.

The CLI handles OAuth, pagination, response simplification, and contextual hints so agents can focus on the health question, not API plumbing.

## The MCP server is a wrapper, not a second implementation

`pkg/mcpserver` runs the CLI as a child process and returns its stdout unchanged. That is deliberate: filter construction, pagination, rollup range caps, simplification, `_hints` and the error envelope have one home, and an MCP tool result's text block is byte-for-byte what the equivalent CLI command prints. It is also what makes concurrent tool calls safe, since the cobra tree keeps flag state in package-level variables and writes to `os.Stdout`.

Consequences to respect when changing things:

- Improve behaviour in `cmd/` or `pkg/output` and both surfaces get it. Never reimplement CLI logic inside `pkg/mcpserver`.
- A tool handler's only jobs are validating input against the registry and building argv. Flags are gated by operation so the child never receives a flag its subcommand has not registered.
- Tools that answer in JSON return the same bytes twice: as the text block, and as `structuredContent` validated against the tool's declared output schema (`pkg/mcpserver/schemas.go`). Set the text block explicitly — left to itself the SDK fills it in from the structured value, which has been through a map and comes back alphabetised, and the byte-for-byte property is gone. Change what the CLI prints and the schemas have to keep up; `TestEndToEndResultsCarryValidatedStructuredContent` is what tells you they have not.
- The output schemas stay permissive: nothing required, additional properties allowed. `raw: true` returns the untouched API response, and a schema that rejected it would turn a working read into a validation error. Their job is to name and explain the fields an agent will meet, not to police them.
- Hints are surface-aware. The suggestion is shared; the phrasing is not. A hint has to name a step its reader can take, and an MCP client has no shell to run `ghealth data ... --detail` in, so `pkg/output/hints.go` renders each hint as a command line for `SurfaceCLI` and as a tool call for `SurfaceMCP`. The MCP server sets `GHEALTH_SURFACE=mcp` on its children — in both auth modes — and nothing else sets it. Add a hint and you add both phrasings; the MCP one is asserted to contain no CLI syntax.
- The MCP surface is read-only, and stays that way. It is built to be reachable over the network, where the blast radius of a bad call is someone's health record.
- HTTP mode fails closed without `GHEALTH_MCP_TOKEN`. Do not add a way to serve unauthenticated.
- Renaming a CLI flag or subcommand breaks the MCP layer silently — the argv is only checked at runtime. `pkg/mcpserver`'s tests assert the exact argv, and the end-to-end tests drive the real binary against a stub API; run them after any change to `cmd/`.

## Multi-user mode

Over HTTP the server has two authentication modes, and refuses to start without one. `GHEALTH_MCP_TOKEN` is single-account: every caller reads the server's own Google account. Google sign-in (`pkg/mcpauth`) is multi-user: this server becomes the OAuth 2.1 authorization server the MCP client registers with, and federates to Google behind it. Google cannot fill that role itself — MCP clients need Dynamic Client Registration and resource indicators, and Google supports neither.

Rules that are not negotiable, each with tests that fail if broken:

- **A request with no session gets no data.** `ExecRunner.PerRequestAuth` gives a child process exactly one credential — the caller's — and strips every other source from its environment (`credentialEnv`). It must never fall back to the operator's credentials. See `pkg/mcpserver/tenancy_test.go`.
- **The consent screen stays.** Every MCP client shares one Google OAuth client, so Google may skip its own consent for a returning user. Ours names the requesting client and its redirect URI; removing it reintroduces the confused-deputy vulnerability the MCP spec calls out.
- **The client's token is never forwarded upstream.** The MCP access token and the Google token are separate, and inbound tokens are checked against this server's own resource URI.
- **PKCE S256 only**, exact redirect-URI matching, single-use codes, rotating refresh tokens.
- **Credentials are stateless.** Client IDs, codes and tokens are AEAD-sealed blobs, sealed under distinct kinds so one can never be replayed as another. That is what removes the need for a database on an ephemeral host; the trade-off is that revocation is by expiry or by rotating `GHEALTH_MCP_SECRET`.

Changing anything in `pkg/mcpauth` warrants a mutation check, not just a green suite: break the rule deliberately and confirm a test catches it.

## Progressive disclosure

Information is layered so agents load only what they need:

1. `ghealth --help` → commands overview
2. `ghealth data --help` → list vs daily-rollup guidance, all 40 types
3. `ghealth data <type> --help` → available operations
4. `ghealth data <type> <op> --help` → flags for that operation
5. `ghealth schema types` → machine-readable type registry
6. `ghealth schema type <name>` → fields, parameters, scope for one type
7. `_hints` in responses → contextual next-step suggestions, phrased for the surface that will read them
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
| Add a contextual hint (both surfaces) | `pkg/output/hints.go` |
| Change CLI flags or help text | `cmd/root.go` (globals), `cmd/data.go` (operations) |
| OAuth or auth flow | `pkg/auth/auth.go` |
| Add/change an MCP tool | `pkg/mcpserver/tools.go` |
| Change an MCP output schema | `pkg/mcpserver/schemas.go` |
| MCP transports, auth mode wiring, health check | `pkg/mcpserver/server.go` |
| Per-caller credential isolation | `pkg/mcpserver/exec.go` |
| OAuth endpoints, consent screen, PKCE | `pkg/mcpauth/provider.go` |
| Google federation, token cache, replay guard | `pkg/mcpauth/google.go` |
| Sealed-credential format | `pkg/mcpauth/crypto.go` |
| MCP command flags and env handling | `cmd/mcp.go` |
| Headless credential bootstrap (single-account) | `pkg/mcpserver/bootstrap.go` |
| Container or Railway deployment | `Dockerfile`, `railway.json` |

## Documentation to keep updated

| When you... | Update |
|-------------|--------|
| Add/remove a data type | `README.md` types table, `skills/ghealth/SKILL.md` |
| Change flags or commands | `README.md`, `skills/ghealth-shared/SKILL.md` |
| Add/change an MCP tool | `README.md` MCP tools table, the tool's own description string, its output schema in `pkg/mcpserver/schemas.go` |
| Change what the CLI prints for a read | `pkg/mcpserver/schemas.go`, `README.md` Output section, `skills/ghealth/SKILL.md` |
| Add or reword a hint | both surface phrasings in `pkg/output/hints.go` |
| Add an MCP environment variable | `README.md` MCP environment variables table, `cmd/mcp.go` help text |
| Change the OAuth flow or its guarantees | `README.md` Security notes, the multi-user rules above |

## Reference

- [Google Health API docs](https://developers.google.com/health)
- [Health API setup guide](https://developers.google.com/health/setup)
- [skills/](skills/) — agent skill files
