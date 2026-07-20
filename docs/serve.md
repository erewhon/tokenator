# tokenator serve — session browser, content search, transcripts

`tokenator serve` runs a localhost web UI over the tokenator database:

```
tokenator serve                      # http://127.0.0.1:8990
tokenator serve --listen 127.0.0.1:9000 --scan-limit 200
```

Three views:

- **`/` browser** — sessions newest-first with project/since filters and a
  free-text content search box. Rows link to the transcript and the existing
  session profile page.
- **`/session/<key>`** — the M4 profile page (timeline, composition, tables),
  same renderer as `tokenator session --html`, plus navigation.
- **`/session/<key>/transcript`** — the session's actual content: every
  entry (user/assistant text, thinking, tool calls and results, compaction
  boundaries) with kind chips in the standard palette slots, long bodies
  folded behind `<details>`, and `?q=` term highlighting with `#e<idx>`
  anchors.

## Two-stage search

The database deliberately stores **no block content** (sizes + hashes only),
so search cannot be a SQL query. Instead:

1. **Narrow** — `store.SessionList` picks candidate sessions by project,
   time window, and recency (newest `--scan-limit`, default 80).
2. **Scan** — `internal/transcript` searches the harness's own files:
   Claude Code `<root>/<slug>/<session>.jsonl`, OpenCode
   `storage/message/<ses>/ + storage/part/<msg>/`. A raw-byte
   case-insensitive pass skips non-matching files without JSON parsing
   (~1 GB/s); only files that might match are parsed, so snippets and
   anchors line up with the transcript view. Queries containing
   JSON-escaped characters (`" \ < > &`, non-ASCII, control chars) skip the
   fast path to avoid false negatives.

The result header always reports how many sessions were scanned and whether
the candidate list hit the cap — truncation is never silent. In practice a
query over 80 real sessions (~1 GB of transcripts, mostly skipped by the
byte pass) returns in ~150 ms.

Sessions whose transcript files are gone (deleted, other machine) stay
listed and profiled — only content search/reading degrades, with the error
shown inline.

## Notes

- The server binds loopback by default and has no auth — keep it that way,
  or put it behind the usual homelab front door if it ever needs to leave
  the machine.
- Transcript reading is on-demand and read-only; nothing is written back to
  harness storage, and no content is copied into the database.
