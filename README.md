# tokenator

A token and prompt-cache profiler for AI coding agents. tokenator ingests
session transcripts from **Claude Code** and **OpenCode** into a local SQLite
database, then answers the questions the harnesses don't: where the tokens
went, what the cache is doing, which tools and files dominate your context,
and whether configuration A actually beats configuration B.

Everything runs locally. Block *content* is never stored — only sizes,
hashes, and structure — so the database is a profile, not a copy of your
conversations.

## Install

```sh
# Homebrew (macOS)
brew install --cask erewhon/tap/tokenator

# Go
go install github.com/erewhon/tokenator/cmd/tokenator@latest

# From source
just build   # or: go build -o bin/tokenator ./cmd/tokenator
```

## Quick start

```sh
tokenator ingest             # slurp Claude Code + OpenCode transcripts
tokenator report             # usage rollups with cache splits
tokenator report --by model  # ... by project, model, or session
tokenator session <id>       # single-session timeline (--html for a report)
tokenator serve              # web UI: session browser, content search
```

The database lives at `$XDG_DATA_HOME/tokenator/tokenator.db` (override with
`--db`). Re-running `ingest` is incremental.

## What it does

- **`report`** — input/output/cache-read/cache-write rollups, estimated cost
  under subscription or metered billing.
- **`attr` / `waste`** — block-level attribution: which tools, MCP servers,
  and files consume your context; repeat reads; oversized results;
  long-resident heavyweights (`--residency` weights by tokens × requests
  kept in context).
- **`cache`** — the cache doctor: finds invalidation events and their causes,
  scores cache reuse per session.
- **`session`** — one session's timeline, composition, and events, in the
  terminal or as a self-contained HTML report.
- **`serve`** — a localhost web UI over all of it: session browser, full-text
  content search across raw transcripts, and per-session drill-down.
  Loopback-only by default; it has **no authentication**, so put it behind
  your own auth if you expose it (see `docs/deploy-serve.md`).
- **`bench`** — an A/B harness: run the same task through multiple arms
  (harness + configuration variants) × N trials in clean isolated sessions,
  grade with check commands, and compare pass rate, duration, tokens, and
  cost. Trials consume real API/subscription usage. See `docs/bench.md`.
- **`otel` / `reqlog`** — wire-level taps: an OTLP/HTTP receiver for Claude
  Code telemetry, and an ingester for request logs from an upstream gateway,
  including prefix-hash chains for wire-truth cache diagnosis
  (`cache --wire`). See `docs/phase2-gateway.md`.

## Docs

- [docs/phase1-profiler.md](docs/phase1-profiler.md) — profiler design:
  ingestion, attribution, the cache doctor
- [docs/phase2-gateway.md](docs/phase2-gateway.md) — gateway/OTel wire taps
- [docs/serve.md](docs/serve.md) — the web UI
- [docs/bench.md](docs/bench.md) — the A/B bench harness
- [examples/bench/](examples/bench/) — ready-to-adapt bench specs

## License

[AGPL-3.0](LICENSE)
