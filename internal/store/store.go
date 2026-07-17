// Package store owns the tokenator SQLite database: schema, migrations, and
// the write/read paths shared by every ingester and report.
//
// Design constraints (see docs/phase1-profiler.md):
//   - request rows hold harness-reported usage and are the ground truth for
//     spend; anything block-level added later is an estimate layered on top.
//   - dedupe_key is globally unique across the whole database: the same
//     Anthropic message can appear in several transcript files (session
//     resume/fork copies history into new files), and it must be counted once.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schemaV1 = `
CREATE TABLE source (
	id             INTEGER PRIMARY KEY,
	kind           TEXT NOT NULL,             -- claude_code | opencode | otel | reqlog
	root           TEXT NOT NULL,             -- filesystem root or DSN
	billing_regime TEXT NOT NULL DEFAULT 'unknown', -- subscription | metered | local | unknown
	UNIQUE (kind, root)
);

CREATE TABLE session (
	id                 INTEGER PRIMARY KEY,
	source_id          INTEGER NOT NULL REFERENCES source(id),
	harness_session_id TEXT NOT NULL,
	slug               TEXT NOT NULL DEFAULT '',
	project            TEXT NOT NULL DEFAULT '',  -- display name (basename of cwd)
	cwd                TEXT NOT NULL DEFAULT '',
	title              TEXT NOT NULL DEFAULT '',
	agent              TEXT NOT NULL DEFAULT '',  -- subagent name for sidechain sessions
	started_at         TEXT NOT NULL DEFAULT '',  -- RFC3339; '' = unknown
	ended_at           TEXT NOT NULL DEFAULT '',
	parent_session_id  INTEGER REFERENCES session(id),
	UNIQUE (source_id, harness_session_id)
);

CREATE TABLE request (
	id                       INTEGER PRIMARY KEY,
	session_id               INTEGER NOT NULL REFERENCES session(id),
	ts                       TEXT NOT NULL,
	model                    TEXT NOT NULL DEFAULT '',
	provider                 TEXT NOT NULL DEFAULT '',
	input_tokens             INTEGER NOT NULL DEFAULT 0,
	output_tokens            INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens    INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens        INTEGER NOT NULL DEFAULT 0,
	cache_creation_5m_tokens INTEGER,                    -- TTL split when reported
	cache_creation_1h_tokens INTEGER,
	reasoning_tokens         INTEGER,                    -- OpenCode reports this
	cost_usd                 REAL,                       -- harness-reported cost, if any
	finish_reason            TEXT NOT NULL DEFAULT '',
	speed                    TEXT NOT NULL DEFAULT '',
	service_tier             TEXT NOT NULL DEFAULT '',
	dedupe_key               TEXT NOT NULL UNIQUE,
	harness_msg_id           TEXT NOT NULL DEFAULT '',
	harness_request_id       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_request_session ON request(session_id);
CREATE INDEX idx_request_ts ON request(ts);

CREATE TABLE compaction (
	id         INTEGER PRIMARY KEY,
	session_id INTEGER NOT NULL REFERENCES session(id),
	ts         TEXT NOT NULL,
	cause      TEXT NOT NULL DEFAULT '',  -- auto | manual | microcompact
	pre_tokens INTEGER,
	UNIQUE (session_id, ts)
);

CREATE TABLE ingest_file (
	id        INTEGER PRIMARY KEY,
	source_id INTEGER NOT NULL REFERENCES source(id),
	path      TEXT NOT NULL,
	size      INTEGER NOT NULL,
	mtime_ns  INTEGER NOT NULL,
	UNIQUE (source_id, path)
);
`

// schemaV2 adds block-level attribution: one row per context item (text,
// thinking, tool_use, tool_result, ...) with an origin classification.
// byte_len/est_tokens are ESTIMATES (bytes/4); request rows remain the
// ground truth for metered usage. Content itself is never stored — only
// sizes and a truncated content hash for repeat detection.
const schemaV2 = `
CREATE TABLE block (
	id           INTEGER PRIMARY KEY,
	session_id   INTEGER NOT NULL REFERENCES session(id),
	ts           TEXT NOT NULL DEFAULT '',
	kind         TEXT NOT NULL,             -- user_text|meta_text|assistant_text|thinking|tool_use|tool_result|image
	tool         TEXT NOT NULL DEFAULT '',  -- tool name for tool_use/tool_result
	mcp_server   TEXT NOT NULL DEFAULT '',  -- parsed from mcp__<server>__<tool>
	file_path    TEXT NOT NULL DEFAULT '',  -- for file-oriented tools (Read/Edit/Write/...)
	tool_use_id  TEXT NOT NULL DEFAULT '',
	request_key  TEXT NOT NULL DEFAULT '',  -- dedupe_key of the producing request, when known
	byte_len     INTEGER NOT NULL DEFAULT 0,
	est_tokens   INTEGER NOT NULL DEFAULT 0,
	content_hash TEXT NOT NULL DEFAULT '',
	is_error     INTEGER NOT NULL DEFAULT 0,
	dedupe_key   TEXT NOT NULL UNIQUE
);
CREATE INDEX idx_block_session ON block(session_id);
CREATE INDEX idx_block_kind ON block(kind);
CREATE INDEX idx_block_tool ON block(tool) WHERE tool != '';
CREATE INDEX idx_block_file ON block(file_path) WHERE file_path != '';
`

// schemaV3 adds OTel capture: datapoints from Claude Code's OTLP metrics
// export and events from its OTLP logs export (live-only — no backfill).
// Rows carry the harness session uuid as TEXT; session rows remain owned by
// the transcript ingesters, so joins go through session.harness_session_id.
//
// otel_datapoint dedupe: key = hash(metric, start_ts, all attributes). With
// cumulative temporality a series re-exports under the same key with a
// growing value (upsert keeps the latest cumulative total); with delta
// temporality start_ts advances every export, so each point is its own row.
// Either way SUM(value) over rows is the true total.
const schemaV3 = `
CREATE TABLE otel_datapoint (
	id                 INTEGER PRIMARY KEY,
	source_id          INTEGER NOT NULL REFERENCES source(id),
	harness_session_id TEXT NOT NULL DEFAULT '',
	metric             TEXT NOT NULL,             -- claude_code.token.usage, ...
	model              TEXT NOT NULL DEFAULT '',
	type               TEXT NOT NULL DEFAULT '',  -- token type: input|output|cacheRead|cacheCreation
	query_source       TEXT NOT NULL DEFAULT '',  -- main | subagent | auxiliary | sdk
	agent_name         TEXT NOT NULL DEFAULT '',  -- attribution attrs, present when
	skill_name         TEXT NOT NULL DEFAULT '',  --   the corresponding context is
	plugin_name        TEXT NOT NULL DEFAULT '',  --   active for the tokens counted
	mcp_server         TEXT NOT NULL DEFAULT '',
	mcp_tool           TEXT NOT NULL DEFAULT '',
	start_ts           TEXT NOT NULL DEFAULT '',
	ts                 TEXT NOT NULL DEFAULT '',
	value              REAL NOT NULL DEFAULT 0,
	attrs              TEXT NOT NULL DEFAULT '{}', -- residual attributes (JSON)
	dedupe_key         TEXT NOT NULL UNIQUE
);
CREATE INDEX idx_otel_dp_metric ON otel_datapoint(metric);
CREATE INDEX idx_otel_dp_session ON otel_datapoint(harness_session_id);

CREATE TABLE otel_event (
	id                    INTEGER PRIMARY KEY,
	source_id             INTEGER NOT NULL REFERENCES source(id),
	harness_session_id    TEXT NOT NULL DEFAULT '',
	event                 TEXT NOT NULL,            -- api_request, tool_result, ...
	ts                    TEXT NOT NULL DEFAULT '',
	seq                   INTEGER NOT NULL DEFAULT 0, -- event.sequence (per session)
	prompt_id             TEXT NOT NULL DEFAULT '',
	request_id            TEXT NOT NULL DEFAULT '',   -- joins request.harness_request_id
	model                 TEXT NOT NULL DEFAULT '',
	tool_name             TEXT NOT NULL DEFAULT '',
	tool_use_id           TEXT NOT NULL DEFAULT '',   -- joins block.tool_use_id
	query_source          TEXT NOT NULL DEFAULT '',
	agent_name            TEXT NOT NULL DEFAULT '',
	skill_name            TEXT NOT NULL DEFAULT '',
	mcp_server            TEXT NOT NULL DEFAULT '',
	mcp_tool              TEXT NOT NULL DEFAULT '',
	duration_ms           INTEGER,
	input_tokens          INTEGER,
	output_tokens         INTEGER,
	cache_read_tokens     INTEGER,
	cache_creation_tokens INTEGER,
	cost_usd              REAL,
	attrs                 TEXT NOT NULL DEFAULT '{}', -- residual attributes (JSON)
	dedupe_key            TEXT NOT NULL UNIQUE
);
CREATE INDEX idx_otel_event_session ON otel_event(harness_session_id);
CREATE INDEX idx_otel_event_event ON otel_event(event);
CREATE INDEX idx_otel_event_request ON otel_event(request_id) WHERE request_id != '';
`

var migrations = []string{schemaV1, schemaV2, schemaV3}

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite serializes writers anyway; one connection avoids
	// SQLITE_BUSY between an ingest transaction and report queries.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for ; v < len(migrations); v++ {
		if _, err := s.db.Exec(migrations[v]); err != nil {
			return fmt.Errorf("apply migration %d: %w", v+1, err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

// --- sources ---

func (s *Store) UpsertSource(kind, root, billingRegime string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`
		INSERT INTO source (kind, root, billing_regime) VALUES (?, ?, ?)
		ON CONFLICT (kind, root) DO UPDATE SET billing_regime = excluded.billing_regime
		RETURNING id`, kind, root, billingRegime).Scan(&id)
	return id, err
}

// --- ingest file bookkeeping ---

// FileUnchanged reports whether path was already ingested at exactly this
// size and mtime. Any change triggers a full re-parse; global dedupe keys
// make re-parsing idempotent.
func (s *Store) FileUnchanged(sourceID int64, path string, size, mtimeNS int64) (bool, error) {
	var gotSize, gotMtime int64
	err := s.db.QueryRow(
		`SELECT size, mtime_ns FROM ingest_file WHERE source_id = ? AND path = ?`,
		sourceID, path).Scan(&gotSize, &gotMtime)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return gotSize == size && gotMtime == mtimeNS, nil
}

func (s *Store) RecordFile(sourceID int64, path string, size, mtimeNS int64) error {
	_, err := s.db.Exec(`
		INSERT INTO ingest_file (source_id, path, size, mtime_ns) VALUES (?, ?, ?, ?)
		ON CONFLICT (source_id, path) DO UPDATE SET size = excluded.size, mtime_ns = excluded.mtime_ns`,
		sourceID, path, size, mtimeNS)
	return err
}

type FileState struct {
	Size    int64
	MtimeNS int64
}

// FileStates returns the recorded state of every ingested file for a source
// in one query — for ingesters that track many small files (OpenCode stores
// one JSON file per message), where a per-file query would dominate runtime.
func (s *Store) FileStates(sourceID int64) (map[string]FileState, error) {
	rows, err := s.db.Query(`SELECT path, size, mtime_ns FROM ingest_file WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileState{}
	for rows.Next() {
		var path string
		var fs FileState
		if err := rows.Scan(&path, &fs.Size, &fs.MtimeNS); err != nil {
			return nil, err
		}
		out[path] = fs
	}
	return out, rows.Err()
}

// --- transactional writes ---

type Tx struct{ tx *sql.Tx }

func (s *Store) WithTx(fn func(*Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(&Tx{tx: tx}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type Session struct {
	SourceID  int64
	HarnessID string
	Slug      string
	Project   string
	CWD       string
	Title     string
	Agent     string
	StartedAt string // RFC3339 or ''
	EndedAt   string
}

// UpsertSession merges metadata into an existing session row: non-empty new
// values win for text fields (titles can change), timestamps widen to the
// min/max observed.
func (t *Tx) UpsertSession(sess Session) (int64, error) {
	var id int64
	err := t.tx.QueryRow(`
		INSERT INTO session (source_id, harness_session_id, slug, project, cwd, title, agent, started_at, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_id, harness_session_id) DO UPDATE SET
			slug    = CASE WHEN excluded.slug    != '' THEN excluded.slug    ELSE session.slug    END,
			project = CASE WHEN excluded.project != '' THEN excluded.project ELSE session.project END,
			cwd     = CASE WHEN excluded.cwd     != '' THEN excluded.cwd     ELSE session.cwd     END,
			title   = CASE WHEN excluded.title   != '' THEN excluded.title   ELSE session.title   END,
			agent   = CASE WHEN excluded.agent   != '' THEN excluded.agent   ELSE session.agent   END,
			started_at = CASE
				WHEN session.started_at = '' THEN excluded.started_at
				WHEN excluded.started_at = '' THEN session.started_at
				WHEN excluded.started_at < session.started_at THEN excluded.started_at
				ELSE session.started_at END,
			ended_at = CASE
				WHEN session.ended_at = '' THEN excluded.ended_at
				WHEN excluded.ended_at = '' THEN session.ended_at
				WHEN excluded.ended_at > session.ended_at THEN excluded.ended_at
				ELSE session.ended_at END
		RETURNING id`,
		sess.SourceID, sess.HarnessID, sess.Slug, sess.Project, sess.CWD,
		sess.Title, sess.Agent, sess.StartedAt, sess.EndedAt).Scan(&id)
	return id, err
}

type Request struct {
	SessionID           int64
	TS                  string
	Model               string
	Provider            string
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	CacheCreation5m     *int64
	CacheCreation1h     *int64
	ReasoningTokens     *int64
	CostUSD             *float64
	FinishReason        string
	Speed               string
	ServiceTier         string
	DedupeKey           string
	HarnessMsgID        string
	HarnessRequestID    string
}

// InsertRequest returns true when the row was inserted, false when the
// dedupe key already existed (duplicate transcript line or re-ingest).
func (t *Tx) InsertRequest(r Request) (bool, error) {
	res, err := t.tx.Exec(`
		INSERT INTO request (
			session_id, ts, model, provider,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			cache_creation_5m_tokens, cache_creation_1h_tokens, reasoning_tokens,
			cost_usd, finish_reason, speed, service_tier,
			dedupe_key, harness_msg_id, harness_request_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		r.SessionID, r.TS, r.Model, r.Provider,
		r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens,
		r.CacheCreation5m, r.CacheCreation1h, r.ReasoningTokens,
		r.CostUSD, r.FinishReason, r.Speed, r.ServiceTier,
		r.DedupeKey, r.HarnessMsgID, r.HarnessRequestID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UpsertRequest inserts or refreshes a request row by dedupe key, returning
// true when the row is new. Claude Code transcript lines are immutable, so
// its ingester uses InsertRequest (first write wins); OpenCode message files
// mutate in place while a response streams, so usage for an already-seen
// message can legitimately grow — last write wins here.
func (t *Tx) UpsertRequest(r Request) (bool, error) {
	var exists bool
	if err := t.tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM request WHERE dedupe_key = ?)`, r.DedupeKey).Scan(&exists); err != nil {
		return false, err
	}
	_, err := t.tx.Exec(`
		INSERT INTO request (
			session_id, ts, model, provider,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			cache_creation_5m_tokens, cache_creation_1h_tokens, reasoning_tokens,
			cost_usd, finish_reason, speed, service_tier,
			dedupe_key, harness_msg_id, harness_request_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (dedupe_key) DO UPDATE SET
			ts = excluded.ts,
			model = excluded.model,
			provider = excluded.provider,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cache_creation_tokens = excluded.cache_creation_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			cache_creation_5m_tokens = excluded.cache_creation_5m_tokens,
			cache_creation_1h_tokens = excluded.cache_creation_1h_tokens,
			reasoning_tokens = excluded.reasoning_tokens,
			cost_usd = excluded.cost_usd,
			finish_reason = excluded.finish_reason`,
		r.SessionID, r.TS, r.Model, r.Provider,
		r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens,
		r.CacheCreation5m, r.CacheCreation1h, r.ReasoningTokens,
		r.CostUSD, r.FinishReason, r.Speed, r.ServiceTier,
		r.DedupeKey, r.HarnessMsgID, r.HarnessRequestID)
	return !exists, err
}

type Block struct {
	SessionID   int64
	TS          string
	Kind        string
	Tool        string
	MCPServer   string
	FilePath    string
	ToolUseID   string
	RequestKey  string
	ByteLen     int64
	EstTokens   int64
	ContentHash string
	IsError     bool
	DedupeKey   string
}

// UpsertBlock inserts or refreshes a block row by dedupe key, returning true
// when the row is new. OpenCode part files mutate while streaming (text
// grows), so sizes are refreshed on re-parse; Claude Code re-ingests are
// no-op updates with identical values.
func (t *Tx) UpsertBlock(b Block) (bool, error) {
	var exists bool
	if err := t.tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM block WHERE dedupe_key = ?)`, b.DedupeKey).Scan(&exists); err != nil {
		return false, err
	}
	_, err := t.tx.Exec(`
		INSERT INTO block (
			session_id, ts, kind, tool, mcp_server, file_path, tool_use_id,
			request_key, byte_len, est_tokens, content_hash, is_error, dedupe_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (dedupe_key) DO UPDATE SET
			ts = excluded.ts,
			kind = excluded.kind,
			tool = excluded.tool,
			mcp_server = excluded.mcp_server,
			file_path = excluded.file_path,
			byte_len = excluded.byte_len,
			est_tokens = excluded.est_tokens,
			content_hash = excluded.content_hash,
			is_error = excluded.is_error`,
		b.SessionID, b.TS, b.Kind, b.Tool, b.MCPServer, b.FilePath, b.ToolUseID,
		b.RequestKey, b.ByteLen, b.EstTokens, b.ContentHash, b.IsError, b.DedupeKey)
	return !exists, err
}

type Compaction struct {
	SessionID int64
	TS        string
	Cause     string
	PreTokens *int64
}

func (t *Tx) InsertCompaction(c Compaction) error {
	_, err := t.tx.Exec(`
		INSERT INTO compaction (session_id, ts, cause, pre_tokens) VALUES (?, ?, ?, ?)
		ON CONFLICT (session_id, ts) DO NOTHING`,
		c.SessionID, c.TS, c.Cause, c.PreTokens)
	return err
}

// --- otel writes ---

type OTelDatapoint struct {
	SourceID    int64
	SessionKey  string // harness session uuid from the session.id attribute
	Metric      string
	Model       string
	Type        string
	QuerySource string
	AgentName   string
	SkillName   string
	PluginName  string
	MCPServer   string
	MCPTool     string
	StartTS     string
	TS          string
	Value       float64
	Attrs       string // residual attributes as JSON
	DedupeKey   string
}

// UpsertOTelDatapoint inserts or refreshes a datapoint, returning true when
// the row is new. A cumulative series re-exports under the same dedupe key
// with a growing value; last write wins, guarded against out-of-order
// delivery by the ts comparison.
func (t *Tx) UpsertOTelDatapoint(d OTelDatapoint) (bool, error) {
	var exists bool
	if err := t.tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM otel_datapoint WHERE dedupe_key = ?)`, d.DedupeKey).Scan(&exists); err != nil {
		return false, err
	}
	_, err := t.tx.Exec(`
		INSERT INTO otel_datapoint (
			source_id, harness_session_id, metric, model, type, query_source,
			agent_name, skill_name, plugin_name, mcp_server, mcp_tool,
			start_ts, ts, value, attrs, dedupe_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (dedupe_key) DO UPDATE SET
			ts = excluded.ts,
			value = excluded.value
		WHERE excluded.ts >= otel_datapoint.ts`,
		d.SourceID, d.SessionKey, d.Metric, d.Model, d.Type, d.QuerySource,
		d.AgentName, d.SkillName, d.PluginName, d.MCPServer, d.MCPTool,
		d.StartTS, d.TS, d.Value, d.Attrs, d.DedupeKey)
	return !exists, err
}

type OTelEvent struct {
	SourceID            int64
	SessionKey          string
	Event               string // event name without the claude_code. prefix
	TS                  string
	Seq                 int64
	PromptID            string
	RequestID           string
	Model               string
	ToolName            string
	ToolUseID           string
	QuerySource         string
	AgentName           string
	SkillName           string
	MCPServer           string
	MCPTool             string
	DurationMS          *int64
	InputTokens         *int64
	OutputTokens        *int64
	CacheReadTokens     *int64
	CacheCreationTokens *int64
	CostUSD             *float64
	Attrs               string
	DedupeKey           string
}

// InsertOTelEvent inserts an event, returning true when the row is new.
// Events are immutable; a duplicate dedupe key (re-delivered export batch)
// is a no-op.
func (t *Tx) InsertOTelEvent(e OTelEvent) (bool, error) {
	res, err := t.tx.Exec(`
		INSERT INTO otel_event (
			source_id, harness_session_id, event, ts, seq, prompt_id, request_id,
			model, tool_name, tool_use_id, query_source, agent_name, skill_name,
			mcp_server, mcp_tool, duration_ms, input_tokens, output_tokens,
			cache_read_tokens, cache_creation_tokens, cost_usd, attrs, dedupe_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		e.SourceID, e.SessionKey, e.Event, e.TS, e.Seq, e.PromptID, e.RequestID,
		e.Model, e.ToolName, e.ToolUseID, e.QuerySource, e.AgentName, e.SkillName,
		e.MCPServer, e.MCPTool, e.DurationMS, e.InputTokens, e.OutputTokens,
		e.CacheReadTokens, e.CacheCreationTokens, e.CostUSD, e.Attrs, e.DedupeKey)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- reads ---

type RollupRow struct {
	Group       string
	Requests    int64
	Input       int64
	Output      int64
	CacheRead   int64
	CacheCreate int64
}

var rollupGroups = map[string]string{
	"project": `COALESCE(NULLIF(s.project, ''), '(unknown)')`,
	"model":   `COALESCE(NULLIF(r.model, ''), '(unknown)')`,
	"session": `s.harness_session_id || CASE WHEN s.title != '' THEN ' — ' || s.title ELSE '' END`,
}

// Rollup aggregates request usage grouped by project, model, or session.
// sinceTS is an RFC3339 lower bound; empty means all time.
func (s *Store) Rollup(by, sinceTS string) ([]RollupRow, error) {
	groupExpr, ok := rollupGroups[by]
	if !ok {
		return nil, fmt.Errorf("unknown rollup group %q (want project, model, or session)", by)
	}
	rows, err := s.db.Query(`
		SELECT `+groupExpr+` AS grp,
			COUNT(*),
			SUM(r.input_tokens), SUM(r.output_tokens),
			SUM(r.cache_read_tokens), SUM(r.cache_creation_tokens)
		FROM request r
		JOIN session s ON s.id = r.session_id
		WHERE (? = '' OR r.ts >= ?)
		GROUP BY grp
		ORDER BY SUM(r.input_tokens + r.output_tokens + r.cache_read_tokens + r.cache_creation_tokens) DESC`,
		sinceTS, sinceTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RollupRow
	for rows.Next() {
		var r RollupRow
		if err := rows.Scan(&r.Group, &r.Requests, &r.Input, &r.Output, &r.CacheRead, &r.CacheCreate); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type TableCounts struct {
	Sources, Sessions, Requests, Blocks, Compactions, Files int64
	OTelDatapoints, OTelEvents                              int64
}

func (s *Store) Counts() (TableCounts, error) {
	var c TableCounts
	for _, q := range []struct {
		table string
		dst   *int64
	}{
		{"source", &c.Sources},
		{"session", &c.Sessions},
		{"request", &c.Requests},
		{"block", &c.Blocks},
		{"compaction", &c.Compactions},
		{"ingest_file", &c.Files},
		{"otel_datapoint", &c.OTelDatapoints},
		{"otel_event", &c.OTelEvents},
	} {
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + q.table).Scan(q.dst); err != nil {
			return c, err
		}
	}
	return c, nil
}

// --- attribution ---

type AttrRow struct {
	Group     string
	Blocks    int64
	Errors    int64
	Bytes     int64
	EstTokens int64
}

// AttrRollup aggregates extracted blocks. Flow attribution: each block is
// counted once at its estimated size (tokens that ENTERED context), not
// multiplied by residency. sessionID narrows to one session; 0 means all.
//   - tool: tool_result payloads grouped by tool name
//   - mcp:  tool_use + tool_result grouped by MCP server
//   - file: tool_result payloads grouped by file path
//   - kind: everything grouped by block kind
func (s *Store) AttrRollup(by, sinceTS string, sessionID int64) ([]AttrRow, error) {
	var groupExpr, where string
	switch by {
	case "tool":
		groupExpr, where = `COALESCE(NULLIF(b.tool,''),'(unresolved)')`, `b.kind = 'tool_result'`
	case "mcp":
		groupExpr, where = `b.mcp_server`, `b.mcp_server != ''`
	case "file":
		groupExpr, where = `b.file_path`, `b.kind = 'tool_result' AND b.file_path != ''`
	case "kind":
		groupExpr, where = `b.kind`, `1=1`
	default:
		return nil, fmt.Errorf("unknown attr group %q (want tool, mcp, file, or kind)", by)
	}
	rows, err := s.db.Query(`
		SELECT `+groupExpr+` AS grp, COUNT(*),
			SUM(b.is_error), SUM(b.byte_len), SUM(b.est_tokens)
		FROM block b
		WHERE `+where+` AND (? = '' OR b.ts >= ?) AND (? = 0 OR b.session_id = ?)
		GROUP BY grp
		ORDER BY SUM(b.est_tokens) DESC`,
		sinceTS, sinceTS, sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttrRow
	for rows.Next() {
		var r AttrRow
		if err := rows.Scan(&r.Group, &r.Blocks, &r.Errors, &r.Bytes, &r.EstTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Calibration compares block estimates against metered request usage over
// the same window, split by direction. Input-side blocks (user/meta text,
// tool results) correspond to NEW prompt tokens (input + cache_creation);
// output-side blocks (assistant text, thinking, tool_use) to output tokens.
// Coverage below 100% is expected — system prompts, tool schemas, and
// harness injections are metered but not extracted as blocks.
type Calibration struct {
	InBlockEst  int64 // est tokens of input-side blocks
	InMetered   int64 // SUM(input + cache_creation) over requests
	OutBlockEst int64 // est tokens of output-side blocks
	OutMetered  int64 // SUM(output) over requests
}

func (s *Store) Calibrate(sinceTS string) (Calibration, error) {
	var c Calibration
	err := s.db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN kind IN ('user_text','meta_text','tool_result','image') THEN est_tokens END), 0),
			COALESCE(SUM(CASE WHEN kind IN ('assistant_text','thinking','tool_use') THEN est_tokens END), 0)
		FROM block WHERE (? = '' OR ts >= ?)`, sinceTS, sinceTS).Scan(&c.InBlockEst, &c.OutBlockEst)
	if err != nil {
		return c, err
	}
	err = s.db.QueryRow(`
		SELECT COALESCE(SUM(input_tokens + cache_creation_tokens), 0), COALESCE(SUM(output_tokens), 0)
		FROM request WHERE (? = '' OR ts >= ?)`, sinceTS, sinceTS).Scan(&c.InMetered, &c.OutMetered)
	return c, err
}

// --- waste ---

type RepeatReadRow struct {
	Project   string
	Session   string
	FilePath  string
	Reads     int64
	Versions  int64 // distinct content hashes; 1 = identical bytes re-read
	EstTotal  int64
	EstWasted int64 // total minus the largest single read
}

// RepeatReads finds files whose contents entered the SAME session more than
// once via tool results. Versions==1 means byte-identical re-reads.
func (s *Store) RepeatReads(sinceTS string, limit int) ([]RepeatReadRow, error) {
	rows, err := s.db.Query(`
		SELECT s.project, s.harness_session_id, b.file_path, COUNT(*),
			COUNT(DISTINCT b.content_hash),
			SUM(b.est_tokens), SUM(b.est_tokens) - MAX(b.est_tokens)
		FROM block b JOIN session s ON s.id = b.session_id
		WHERE b.kind = 'tool_result' AND b.file_path != '' AND (? = '' OR b.ts >= ?)
		GROUP BY b.session_id, b.file_path
		HAVING COUNT(*) > 1
		ORDER BY SUM(b.est_tokens) - MAX(b.est_tokens) DESC
		LIMIT ?`, sinceTS, sinceTS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepeatReadRow
	for rows.Next() {
		var r RepeatReadRow
		if err := rows.Scan(&r.Project, &r.Session, &r.FilePath, &r.Reads,
			&r.Versions, &r.EstTotal, &r.EstWasted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionMeta identifies one session for the session view.
type SessionMeta struct {
	ID      int64
	Key     string
	Slug    string
	Project string
	Title   string
	Agent   string
}

// SessionByPrefix resolves a harness-session-id or slug prefix to one
// session; when several match, the most recently active wins.
func (s *Store) SessionByPrefix(prefix string) (SessionMeta, error) {
	var m SessionMeta
	err := s.db.QueryRow(`
		SELECT id, harness_session_id, slug, project, title, agent
		FROM session
		WHERE harness_session_id LIKE ? || '%' OR slug LIKE ? || '%'
		ORDER BY ended_at DESC LIMIT 1`, prefix, prefix).Scan(
		&m.ID, &m.Key, &m.Slug, &m.Project, &m.Title, &m.Agent)
	if err == sql.ErrNoRows {
		return m, fmt.Errorf("no session matches prefix %q", prefix)
	}
	return m, err
}

// SessionBlock is a block row for timeline bucketing.
type SessionBlock struct {
	TS        string
	Kind      string
	EstTokens int64
}

func (s *Store) BlocksForSession(sessionID int64) ([]SessionBlock, error) {
	rows, err := s.db.Query(
		`SELECT ts, kind, est_tokens FROM block WHERE session_id = ? ORDER BY ts`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionBlock
	for rows.Next() {
		var b SessionBlock
		if err := rows.Scan(&b.TS, &b.Kind, &b.EstTokens); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// --- cache analysis inputs ---

// CacheReq feeds the analyzer (internal/analyze). Rows are ordered by
// (session_id, ts) as the analyzer requires.
type CacheReq struct {
	SessionID  int64
	Project    string
	SessionKey string
	Title      string
	TS         string
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Write5m    *int64
	Write1h    *int64
}

// RequestsForCache returns analyzer input. The since filter is applied at
// SESSION granularity (a session is included whole when any of its requests
// falls in the window) — cutting a session mid-way would fabricate a cold
// start. sessionPrefix narrows to sessions whose harness id or slug starts
// with the prefix; empty matches all.
func (s *Store) RequestsForCache(sinceTS, sessionPrefix string) ([]CacheReq, error) {
	rows, err := s.db.Query(`
		SELECT r.session_id, s.project, s.harness_session_id, s.title, r.ts, r.model,
			r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_creation_tokens,
			r.cache_creation_5m_tokens, r.cache_creation_1h_tokens
		FROM request r
		JOIN session s ON s.id = r.session_id
		WHERE (? = '' OR r.session_id IN (SELECT DISTINCT session_id FROM request WHERE ts >= ?))
		  AND (? = '' OR s.harness_session_id LIKE ? || '%' OR s.slug LIKE ? || '%')
		ORDER BY r.session_id, r.ts`,
		sinceTS, sinceTS, sessionPrefix, sessionPrefix, sessionPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheReq
	for rows.Next() {
		var r CacheReq
		if err := rows.Scan(&r.SessionID, &r.Project, &r.SessionKey, &r.Title, &r.TS, &r.Model,
			&r.Input, &r.Output, &r.CacheRead, &r.CacheWrite, &r.Write5m, &r.Write1h); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CompactionTimes maps session id to its compact-boundary timestamps.
func (s *Store) CompactionTimes() (map[int64][]string, error) {
	rows, err := s.db.Query(`SELECT session_id, ts FROM compaction ORDER BY session_id, ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var ts string
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		out[id] = append(out[id], ts)
	}
	return out, rows.Err()
}

type BigBlockRow struct {
	Project   string
	Tool      string
	FilePath  string
	EstTokens int64
	TS        string
}

// --- otel reads ---

// NameValue is a generic (label, numeric total) pair for OTel rollups.
type NameValue struct {
	Name  string
	Value float64
}

// OTelStatus summarizes captured OTel data for `tokenator otel --status`.
type OTelStatus struct {
	Sessions     int64       // distinct harness sessions seen
	Datapoints   int64       // otel_datapoint rows
	Events       int64       // otel_event rows
	TokensByType []NameValue // token.usage totals: input, output, cacheRead, cacheCreation
	CostUSD      float64     // cost.usage total
	EventCounts  []NameValue // events by name

	// Cross-check of api_request events against transcript-ingested request
	// rows, joined on request_id. Sums cover MATCHED pairs only, so the two
	// sides are directly comparable.
	APIReqEvents  int64
	APIReqMatched int64
	OTelInput     int64
	OTelOutput    int64
	JSONLInput    int64
	JSONLOutput   int64
}

func (s *Store) OTelStatus() (OTelStatus, error) {
	var st OTelStatus
	err := s.db.QueryRow(`
		SELECT
			(SELECT COUNT(DISTINCT harness_session_id) FROM otel_datapoint) ,
			(SELECT COUNT(*) FROM otel_datapoint),
			(SELECT COUNT(*) FROM otel_event),
			(SELECT COALESCE(SUM(value), 0) FROM otel_datapoint WHERE metric = 'claude_code.cost.usage')`).
		Scan(&st.Sessions, &st.Datapoints, &st.Events, &st.CostUSD)
	if err != nil {
		return st, err
	}
	rows, err := s.db.Query(`
		SELECT type, SUM(value) FROM otel_datapoint
		WHERE metric = 'claude_code.token.usage' GROUP BY type ORDER BY SUM(value) DESC`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var nv NameValue
		if err := rows.Scan(&nv.Name, &nv.Value); err != nil {
			return st, err
		}
		st.TokensByType = append(st.TokensByType, nv)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	erows, err := s.db.Query(`
		SELECT event, COUNT(*) FROM otel_event GROUP BY event ORDER BY COUNT(*) DESC`)
	if err != nil {
		return st, err
	}
	defer erows.Close()
	for erows.Next() {
		var nv NameValue
		if err := erows.Scan(&nv.Name, &nv.Value); err != nil {
			return st, err
		}
		st.EventCounts = append(st.EventCounts, nv)
	}
	if err := erows.Err(); err != nil {
		return st, err
	}
	err = s.db.QueryRow(`
		SELECT COUNT(*),
			COUNT(r.id),
			COALESCE(SUM(CASE WHEN r.id IS NOT NULL THEN e.input_tokens END), 0),
			COALESCE(SUM(CASE WHEN r.id IS NOT NULL THEN e.output_tokens END), 0),
			COALESCE(SUM(r.input_tokens), 0),
			COALESCE(SUM(r.output_tokens), 0)
		FROM otel_event e
		LEFT JOIN request r ON e.request_id != '' AND r.harness_request_id = e.request_id
		WHERE e.event = 'api_request'`).
		Scan(&st.APIReqEvents, &st.APIReqMatched,
			&st.OTelInput, &st.OTelOutput, &st.JSONLInput, &st.JSONLOutput)
	return st, err
}

// BiggestResults lists the largest single tool results — candidates for
// output compression (rtk-style) or narrower reads.
func (s *Store) BiggestResults(sinceTS string, limit int) ([]BigBlockRow, error) {
	rows, err := s.db.Query(`
		SELECT s.project, COALESCE(NULLIF(b.tool,''),'(unresolved)'), b.file_path, b.est_tokens, b.ts
		FROM block b JOIN session s ON s.id = b.session_id
		WHERE b.kind = 'tool_result' AND (? = '' OR b.ts >= ?)
		ORDER BY b.est_tokens DESC
		LIMIT ?`, sinceTS, sinceTS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BigBlockRow
	for rows.Next() {
		var r BigBlockRow
		if err := rows.Scan(&r.Project, &r.Tool, &r.FilePath, &r.EstTokens, &r.TS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
