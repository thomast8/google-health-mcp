# ghealth

CLI and [MCP server](#mcp-server) for the [Google Health API v4](https://developers.google.com/health) — built for AI agents and developers.

- **40 verified data types**: steps, heart rate, exercise, sleep, weight, SpO2, HRV, ECG, blood glucose, nutrition, and more
- **Agent-first**: simplified JSON output, deterministic exit codes, `--dry-run`, `--raw`
- **MCP built in**: `ghealth mcp` for a local client, `ghealth mcp --http` for a remote connector
- **Single binary**: `go build -o ghealth .`

## Quick Start

```bash
ghealth setup                                              # One-time: GCP project + OAuth
ghealth data steps daily-rollup --from 2026-03-22 --to 2026-03-29  # Weekly step totals
ghealth data heart-rate list --from today --limit 10       # Recent heart rate readings
ghealth schema types                                       # See all available data types
```

## Requirements

Download and Install [Go](https://go.dev/doc/install) 1.25 or later (required by the MCP SDK).

## Installation

```bash
git clone https://github.com/Google-Health-API/google-health-cli.git
cd google-health-cli
go build -o ghealth .
```

## Setup

```bash
ghealth setup
```

Walks you through: GCP project ID, OAuth credentials (download from [Console](https://console.cloud.google.com/apis/credentials) — Desktop application type), Health API enablement, scope selection, and browser-based OAuth login.

Files written under `~/.config/ghealth/` (override with `GHEALTH_CONFIG_DIR`):

- `client_secret.json` — your OAuth client (mode 0600)
- `credentials.json` — access + refresh tokens (mode 0600, plaintext JSON)
- `config.toml` — active profile (project, scopes)

Tokens refresh automatically.

### Non-interactive setup (for agents / CI)

```bash
ghealth setup \
  --project-id my-project \
  --client-secret ~/Downloads/client_secret_123.json \
  --scopes-preset readonly \
  --skip-enable-api \
  --no-prompt
```

Add `--non-interactive-auth` to skip the browser step too — complete later with `ghealth auth login --complete <code>` (see below).

## Authentication

| Scenario | Method |
|----------|--------|
| Interactive | `ghealth setup` or `ghealth auth login` |
| Headless / no browser | `ghealth auth login --non-interactive` → click URL on any device → `ghealth auth login --complete <code>` |
| Move tokens between machines | `ghealth auth export` → `ghealth auth import` |
| Pre-configured token | `export GHEALTH_ACCESS_TOKEN=ya29...` |
| Credential file | `export GHEALTH_CREDENTIALS_FILE=/path/to/creds.json` |
| GCP environment | Application Default Credentials (automatic) |

Precedence: `GHEALTH_ACCESS_TOKEN` > `GHEALTH_CREDENTIALS_FILE` > stored credentials > ADC.

### Headless OAuth flow

```bash
# 1. On the host running ghealth:
ghealth auth login --non-interactive --scopes-preset readonly
# → JSON with auth_url (PKCE S256 challenge + random state baked in)
#   and a complete_command. pending_auth.json holds the verifier locally.

# 2. Open auth_url in any browser, click "Allow".
#    The browser will redirect to a localhost URL that fails to load — expected.
#    Copy either the full redirected URL or just the 'code' query parameter.

# 3. Back on the ghealth host (both forms work):
ghealth auth login --complete 'http://localhost/?code=4/0AX4XfWh...&state=cQq...'
ghealth auth login --complete 4/0AX4XfWh...
# → state validated, PKCE verifier sent on exchange, tokens persisted.
```

State mismatch (URL paste with the wrong `state` parameter) clears the pending flow and returns exit 2. The bare-code form skips state validation but still consumes the pending file, so a stale flow can't be replayed.

### Move tokens between machines

```bash
# source (already authenticated):
ghealth auth export > /tmp/ghealth-creds.json
scp /tmp/ghealth-creds.json target:

# target (also needs client_secret.json — either run 'ghealth setup --non-interactive-auth' or copy it):
ghealth auth import --file /tmp/ghealth-creds.json
```

### Bootstrap from a fresh machine (no client_secret yet)

When no OAuth `client_secret.json` is configured, every auth command returns a
structured error with a `next_steps` array — the same six steps every time —
so an agent can relay it to a user verbatim without scraping prose:

```bash
ghealth auth login
# → exit 5, JSON on stderr:
# {
#   "error": {
#     "type": "config", "code": 5,
#     "message": "No OAuth client_secret.json configured",
#     "hint":    "Run 'ghealth setup' to create or import OAuth credentials",
#     "next_steps": [
#       "Open https://console.cloud.google.com/apis/credentials",
#       "Create or select a Google Cloud project",
#       "Enable the Google Health API (...)",
#       "Create OAuth client ID with Application type: Desktop app",
#       "Download the client_secret JSON",
#       "Run: ghealth setup --client-secret /path/to/client_secret.json"
#     ]
#   }
# }
```

Same `next_steps` are emitted by:

- `ghealth auth login` (interactive / `--non-interactive` / `--complete`)
- `ghealth auth status` when neither stored creds nor env creds are present
- `ghealth auth refresh`, `ghealth auth export` (when nothing to refresh/export)
- `ghealth setup --no-prompt` when `--client-secret` is missing

To fetch the checklist **without** triggering an error (e.g. so an agent can
display the bootstrap steps before calling auth at all):

```bash
ghealth setup --instructions
# → exit 0, JSON on stdout with status: "instructions" and the next_steps array
```

## Data Types

40 types verified against the live API. Run `ghealth schema types` for the full list.

| Type | Key Values | Operations |
|------|-----------|------------|
| `steps` | *(use daily-rollup for `countSum`)* | list, rollup, daily-rollup, reconcile |
| `heart-rate` | `beatsPerMinute` | list, rollup, daily-rollup, reconcile |
| `exercise` | type, duration, calories, avgHeartRate, notes | list, get, create, update, delete, reconcile, export-tcx |
| `sleep` | minutesAsleep, minutesAwake, stageMinutes | list, get, create, update, delete, reconcile |
| `weight` | `weightGrams` | list, get, create, update, delete, rollup, daily-rollup, reconcile |
| `body-fat` | `percentage` | list, get, create, update, delete, rollup, daily-rollup, reconcile |
| `height` | `heightMillimeters` | list, get, create, update, delete, reconcile |
| `distance` | *(use daily-rollup for `millimetersSum`)* | list, rollup, daily-rollup, reconcile |
| `heart-rate-variability` | RMSSD | list, reconcile |
| `oxygen-saturation` | `percentage` (SpO2) | list, reconcile |
| `altitude` | altitude value | list, rollup, daily-rollup, reconcile |
| `active-zone-minutes` | `activeZoneMinutes`, `heartRateZone` | list, daily-rollup, reconcile |
| `activity-level` | SEDENTARY, LIGHT, MODERATE, VIGOROUS | list, reconcile |
| `basal-energy-burned` | `kcal` per interval (BMR) | list, reconcile |
| `active-energy-burned` | `kcal` per interval (activity) | list, rollup, daily-rollup, reconcile |
| `vo2-max` | VO2 max value | list, reconcile |
| `total-calories` | *(use daily-rollup for `kcalSum`)* | daily-rollup |
| `sedentary-period` | sedentary intervals | list, daily-rollup, reconcile |
| `swim-lengths-data` | `swimStrokeType`, `strokeCount` *(use daily-rollup for `strokeCountSum`)* | list, rollup, daily-rollup, reconcile |
| `hydration-log` | `milliliters` consumed | list, get, daily-rollup, reconcile |
| `nutrition-log` | nutrients, `energy`, mealType, food | list, get, rollup, daily-rollup, reconcile |
| `food` | nutrient profiles, servings *(catalog — no time filter)* | list, get |
| `food-measurement-unit` | `displayName` *(catalog — no time filter)* | list, get |
| `blood-glucose` | mg/dL, mealType, measurementTiming | list, get, rollup, daily-rollup, reconcile |
| `core-body-temperature` | `temperatureCelsius` | list, get, rollup, daily-rollup, reconcile |
| `electrocardiogram` | waveform, `resultClassification` *(requires `ecg.readonly`)* | list |
| `irregular-rhythm-notification` | alert windows *(requires `irn.readonly`)* | list |
| `daily-resting-heart-rate` | `beatsPerMinute` per day | list, reconcile |
| `daily-heart-rate-variability` | daily HRV summary | list, reconcile |
| `daily-oxygen-saturation` | daily SpO2 summary | list, reconcile |
| `daily-respiratory-rate` | daily respiratory rate | list, reconcile |
| `daily-vo2-max` | daily VO2 max | list, reconcile |
| `daily-sleep-temperature-derivations` | temp deviation from baseline | list, reconcile |
| `respiratory-rate-sleep-summary` | per-stage respiratory rate | list, reconcile |
| `run-vo2-max` | VO2 max from running | list, daily-rollup, reconcile |
| `floors` | *(rollup only — `countSum`)* | rollup, daily-rollup, reconcile |
| `active-minutes` | *(rollup only)* | rollup, daily-rollup, reconcile |
| `time-in-heart-rate-zone` | *(rollup only)* | daily-rollup, reconcile |
| `calories-in-heart-rate-zone` | *(rollup only — `caloriesInHeartRateZones` array per bucket)* | rollup, daily-rollup, reconcile |
| `daily-heart-rate-zones` | *(reconcile only)* | reconcile |

### Exercise track export

`export-tcx` writes the raw Google TCX, or — with `--as csv` — flattens it to one row per trackpoint (`time, activity, lap, sport, latitude_deg, longitude_deg, altitude_m, distance_m, heart_rate_bpm, cadence_rpm, speed_mps, watts`) for direct `pd.read_csv` consumption. Indoor activities have no track and yield a header-only CSV; their summary/notes live in `data exercise list`. Pass `--output -` to stream to stdout instead of a file.

```bash
ghealth data exercise export-tcx --id <id> --output ride.csv --as csv
ghealth data exercise export-tcx --id <id> --output - --as csv | head   # stream to stdout
```

## Usage

### Reading data

```bash
# Recent heart rate (sample-type: returns individual readings)
ghealth data heart-rate list --from today --limit 10

# Daily step totals for a week (rollup: returns aggregated counts)
ghealth data steps daily-rollup --from 2026-03-22 --to 2026-03-29

# Exercises this month
ghealth data exercise list --from 2026-03-01

# Weight history
ghealth data weight list --limit 20

# Sleep (summary by default, --detail for stage-by-stage breakdown)
ghealth data sleep list --limit 5
ghealth data sleep list --limit 5 --detail
```

Every read (`list`, `get`, `rollup`, `daily-rollup`, `reconcile`) returns the same JSON shape — an object `{"dataPoints": [...]}` with optional `_hints` and `nextPageToken` — so the rows are always under `dataPoints`.

`list` returns up to `--limit` rows (default 500). When more exist it includes a `nextPageToken`; pass it back with `--page-token` to fetch the next page losslessly (no rows skipped or repeated):

```bash
ghealth data heart-rate list --from 2026-06-15 --limit 500            # → {"dataPoints":[…], "nextPageToken":"ABC"}
ghealth data heart-rate list --from 2026-06-15 --limit 500 --page-token ABC
```

### Important: list vs daily-rollup

Some types (steps, distance) return **time intervals without values** from `list`. Use `daily-rollup` to get totals:

```bash
# This returns minute-by-minute intervals (no step count):
ghealth data steps list --from today --limit 5

# This returns daily totals with actual counts:
ghealth data steps daily-rollup --from 2026-03-22 --to 2026-03-29
# → {"dataPoints": [{"date": "2026-03-28", "countSum": "9037"}, ...]}
```

### Gotcha: missing days are NOT zeros

For the presence-aware types — `altitude`, `distance`, `floors`, `steps`, `total-calories` — a date that is **absent** from rollup output means the device was not worn (or did not sync) that day, **not** that the value was zero. A bucket with `countSum: "0"` is a **true zero**: the device was worn and genuinely recorded no activity.

- Missing date → render as "no data", never coalesce to 0
- `countSum: "0"` → true zero (worn, no activity)
- Never average over absent days as if they were zeros — that silently deflates weekly/monthly stats

### Filtering

```bash
ghealth data heart-rate list --from 2026-03-28                  # From date
ghealth data heart-rate list --from 2026-03-28 --to 2026-03-29  # Date range
ghealth data heart-rate list --from today --limit 50            # Today, max 50
ghealth data heart-rate list --from yesterday                   # Since yesterday
```

`--filter` passes a raw expression to the API (overrides `--from`/`--to`). Filter syntax follows [AIP-160](https://google.aip.dev/160) — interval types use `{type}.interval.civil_start_time` (ISO 8601, no `Z`), sleep uses `sleep.interval.civil_end_time` (only end-time is filterable), sample types use `{type}.sample_time.physical_time` (RFC-3339, with `Z`). Only `>=` and `<` comparators are supported.

### Writing data

Writable types: `exercise`, `sleep`, `weight`, `body-fat`, `height`.

Write operations are asynchronous — the API returns an Operation object. Use `list` to verify the data was persisted.

To discover the correct JSON format, inspect a real response: `ghealth data weight list --raw --limit 1`

```bash
# Create (use --raw list output to model the payload structure)
ghealth data weight create --json '{"weight": {"weightGrams": 75500, "sampleTime": {"physicalTime": "2026-03-29T10:00:00Z", "utcOffset": "3600s"}}}'

# Update (use --update-mask to specify which fields to change)
ghealth data weight update --id <id> --json '{"weight": {"weightGrams": 76000}}'

# Delete (accepts bare IDs or full resource names)
ghealth data exercise delete --ids 7649353586249326520
```

## Output

Responses are **simplified by default** — redundant timestamps, empty fields, and repeated metadata are stripped. Timestamps include the user's UTC offset (e.g., `+01:00`).

```bash
ghealth data heart-rate list --from today --limit 2
```
```json
{
  "dataPoints": [
    {"time": "2026-03-29T16:33:07+01:00", "beatsPerMinute": "80", "source": "Google Pixel Watch 4 (41mm)"},
    {"time": "2026-03-29T16:33:04+01:00", "beatsPerMinute": "80", "source": "Google Pixel Watch 4 (41mm)"}
  ]
}
```

```bash
ghealth data steps daily-rollup --from 2026-03-26 --to 2026-03-29
```
```json
{
  "dataPoints": [
    {"date": "2026-03-28", "countSum": "9037"},
    {"date": "2026-03-27", "countSum": "2408"},
    {"date": "2026-03-26", "countSum": "6474"}
  ]
}
```

| Flag | Effect |
|------|--------|
| `--raw` | Return the original API response with no simplification |
| `--format table` | Aligned columns |
| `--format csv` | CSV output (nested objects flatten to dot-separated columns) |
| `-o, --output <file>` | Write data to the file; print only a column schema + 3-row preview to stdout. Prefer this over `> file` (which gives the file but no schema) |
| `--dry-run` | Show the HTTP request without executing |

In `--format csv` and `--format table`, the data stream stays pure: `_hints` and a leftover `nextPageToken` are written to **stderr** rather than mixed into the rows, and an empty result emits an empty CSV (never a JSON object). Use `-o <file>` and the stderr signals together to page through a large export without polluting the CSV.

## AI Agent Skills

The repo ships 2 Agent Skills (`SKILL.md` files) — one for shared prerequisites (auth, setup, global flags) and one covering all 40 data types, operations, patterns, and gotchas.

```bash
# Install all skills at once
npx skills add https://github.com/Google-Health-API/google-health-cli

# Or pick only what you need
npx skills add https://github.com/Google-Health-API/google-health-cli/tree/main/skills/ghealth
npx skills add https://github.com/Google-Health-API/google-health-cli/tree/main/skills/ghealth-shared
```


Agents don't need to read the full skill file upfront. The CLI supports progressive self-discovery:

```bash
ghealth schema types              # What types exist? What operations?
ghealth schema type heart-rate    # Fields, parameters, scope for one type
ghealth data <type> --help        # What operations does this type support?
ghealth data <type> list --help   # What flags does this operation take?
ghealth --dry-run ...             # What HTTP request would this send?
```

## MCP Server

`ghealth mcp` serves the same data access over the [Model Context Protocol](https://modelcontextprotocol.io),
so a chat client can read your health data without a shell. Every tool runs the CLI as a child
process, so a tool result is byte-for-byte what the equivalent CLI command prints — filters,
pagination, rollup caps, simplification and `_hints` all behave identically.

The MCP surface is **read-only**. Creating, updating and deleting data stays in the CLI.

### Tools

| Tool | What it does |
|------|--------------|
| `list_data_types(category?)` | The data types this server can read and the operations each supports. The place to start. |
| `describe_data_type(data_type)` | Fields, operation parameters, filter template and OAuth scope for one type. |
| `query_data(data_type, operation, from?, to?, limit?, page_token?, filter?, window_size?, window_days?, id?, detail?, raw?)` | The workhorse. Operations: `list`, `get`, `rollup`, `daily-rollup`, `reconcile`. |
| `get_user_info(resource)` | `identity`, `profile`, `settings`, `irn-profile` or `paired-devices`. |
| `auth_status()` | Which credentials are in use, the account, granted scopes and token expiry. |
| `export_exercise_tcx(id, as?)` | One exercise's track as a trackpoint CSV (default) or raw TCX XML. |

### Local client (stdio)

```bash
claude mcp add ghealth -- ghealth mcp
```

Or, for any client that takes a JSON config:

```json
{
  "mcpServers": {
    "ghealth": { "command": "ghealth", "args": ["mcp"] }
  }
}
```

stdio mode uses whatever credentials the CLI already has, so run `ghealth setup` and
`ghealth auth login` first and check with `ghealth auth status`.

### Remote server: choosing an authentication mode

`--http` serves the MCP endpoint at `/mcp`, with an unauthenticated `/healthz` for the host's
probes. A deployment gets a public HTTPS URL the moment it boots, and that endpoint reads personal
health records, so **the server refuses to start over HTTP without one of these two modes**.

| | **Google sign-in** | **Shared token** |
|---|---|---|
| Who can use it | Anyone with a Google account | Whoever holds the token |
| Whose data they see | Their own | Yours |
| Client setup | Automatic (OAuth) | Paste a token |
| Set up | Google Cloud OAuth client | One env var |

Google sign-in is the mode to pick if anyone other than you will connect. The rest of this section
covers it; the shared-token mode is [below](#shared-token-single-account).

### Google sign-in (multi-user)

Each user signs in with their own Google account and sees only their own data. The server holds
their Google refresh token, mints its own short-lived tokens for the MCP client, and never passes
the client's token upstream.

**The Google client ID and secret identify the app, not a person.** They are how Google recognises
"this is your MCP server asking", in the same way a mobile app ships one client ID and still shows
each user their own account. Whose data a request reads is decided by the refresh token Google
issues when *that user* signs in and consents — one per person, sealed into the access token their
MCP client holds. So the shared client credentials serve everyone without anyone seeing anyone
else's data, and the operator's own health record is not reachable through the deployment at all
unless they sign in like any other user.

Google cannot act as the MCP authorization server directly — MCP clients need Dynamic Client
Registration (RFC 7591) and resource indicators (RFC 8707), and Google supports neither. So this
server *is* the authorization server the client registers with, and federates to Google behind it:

```
ChatGPT ──registers──▶ /oauth/register          (RFC 7591, open registration)
        ──authorize─▶ /oauth/authorize          (PKCE required, S256 only)
                      └─▶ this server's consent screen
                          └─▶ Google sign-in ─▶ /oauth/callback
        ◀──code──────  redirect back to the client
        ──exchange──▶ /oauth/token              (short-lived access + rotating refresh)
        ──MCP───────▶ /mcp                      (Authorization: Bearer …)
```

The consent screen is not decoration. Every MCP client shares this server's single Google OAuth
client, so Google may have consent already recorded and would wave a returning user straight
through. That screen names the requesting client and where it will send the user back, which is
what stops a client the user never chose from silently obtaining a code on their behalf — the
confused-deputy mitigation the MCP spec requires.

The Google OAuth client has to be told the exact URL Google will redirect back to, and that URL
contains a domain that does not exist until the service is deployed. So deploy first, get the
domain, then do the Google setup — not the other way round.

Nothing below needs Go or Docker locally: `railway up` uploads the directory and builds the
Dockerfile remotely.

<details>
<summary><strong>Working in Google Cloud Shell, or any shell with no browser?</strong></summary>

Two adjustments. First, install the CLI under `$HOME`, because Cloud Shell only persists your home
directory — a CLI installed into `/usr/local` disappears when the VM is recycled:

```bash
npm config set prefix "$HOME/.npm-global"
npm install -g @railway/cli
echo 'export PATH="$HOME/.npm-global/bin:$PATH"' >> ~/.bashrc
export PATH="$HOME/.npm-global/bin:$PATH"
railway --version
```

Second, `railway login` wants to open a browser it cannot reach. Use the device-code flow, which
prints a URL and a short pairing code to open on any other device:

```bash
railway login --browserless
```

If that reports `Unauthorized` even after the web page says success — a
[known CLI issue](https://station.railway.com/community/cli-login-loop-issue-railway-login-ra-2f09f81d)
— fall back to an account token from [railway.com/account/tokens](https://railway.com/account/tokens):

```bash
read -rsp 'Railway API token: ' RAILWAY_API_TOKEN; echo; export RAILWAY_API_TOKEN
railway whoami
```

`read -rsp` keeps the token out of your shell history. It lasts for the session only; re-export it
after a Cloud Shell restart rather than writing it to `~/.bashrc`.

Cloud Shell is already authenticated to gcloud, so enabling the API is one command:

```bash
gcloud config get-value project
gcloud services enable health.googleapis.com
```

</details>

<details>
<summary><strong>No terminal at all? The whole setup works from a phone.</strong></summary>

Every step has a web UI, so none of the CLI below is required. Deploying from GitHub is arguably
the better route anyway — it redeploys on every push.

**Deploy.** In the [Railway dashboard](https://railway.com/new): *New Project → Deploy from GitHub
repo*, authorise GitHub, pick the repo. If the code is on a branch rather than the default one, set
it under *Service → Settings → Source → Branch* and redeploy. Railway builds the `Dockerfile`
because `railway.json` names it explicitly, so there is nothing to configure about the build.

**Domain.** *Service → Settings → Networking → Generate Domain*. It asks for a target port:
answer **8000**. Railway does not read `EXPOSE` from a Dockerfile, which is why it has to ask. Add
`PORT=8000` as a service variable too, so a host-injected default cannot differ from what you
answered here — a mismatch shows up as a 502 with nothing in the logs to explain it. The port the
server actually bound is printed on startup.

**Variables.** *Service → Variables → New Variable*, one per row.

**Generating the secret without a shell.** Deploy with no `GHEALTH_MCP_SECRET` set and read the
logs: the startup error carries a freshly generated one to copy into *Variables*. The server never
runs on a secret it invented — it still fails closed — so this is a suggestion you pin, not a
default. Do not use a random-string website; it has then seen the key that seals every token the
server issues. If you would rather have a real shell on the phone,
[Cloud Shell](https://shell.cloud.google.com) runs in mobile Safari and gives you
`openssl rand -hex 32`.

**Skip `GHEALTH_MCP_PUBLIC_URL` on Railway.** Railway injects `RAILWAY_PUBLIC_DOMAIN` once a domain
exists and the server completes it into an origin itself, so there is one less URL to type
correctly.

**Checking it works.** Open the URLs in the browser instead of using `curl` — `/healthz` and
`/.well-known/oauth-authorization-server` both return JSON that renders fine.

**Google Cloud Console** works in mobile Safari, but the credentials screens are cramped; use
*Request Desktop Website* from the address-bar menu.

**ChatGPT must be set up on the web, not in the app** — the iOS app cannot create custom
connectors. Open `chatgpt.com` in Safari, request the desktop site, and add the connector there.
Once it exists it works in the iOS app on the same account.

</details>

**1. Deploy and get a domain.** `Dockerfile` and `railway.json` are included and set
`GHEALTH_MCP_HTTP=1`.

```bash
railway init
railway variables \
  --set "GHEALTH_MCP_TOKEN=$(openssl rand -hex 32)" \
  --set "PORT=8000"                                 # pin the port the domain will forward to
railway up
railway domain          # prints the generated <name>.up.railway.app
```

The temporary token exists only so the first deploy boots and its health check passes — the server
refuses to start with no authentication configured at all, and a crash-looping service is a
confusing thing to debug a domain out of. `railway up` never exposes a service publicly on its own;
`railway domain` is what creates the URL (or add a custom one with `railway domain example.com`).
The generated domain is stable, so it is safe to register with Google.

If you are asked for a **target port**, answer `8000` — Railway ignores a Dockerfile's `EXPOSE`, so
it cannot infer one. Setting `PORT=8000` explicitly, as above, keeps the port the server binds and
the port the domain forwards to from drifting apart; when they differ you get a 502 and no log line
explaining it. The startup log always states the address it bound:

```
ghealth mcp: serving Streamable HTTP on 0.0.0.0:8000/mcp (health: /healthz)
```

Confirm it is live before going further:

```bash
curl https://<your-domain>/healthz          # → {"status":"ok"}
```

**2. Create the Google OAuth client.** It must be a **Web application** client — the Desktop client
`ghealth setup` creates cannot receive a server-side redirect.

- Open [Google Cloud credentials](https://console.cloud.google.com/apis/credentials)
- Enable the [Google Health API](https://console.cloud.google.com/apis/api/health.googleapis.com)
- *Create credentials → OAuth client ID → Web application*
- Under **Authorized redirect URIs** add `https://<your-domain>/oauth/callback` — the domain from
  step 1, exactly, with no trailing slash
- On the consent screen (*Google Auth Platform*, or *APIs & Services → OAuth consent screen* in
  older console layouts): set **Audience** to External, add yourself under **Test users**, and add
  the scopes below under **Data access**

Copy the client ID and client secret.

The server requests these by default — `openid` and `email` to name the account, and read-only
access to each health category:

```
openid
email
https://www.googleapis.com/auth/googlehealth.activity_and_fitness.readonly
https://www.googleapis.com/auth/googlehealth.health_metrics_and_measurements.readonly
https://www.googleapis.com/auth/googlehealth.sleep.readonly
https://www.googleapis.com/auth/googlehealth.nutrition.readonly
https://www.googleapis.com/auth/googlehealth.profile.readonly
https://www.googleapis.com/auth/googlehealth.settings.readonly
https://www.googleapis.com/auth/googlehealth.location.readonly
https://www.googleapis.com/auth/googlehealth.ecg.readonly
https://www.googleapis.com/auth/googlehealth.irn.readonly
```

Google rejects an authorization request outright if **any one** of the scopes in it is unavailable
to the project, and `ecg` and `irn` are the ones a project is least likely to be granted. If sign-in
fails with `invalid_scope`, narrow the request with `GHEALTH_MCP_SCOPES` rather than editing the
code — bare suffixes are expanded, so this is short enough to type on a phone:

```
GHEALTH_MCP_SCOPES=activity_and_fitness.readonly,sleep.readonly,health_metrics_and_measurements.readonly
```

`openid` and `email` are always included whatever you list.

**3. Switch on Google sign-in.**

```bash
railway variables \
  --set "GHEALTH_MCP_GOOGLE_CLIENT_ID=<client-id>.apps.googleusercontent.com" \
  --set "GHEALTH_MCP_GOOGLE_CLIENT_SECRET=<client-secret>" \
  --set "GHEALTH_MCP_SECRET=$(openssl rand -hex 32)"
```

On Railway you can leave `GHEALTH_MCP_PUBLIC_URL` unset: `RAILWAY_PUBLIC_DOMAIN` is injected once a
domain exists and the server completes it into an origin. Set it explicitly on hosts that provide no
equivalent, or when serving from a custom domain that Railway does not know about.

Then drop the temporary token — `railway variable delete GHEALTH_MCP_TOKEN`, or remove it under
*Variables* in the dashboard. Google sign-in takes precedence over the shared token, so it stops
having any effect the moment the three Google variables are set; deleting it is tidiness, not a
step the flow depends on.

`GHEALTH_MCP_SECRET` seals every credential the server issues. **Keep it stable** — changing it
signs everyone out, which is also how you revoke access in a hurry. Without
`GHEALTH_MCP_PUBLIC_URL` the server derives its URLs from forwarded headers, which works but is
worth pinning; on Railway, `RAILWAY_PUBLIC_DOMAIN` is the fallback.

No database and no volume are needed in this mode: client IDs, authorization codes and tokens are
all self-contained encrypted blobs, so the server keeps working across redeploys.

Check the discovery document, and that the redirect URI it reports is the one you registered — the
server logs it on startup too:

```bash
curl https://<your-domain>/.well-known/oauth-authorization-server
railway logs | grep "redirect URI"
```

**4. Before other people can use it: Google verification.** Health scopes are *sensitive*, so
Google gates them. While your OAuth app's publishing status is **Testing**, only accounts you add
as test users can sign in, and there is a hard cap of 100 users. Going to **Production** requires
submitting the app for [sensitive scope verification](https://developers.google.com/identity/protocols/oauth2/production-readiness/sensitive-scope-verification);
until it is approved, users see an "unverified app" warning and the cap still applies. This is a
Google review process measured in weeks, not a configuration switch. Plan for it if "anyone" means
more than a hundred people you know.

### Shared token (single account)

Every caller presenting the token reads the one Google account the server itself is authenticated
as. Suitable for a private deployment; not for sharing. Deploying is step 1 above — `railway init`,
`railway up`, `railway domain` — and this mode needs no Google OAuth client of its own.

```bash
export GHEALTH_MCP_TOKEN=$(openssl rand -hex 32)
ghealth mcp --http
```

Credentials reach the container through the environment, since it has no browser and no
interactive login: `GHEALTH_CLIENT_SECRET_JSON` and `GHEALTH_CREDENTIALS_JSON`, each raw JSON or
base64. The server writes them into the config directory at startup and never overwrites files that
already exist.

```bash
ghealth auth export > /tmp/ghealth-creds.json
railway variables \
  --set "GHEALTH_MCP_TOKEN=$(openssl rand -hex 32)" \
  --set "GHEALTH_CLIENT_SECRET_JSON=$(base64 -w0 ~/.config/ghealth/client_secret.json)" \
  --set "GHEALTH_CREDENTIALS_JSON=$(base64 -w0 /tmp/ghealth-creds.json)"
```

On macOS `base64` has no `-w0`; use `base64 -i <file>`.

**Attach a volume at `/home/ghealth/.config/ghealth`** in this mode. Google can rotate the refresh
token and the CLI writes the new one to disk; on an ephemeral filesystem that rotation is lost on
redeploy and the original may stop working. Google sign-in mode does not need this — it holds no
credentials on disk.

### Add it in ChatGPT

Requires a Pro, Plus, Business, Enterprise or Education plan. **Do this in the web app** — the
mobile apps cannot create custom connectors, though they can use one once it exists. On a Business
or Enterprise workspace an admin may first have to allow it under *Workspace Settings → Permissions
& Roles → Connected Data → Create custom MCP connectors*.

1. Profile picture → **Settings** → **Apps** (labelled **Connectors** in some accounts; OpenAI has
   been renaming it)
2. Scroll to **Advanced settings** and enable **Developer mode**
3. Back in **Apps**/**Connectors**, click **Create**
4. Set the MCP server URL to `https://<your-domain>/mcp`
5. Choose **OAuth** for authentication and save

ChatGPT registers itself, then sends you through Google sign-in. Approve this server's consent
screen, sign in at Google, and the connector goes live. Ask *"what were my daily steps last week?"*
to confirm it works, or call `auth_status` to see which Google account you are connected as.

If you are still in Testing status, add the Google account you sign in with as a test user first,
or Google will refuse the sign-in.

In shared-token mode instead, pick the API-key / bearer-token option and paste the token.

### Add it in Claude

Requires a Pro, Max, Team or Enterprise plan. On Team or Enterprise an Owner adds it first under
*Organization Settings → Connectors*, then members connect individually.

1. [claude.ai/settings/connectors](https://claude.ai/settings/connectors) → *Add custom connector*
2. URL: `https://<your-domain>/mcp`
3. Leave the *Advanced settings* OAuth client ID and secret **blank** — those are for servers that
   cannot self-register. This one supports Dynamic Client Registration, so Claude registers itself.

**Nothing changes on the Google side.** Claude registers with this server, not with Google, and the
one Google redirect URI (`/oauth/callback`) is already registered. The same is true in reverse:
adding ChatGPT after Claude needs no reconfiguration either. Both clients are covered by tests.

Claude connects from Anthropic's cloud, so the server has to be reachable from the public internet —
it is, on any of the hosts above, but not if you are only running it on localhost.

### MCP environment variables

| Variable | Default | Meaning |
|----------|---------|---------|
| `GHEALTH_MCP_HTTP` | `0` | `1` selects Streamable HTTP instead of stdio (same as `--http`). |
| `HOST` / `PORT` | `0.0.0.0` / `8000` | Bind address for HTTP mode. A host that injects `PORT` overrides the default, so set it explicitly where the host also asks you for a target port. |
| `GHEALTH_MCP_TIMEOUT` | `120s` | Per-tool-call timeout, as a Go duration. |
| **Google sign-in** | | |
| `GHEALTH_MCP_GOOGLE_CLIENT_ID` | — | Web OAuth client ID. |
| `GHEALTH_MCP_GOOGLE_CLIENT_SECRET` | — | Web OAuth client secret. |
| `GHEALTH_MCP_SECRET` | — | Seals issued credentials; min 32 chars. Changing it signs everyone out. |
| `GHEALTH_MCP_SCOPES` | all read scopes | Comma- or space-separated Google scopes to request. Bare suffixes such as `sleep.readonly` are expanded. Use it to drop a scope the project is not granted. |
| `GHEALTH_MCP_PUBLIC_URL` | derived | Public origin, e.g. `https://ghealth.up.railway.app`. Optional on Railway, which supplies `RAILWAY_PUBLIC_DOMAIN`. |
| **Shared token** | | |
| `GHEALTH_MCP_TOKEN` | — | Bearer token granting access to the server's own account. |
| `GHEALTH_CLIENT_SECRET_JSON` | — | `client_secret.json` contents, raw JSON or base64. |
| `GHEALTH_CREDENTIALS_JSON` | — | `ghealth auth export` output, raw JSON or base64. |

Setting the Google variables enables multi-user mode and takes precedence over
`GHEALTH_MCP_TOKEN`. Setting only some of them is an error rather than a silent fallback.

### Security notes

- **Read-only.** No tool can create, update or delete health data.
- **Tokens are never passed through.** The MCP client's token and the user's Google token are
  separate; the server exchanges one for the other and validates that a token was issued for it.
- **Per-request isolation.** Each tool call runs the CLI as a child process holding exactly one
  credential — the caller's. Every other credential source is stripped from that process's
  environment, so a request without a session fails rather than falling back to the operator's own.
- **PKCE (S256) is mandatory**, authorization codes are single-use and expire in two minutes, and
  refresh tokens rotate on every use.
- **Revocation.** Users can disconnect at
  [myaccount.google.com/permissions](https://myaccount.google.com/permissions). Rotating
  `GHEALTH_MCP_SECRET` invalidates every outstanding token at once.

## Other Commands

```bash
ghealth user identity                    # User identity
ghealth user profile get                 # Profile (age, stride length)
ghealth user settings get                # Settings (timezone, units)
ghealth auth status                      # Auth state (scopes, expiry)
ghealth schema types                     # All data types + operations
ghealth schema type heart-rate           # Detail for one type
ghealth schema scopes                    # OAuth scopes
ghealth schema endpoints                 # All API endpoints
ghealth config show                      # Show active configuration (project, scopes, format)
ghealth config set timezone <IANA zone>  # Set a config value (keys: project_id, format, timezone)
ghealth webhooks subscribers list        # Manage push-notification subscribers / subscriptions
ghealth mcp                              # Run the MCP server over stdio (--http for a remote connector)
```

`webhooks` (subscribers, subscriptions, `verify`) manages project-level push notifications and requires the `cloud-platform` scope plus a configured project ID — see the skill docs for details.

Because a `cloud-platform` token is rejected by the data-plane endpoints, keep webhook credentials in a **separate config dir** from your health-data credentials. `auth login` always writes tokens to the active config dir, so use a dedicated `GHEALTH_CONFIG_DIR` for webhooks (not `GHEALTH_CREDENTIALS_FILE`, which only changes where tokens are *read* from):

```bash
export GHEALTH_CONFIG_DIR=~/.config/ghealth-webhooks
ghealth setup                              # its own client_secret + project
ghealth auth login --scopes cloud-platform
ghealth webhooks subscribers list
```

### Timezones

Date arguments (`--from`/`--to`, `today`, `yesterday`) resolve against exactly one timezone, chosen in this order:

1. the active profile's configured zone — `ghealth config set timezone <IANA zone>`
2. otherwise, the machine-local timezone

### Verifying tokens

`ghealth auth status` is a fast local check by default — it reports what's configured without making any network calls, and the `authenticated` field reflects local state only. For env-token / credentials-file modes it is omitted entirely (presence of a token doesn't prove validity).

```bash
ghealth auth status --validate
```

Verifies the access token against Google's `tokeninfo` endpoint. `authenticated` then reflects actual validity, and the response includes `expires_in` and `scope` from Google.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | API error (4xx/5xx) |
| 2 | Auth error (have client_secret, missing or invalid tokens — run `ghealth auth login`) |
| 3 | Validation error |
| 4 | Network error |
| 5 | Config error (often: no `client_secret.json` — error carries `next_steps` for bootstrap) |

Errors are always JSON on stderr and may include a `next_steps: []string` array for multi-step recovery (currently only emitted when no OAuth client is configured).

## Environment Variables

| Variable | Purpose |
|----------|---------|
| `GHEALTH_ACCESS_TOKEN` | Direct access token |
| `GHEALTH_CREDENTIALS_FILE` | Path to credential JSON |
| `GHEALTH_CONFIG_DIR` | Config directory override |
| `GHEALTH_PROFILE` | Active profile name |
| `GHEALTH_FORMAT` | Default output format (json/table/csv) |
| `GHEALTH_BASE_URL` | Override the API base URL |

The MCP server adds [its own variables](#mcp-environment-variables).

## License

Apache 2.0 — see [LICENSE](LICENSE.md).
