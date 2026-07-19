# Phase 2: the gateway tap

Phase 1 reconstructed context from transcripts. Phase 2 observes it on the
wire: Claude Code's `ANTHROPIC_BASE_URL` points at the LLM router
(`https://llm.bcc.sh`), whose `api_class: anthropic` passthrough
(llm-router-go ≥ v0.6.1) forwards `/v1/messages` verbatim — client
credentials untouched, so subscription OAuth billing survives — and logs one
row per request to the reqlog Postgres (`router_requests` on euclid:5433):
usage with cache splits parsed from the response (SSE tee or JSON body), and
a **prefix hash chain** — cumulative sha256 digests (16 hex chars per
segment) over the rendered prompt in cache order: tools → system → each
message. Hashes only; content never persists outside the request.

## `tokenator reqlog`

Pulls `router_requests` into `gw_request` (schema v5), incrementally by
origin row id. All api classes are ingested (the table also carries the
fleet's OpenAI-shape traffic); the anthropic class is the phase-2 focus.

    export TOKENATOR_REQLOG_DSN="postgres://router:$(ho secret get llm-router/reqlog-pg-password)@euclid.m.bcc.sh:5433/router"
    tokenator reqlog            # pull new rows + match
    tokenator reqlog --status   # capture summary

## Pairing wire rows with transcript rows

The router cannot see harness session ids, and its `request_id` is its own
middleware ID (Claude Code sends no `X-Request-ID`) — the Anthropic
`request-id` lives only in the response headers, which the router does not
record. So the join is inferred (`MatchGwRequests`, run after both `reqlog`
and `ingest`): a transcript request with the **exact usage tuple** (input,
output, cache_creation, cache_read) within ±30 minutes, closest timestamp
wins, each transcript row claimed once. Model equality is deliberately not
required: the wire carries the alias the client sent
(`claude-sonnet-4-5`), the transcript the canonical id from the response
(`claude-sonnet-4-5-20250929`).

Validated on first live pull: 86/102 usage-bearing rows matched, 85/86
matched pairs agree on model (the one disagreement is the alias case), max
timestamp skew 84s. Unmatched rows are a feature, not noise — they are the
traffic transcripts cannot see: error responses (401/404/429), other
machines' sessions, and Claude Code's internal utility calls (title
generation and similar), which hit the API but write no usage rows to any
transcript.

## The wire-level cache doctor

`tokenator cache --wire` diffs consecutive prefix hash chains within each
(session, model) stream and names the exact segment where the cached prefix
diverged: segment 0 = tools, 1 = system, k≥2 = message k−2 (assumes tools
and system present — true for every Claude Code request). Event causes
follow the offline doctor's precedence (compaction → TTL → the divergence
class) and share its cost gates, so `cache` and `cache --wire` report
comparable numbers. `--session <prefix>` prints the per-transition
timeline.

Two things the first live day taught us that inference could not:

- **The chain is noisier than the cache.** Claude Code moves its
  `cache_control` breakpoint to the newest message every request, so the
  previous request's final message always re-serializes → the chain
  "diverges" at exactly PrevSegs−1 while cache reads stay fully warm
  (observed shortfall: 1–2 tokens). Classified as `marker_rotation`, and
  99% of all transitions are this. The same churn hits the tools segment:
  `tools_changed` with a fully-warm read is placement noise, not a tool
  change.
- **Every material miss was TTL.** All costed events in the first day
  (1.4M re-processed tokens) sat behind 1h+ idle gaps — including two that
  also showed tools-segment divergence, where blaming the tool change
  would have suggested a fixable cost that isn't. Wire truth confirms
  M3's inference: idle-gap expiry dominates, genuine history edits are
  rare.

A router-side refinement would sharpen this further: stripping
`cache_control` keys before hashing in `anthropicPrefixChain` would make
chain divergence mean content divergence, eliminating the marker_rotation
class entirely.
