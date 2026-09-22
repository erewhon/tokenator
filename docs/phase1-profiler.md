# Tokenator Phase 1 — the profiler

Status: design sketch (2026-07-16). M1–M4 and the OTel receiver are implemented; details below that were "to verify" are annotated where reality differed.

## Purpose

Answer, from data already on disk, the questions no existing tool answers:

1. **Composition** — what is my context window made of, per request, over the life of a session?
2. **Attribution** — which tool, MCP server, file, or subagent did my tokens go to?
3. **Cache efficiency** — how much of my input was served from cache, and when the cache missed, *why*?
4. **Waste** — which tokens bought nothing (repeat file reads, never-referenced tool results, dead MCP schemas)?

Pure observation. Phase 1 never modifies a request, a session, or a harness config.

## Non-goals (phase 1)

- No pruning, compression, or any intervention (phase 4).
- No live proxying (phase 2 — router `api_class: anthropic`, tracked in Forge under LLM Router).
- No independent benchmarking of third-party optimizers (phase 3).
- Not another spend dashboard: ccusage already does daily/monthly cost accounting well. We ingest the same data but answer different questions. Parity with ccusage totals is a *correctness check*, not the product.

## Billing regimes

The same token has a different meaning per source. Every report is regime-aware:

| Regime | Example | What matters |
|---|---|---|
| Subscription | Claude Code on Max (home) | Context-window headroom, context quality, 5-hour-window rate limits. Dollars are fiction. |
| Metered API | Bedrock at work, remote providers via router | Real dollars; cache economics dominate (read ≈0.1×, write 1.25–2×). |
| Local | LM Studio / SGLang via router | Tokens are latency and VRAM, not money. Throughput view. |

`sources` carry a `billing_regime`; cost columns are computed only where they mean something.

## Data sources

### 1. Claude Code transcripts (primary)

`~/.claude/projects/<project-slug>/<session-uuid>.jsonl`. Append-only event lines. Verified locally: assistant events carry full `message.usage` including `input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `output_tokens`, `server_tool_use`. Lines also carry `sessionId`, timestamps, parent links, sidechain/subagent markers, tool results, and full message content — enough for block-level attribution, not just totals.

To verify during implementation (schema drifts by CC version; ingest must be lenient):
- exact field names for sidechain/subagent linkage and compaction boundaries
- how queued operations, hook injections, and `<system-reminder>` content appear
- model + `speed`/`effort` fields per request

### 2. OpenCode storage (primary)

`~/.local/share/opencode/storage/`. Verified locally (v1.1.x):
- `session/<projectID>/<ses_*>.json` — id, slug, projectID, directory, title, created/updated, diff summary
- `message/<ses_*>/<msg_*>.json` — role, `parentID`, `providerID`/`modelID`, `agent`/`mode`, `cost`, `tokens {input, output, reasoning, cache {read, write}}`, `finish`
- `part/…` — message content parts (shape to confirm), `tool-output/`, `session_diff/`

OpenCode computes `cost` itself (models.dev pricing; known-broken for custom providers) — record it but recompute our own.

### 3. Claude Code OTel (implemented — `tokenator otel`)

An OTLP/HTTP **JSON-only** receiver (`internal/ingest/otel`): gRPC/protobuf would each add a dependency tree for data encoding/json already reads, and the user is setting OTel env vars anyway (`OTEL_EXPORTER_OTLP_PROTOCOL=http/json`). Live-only (no backfill), so it complements rather than replaces JSONL ingest; sessions/requests stay owned by the transcript ingesters and OTel rows join via session uuid and `request_id`.

Verified against CC 2.1.212 (console-exporter probe + live OTLP run) and current docs:

- **Metrics** → `otel_datapoint`: `claude_code.{session.count, token.usage, cost.usage, active_time.total, lines_of_code.count, ...}`. `token.usage`/`cost.usage` carry `model`, `type` (input|output|cacheRead|cacheCreation), `query_source`, and — only while the context is active — `agent.name`/`skill.name`/`plugin.name`/`mcp_server.name`/`mcp_tool.name` attribution (promoted to columns).
- **Events** (log records, body `claude_code.<name>`) → `otel_event`: `api_request` carries `request_id` + per-request tokens + **`cost_usd` + `duration_ms`** (neither exists in transcripts); `tool_result`/`tool_decision` carry `tool_name` + `tool_use_id` (joins `block.tool_use_id`); plus hook/MCP-connection/compaction/api_error events. `request_id` matches transcript `requestId` — live validation cross-checked 100% of api_request events against ingested request rows with identical token sums (`tokenator otel --status`).
- Dedupe makes temporality a non-issue: datapoint key = hash(metric, start_ts, attrs) — cumulative series update one row in place, delta series land as new rows; SUM(value) is correct either way.

### 4. Router reqlog (optional)

Postgres `reqlog` records from llm-router-go for the OpenAI-side (local + remote per-token) traffic. Read-only join: router requests ↔ OpenCode sessions via model/provider/timestamps. Becomes much richer after the `api_class: anthropic` + cache-fields task lands.

## Core model (SQLite)

One normalized event model across harnesses. The load-bearing entities:

```
source      (id, kind: claude_code|opencode|otel|reqlog, root_path/dsn, billing_regime)
session     (id, source_id, harness_session_id, project, cwd, title,
             started_at, ended_at, parent_session_id NULL)   -- subagents = child sessions
request     (id, session_id, seq, ts, model, provider,
             input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
             reasoning_tokens NULL, cost_usd NULL, latency_ms NULL, finish_reason,
             harness_msg_id)                                  -- one API call; usage is GROUND TRUTH
block       (id, session_id, kind, origin_type, origin_key, first_request_id,
             last_request_id, est_tokens, byte_len, content_hash)
             -- one context item: kind ∈ {system, tool_schema, mcp_schema, user_text,
             --   assistant_text, thinking, tool_use, tool_result, file_content,
             --   memory_injection, hook_injection, compaction_summary}
             -- origin: tool name / mcp server / file path / skill / hook
request_block (request_id, block_id, position)                -- composition of each request
tool_call   (id, request_id, tool_name, mcp_server NULL, input_bytes, result_block_id,
             duration_ms NULL, is_error)
compaction  (id, session_id, request_id, kind: auto|manual|microcompact,
             tokens_before, tokens_after)
```

### Attribution methodology (the hard part)

- **Request-level usage is exact** (harness-reported). Block-level tokens are **estimates**: bytes/4 baseline, better estimator pluggable later.
- **Two weightings** (implemented): *flow* counts each block once at entry size; *residency* (`attr --residency`) weights by est_tokens × requests the block stayed in context — what cache pricing actually scales with. Residency windows are timestamp-approximated: entry after the block's ts, evicted at the next compact boundary (microcompacts are invisible and overcount slightly), thinking stripped at its turn's end (next user_text). Calibration anchor for residency is metered prompt volume: Σ(input + cache_read + cache_creation) over requests. Real-data flip: flow says meta_text dominates (43%); residency says tool_result + tool_use do (76%, avg ~115 requests resident).
- **Calibration closes the gap**: per request, `sum(block est_tokens)` vs metered `input_tokens + cache_* fields` yields a session-level correction factor. Report attribution as percentages of metered totals, never as raw estimates — that keeps attribution honest even with a crude estimator.
- Never call the paid `count_tokens` API from the ingest path.

### Cache analysis (offline approximation)

Without the wire prefix (that's phase 2), infer from usage sequences within a session:
- steady state: `cache_read` ≈ previous request's total prompt → healthy
- **invalidation event**: `cache_creation` spike + `cache_read` drop → flag, then classify by diffing the reconstructed block sequence of request N vs N−1 (first divergent block names the culprit: changed system content, tool set change, edited history…)
- session-level score: actual cache reads vs best-possible (append-only oracle) — "you paid full price for X tokens that an append-only prefix would have served at 0.1×" (metered regimes only).

### Waste heuristics (each independently toggleable, each reported with evidence)

- **Repeat reads**: same `file_content` content_hash entering context >1× in a session.
- **Stale passengers**: tool_result blocks resident for many requests with no later reference (identifier-overlap heuristic; reported as "likely", not fact). *Implemented* at ingest time (content is never stored, so the judgment happens while it's in hand): tool results register distinctive identifiers (code-shaped tokens; path-shaped ones also register their basename since prefixes differ between citer and cited); later assistant text/thinking, tool-call input values, and human (not meta) user text consume them. Verdict on the block row: referenced / unreferenced / unknown — two distinct hits to call referenced, zero to call unreferenced, partial evidence abstains, identifiers claimed by >8 results are ambient and credit no one. Real-data: 81% of judged results referenced; 33% of judged resident tool-result tokens never were.
- **Oversized results**: tool results above a percentile threshold, grouped by tool — the "should rtk handle this?" report.
- **Schema overhead**: resident tool/MCP schema tokens vs how often each tool was actually called.

## CLI sketch

```
tokenator ingest [--watch]        # discover + ingest all sources into ~/.local/share/tokenator/tokenator.db
tokenator report [--project P] [--since 7d]   # regime-aware rollups incl. cache split
tokenator attr --by tool|mcp|file|agent|session [--residency]
tokenator session <id>            # TUI: composition timeline ("context flamegraph"),
                                  #   per-request drill-down, compaction markers
tokenator cache [session]         # cache efficiency score, invalidation events + causes
tokenator waste [--project P]     # the four heuristics, with evidence
tokenator export --html out/      # self-contained HTML (shareable / video-friendly)
tokenator otel [--listen ADDR]    # OTLP/HTTP JSON receiver; --status = capture summary
tokenator doctor                  # source discovery + schema-drift sanity checks
```

## Go layout

```
cmd/tokenator/         # thin wrapper: exit codes only
cli/                   # command line: flag parsing + subcommand wiring (cli.Run; importable by pitf)
internal/store/        # sqlite (modernc.org/sqlite — pure Go, single static binary), migrations
internal/ingest/       # Source interface: Discover() / Backfill() / Watch()
internal/ingest/claudecode/
internal/ingest/opencode/
internal/ingest/otel/  # OTLP/HTTP JSON receiver (tokenator otel)
internal/ingest/reqlog/
internal/estimate/     # token estimator + calibration
internal/analyze/      # composition, cache, waste, attribution
internal/report/       # text + HTML renderers
internal/tui/          # bubbletea
```

## Milestones

1. **M1 — parity**: ingest Claude Code + OpenCode; `tokenator report` totals reconcile with ccusage / OpenCode's own numbers. (Trust gate for everything after.)
2. **M2 — attribution**: block extraction + calibration; `attr` and `waste` reports.
3. **M3 — cache doctor (offline)**: invalidation detection + classification from usage sequences.
4. **M4 — visualization**: session TUI timeline + HTML export.

## Open questions

- Claude Code JSONL fields for compaction boundaries and subagent linkage — pin down against current CC version during M1; design ingest to skip-and-log unknown line types rather than fail.
- OpenCode `part/` shape and whether request-level history composition is reconstructable from parts alone (needed for OpenCode-side block attribution; if not, OpenCode gets totals-only in M1).
- Token estimator: bytes/4 with calibration may be enough forever (we report % of metered totals); revisit only if per-block absolute numbers start to matter.
- Pricing table source for metered regimes: vendor LiteLLM's pricing JSON (ccusage-style) with a pinned snapshot in-repo.
- 5-hour-window tracking for Max: worth including in `report` (ccusage has blocks; ours would add composition context). Cheap to add once requests are timestamped.
