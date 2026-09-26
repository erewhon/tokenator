# Deploying `tokenator serve`

Handoff notes for wiring the tokenator web UI into the homelab. Written for
the homeops agent; the homelab plumbing guide
(`~/Projects/erewhon/homeops/HOMELAB-ACCESS-FOR-AGENTS.md`) governs anything
about front doors, DNS, and SSO.

## What it is

A single static Go binary serving plain HTTP (no TLS, **no auth**) over the
tokenator SQLite database: session browser, content search, transcript and
profile views. It makes **no outbound network calls** — search reads local
transcript files on demand.

```
tokenator serve [--listen 127.0.0.1:8990] [--db PATH] [--scan-limit 80]
```

Build/install from the repo (`~/code/tokenator`): `just install` → puts the
binary at `~/.local/bin/tokenator` (build needs Go; the binary itself has no
runtime deps).

## Where it must run — data locality

Everything it reads lives under **erewhon's home on the box where the coding
sessions happen** (euclid since 2026-09; delphi before that), so it must run
there, as that user. Do not containerize it away from this data. The live
deploy is `homeops/config/euclid/tokenator-serve/` (units, nftables lock,
README); the delphi one is kept as history.

| Path | Access | Purpose |
|---|---|---|
| `~/.local/share/tokenator/tokenator.db` (+ `-wal`/`-shm`) | read/write | the database (WAL sidecars) |
| `~/.claude/projects/` | read-only | Claude Code transcripts (content search/reading) |
| `~/.local/share/opencode/storage/` | read-only | OpenCode transcripts (JSON tree, OpenCode ≤1.1x) |
| `~/.local/share/opencode/opencode.db` | read-only | OpenCode transcripts (SQLite, OpenCode 1.18+); `ingest` and `serve` read whichever exists, both if both do |

The natural shape is a **systemd user service**, matching the existing
`tokenator-otel` unit (`~/.config/systemd/user/tokenator-otel.service`,
linger already enabled):

```ini
# ~/.config/systemd/user/tokenator-serve.service
[Unit]
Description=Tokenator web UI (session browser)
Documentation=file:///home/erewhon/code/tokenator/docs/serve.md

[Service]
ExecStart=%h/.local/bin/tokenator serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

## Exposure — auth is mandatory off-loopback

The server has **no authentication**. Default bind is `127.0.0.1:8990`;
anything beyond loopback must sit behind the hub front door with Zitadel
SSO (native or the shared `oauth2-proxy`), per the homelab access guide.
Transcripts contain everything Claude has seen — code, secrets that leaked
into tool output, private notes — so treat the UI as
sensitive-by-construction.

Constraints for the front-door wiring:

- The hub Caddy on delphi runs in a container; it cannot reach the host's
  `127.0.0.1`. Either bind the service to an address the hub can reach
  (`--listen <mesh-or-bridge-ip>:8990`) — **only** with SSO in front and,
  ideally, a firewall rule limiting the port to the hub — or use whatever
  host-gateway pattern the hub already uses for host services.
- Plain HTTP/1.1, server-rendered pages. No websockets, no SSE, no state:
  nothing special needed in the proxy config.
- Responses can be large (a transcript page can be several MB); don't set
  tight proxy body/response limits.

## Behind the router dashboard

The router dashboard reverse-proxies tokenator at `/tokens/` (llm-router-go
`--dashboard-tokens-url`). Nothing to configure here: the proxy strips the
prefix and announces it with `X-Forwarded-Prefix`, and every URL a page
emits carries it. A proxy that keeps the prefix instead needs
`serve --base-path /tokens`. The dashboard's Tokens tab loads pages with
`?embed=1` (sticky via cookie; `X-Tokenator-Embed: 1` works too), which
drops tokenator's own chrome, repaints the palette to the dashboard's, and
turns "router requests" / "catalog" links into `postMessage` jumps
(`{type:"tokenator-jump", tab, session|model}`) the shell routes through its
hash grammar. Standalone pages are unchanged.

## JSON API

The same data the four pages render, for the router dashboard's native
Tokens tab: `GET /api/sessions?q=&project=&since=&limit=`,
`GET /api/session/{key}`, `GET /api/session/{key}/transcript?q=&offset=&limit=`
(paged, 500 entries per page by default), `GET /api/model/{name}?limit=`.
Envelope `{"data": …}` / `{"error": "…"}`, `Cache-Control: no-store`, same
(non-)auth as the pages; served under the base path too.

## Data freshness (optional but recommended)

`serve` only reads the DB; ingestion is separate. Today ingest runs
manually. A user timer running every ~15 min keeps the browser current:

```
tokenator ingest            # Claude Code + OpenCode transcripts
tokenator reqlog            # gateway rows from the router reqlog Postgres
```

- Concurrent ingest-while-serving is safe: WAL journal mode,
  `busy_timeout=5000`, single-connection pools on both sides. Concurrent
  ingest-while-**otel**-receives is safe since schemaV8: the gateway pairing
  pass runs in short per-chunk transactions over an indexed usage tuple
  (before, one 11-minute transaction that any otel write aborted with
  SQLITE_BUSY_SNAPSHOT).
- **NFS homes:** SQLite WAL does not work over NFS. Put the DB on local disk
  and symlink `~/.local/share/tokenator` to it (euclid does this).
- `tokenator reqlog` needs `TOKENATOR_REQLOG_DSN`:
  `postgres://router:<pw>@192.168.42.20:5433/router` (the hekaton incus proxy
  into the `reqlog-pg` instance; the old euclid:5433 docker Postgres is gone) —
  password via `ho secret get llm-router/reqlog-pg-password`.
  If the timer omits reqlog, only wire-level cache data lags; the browser
  itself needs just `ingest`.

## Sizing / behavior notes

- Memory/CPU are negligible at rest. A content search byte-scans up to
  `--scan-limit` (default 80) newest candidate sessions' files (~1 GB in
  ~150 ms observed); raise the limit only if searches need to reach older
  sessions by default.
- No config file; flags only. Logs to stdout/journal.
