// Package reqlog pulls request rows from the LLM router's reqlog Postgres
// (the router_requests table) into gw_request — the wire-level view of every
// request that crossed the router, including the Anthropic passthrough that
// Claude Code reaches via ANTHROPIC_BASE_URL.
//
// The pull is incremental by origin row id (BIGSERIAL, append-only): the
// cursor is MAX(pg_id) already stored for the source, so re-runs only fetch
// new rows and re-ingests are no-ops via the dedupe key. After each pull the
// store's MatchGwRequests pairs anthropic-class rows (and session-bearing
// chat-class rows) with transcript request rows by usage tuple + time
// proximity, within the row's own harness session when the router logged
// one (router_requests.session_id, llm-router-go f00737c+); the pass is
// incremental over what changed since the last one. Older routers have no such column; Fetch detects that once and
// reads an empty id instead, so tokenator keeps working against an
// un-upgraded origin.
//
// Facts this ingester relies on (llm-router-go v0.6.1, internal/router/reqlog):
//   - one row per request, id BIGSERIAL, ts TIMESTAMPTZ at request start;
//   - usage columns are nullable — NULL means the response reported none
//     (rejected requests, upstream errors), 0 is a real reported zero;
//   - prefix_hash_chain is comma-joined cumulative sha256 prefixes (16 hex
//     chars each) over tools → system → each message, anthropic class only;
//   - request_id is the router's own X-Request-ID, NOT the Anthropic
//     request id — Claude Code sends none, so it cannot join transcripts.
package reqlog

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/erewhon/tokenator/internal/store"
)

const SourceKind = "reqlog"

// fetchBatch bounds one SELECT so a first-ever pull of a large table streams
// in chunks instead of one giant result set.
const fetchBatch = 5000

// Row mirrors one router_requests row. Pointer fields are nullable columns.
type Row struct {
	ID              int64
	RequestID       string
	TS              time.Time
	Method          string
	Path            string
	Model           string
	BackendModel    string
	BackendURL      string
	ResolvedVia     string
	APIClass        string
	ViaToolProxy    bool
	Stream          bool
	Status          int64
	LatencyMS       int64
	PromptTokens    *int64
	CompletionToks  *int64
	CacheCreation   *int64
	CacheRead       *int64
	PrefixHashChain string
	Error           string
	SessionID       string // caller's harness session id; '' when absent
}

// Source yields reqlog rows after a cursor. Root identifies the origin for
// the source table and dedupe keys — it must be stable across runs and must
// not contain credentials.
type Source interface {
	Root() string
	Fetch(ctx context.Context, afterID int64, limit int) ([]Row, error)
	Close() error
}

type Stats struct {
	Fetched   int
	New       int
	Anthropic int
	Matched   int64 // newly matched this run (all sources)
	Unmatched int64 // usage-bearing anthropic rows still unmatched
}

func (st Stats) String() string {
	return fmt.Sprintf("rows=+%d/%d (anthropic +%d) matched=+%d unmatched=%d",
		st.New, st.Fetched, st.Anthropic, st.Matched, st.Unmatched)
}

type Ingester struct {
	Src    Source
	Regime string // billing regime recorded on the source row
}

func (in *Ingester) Run(ctx context.Context, st *store.Store) (Stats, error) {
	var stats Stats
	regime := in.Regime
	if regime == "" {
		regime = "unknown"
	}
	sourceID, err := st.UpsertSource(SourceKind, in.Src.Root(), regime)
	if err != nil {
		return stats, err
	}
	cursor, err := st.MaxGwPGID(sourceID)
	if err != nil {
		return stats, err
	}
	for {
		rows, err := in.Src.Fetch(ctx, cursor, fetchBatch)
		if err != nil {
			return stats, err
		}
		if len(rows) == 0 {
			break
		}
		err = st.WithTx(func(tx *store.Tx) error {
			for _, r := range rows {
				inserted, err := tx.InsertGwRequest(toGwRequest(sourceID, in.Src.Root(), r))
				if err != nil {
					return err
				}
				if inserted {
					stats.New++
					if r.APIClass == "anthropic" {
						stats.Anthropic++
					}
				}
			}
			return nil
		})
		if err != nil {
			return stats, err
		}
		stats.Fetched += len(rows)
		cursor = rows[len(rows)-1].ID
		if len(rows) < fetchBatch {
			break
		}
	}
	stats.Matched, stats.Unmatched, err = st.MatchGwRequests()
	return stats, err
}

func toGwRequest(sourceID int64, root string, r Row) store.GwRequest {
	chainLen := int64(0)
	if r.PrefixHashChain != "" {
		chainLen = 1
		for _, c := range r.PrefixHashChain {
			if c == ',' {
				chainLen++
			}
		}
	}
	return store.GwRequest{
		SourceID:            sourceID,
		PGID:                r.ID,
		RouterReqID:         r.RequestID,
		TS:                  r.TS.UTC().Format(time.RFC3339),
		Method:              r.Method,
		Path:                r.Path,
		Model:               r.Model,
		BackendModel:        r.BackendModel,
		BackendURL:          r.BackendURL,
		ResolvedVia:         r.ResolvedVia,
		APIClass:            r.APIClass,
		ViaToolProxy:        r.ViaToolProxy,
		Stream:              r.Stream,
		Status:              r.Status,
		LatencyMS:           r.LatencyMS,
		InputTokens:         r.PromptTokens,
		OutputTokens:        r.CompletionToks,
		CacheCreationTokens: r.CacheCreation,
		CacheReadTokens:     r.CacheRead,
		PrefixHashChain:     r.PrefixHashChain,
		ChainLen:            chainLen,
		Error:               r.Error,
		SessionID:           r.SessionID,
		DedupeKey:           fmt.Sprintf("%s#%d", root, r.ID),
	}
}

// PGSource fetches rows from the reqlog Postgres over database/sql with the
// pgx stdlib driver. The DSN must be URI form (postgres://user:pass@host/db)
// so credentials can be stripped from the stored source root.
type PGSource struct {
	db   *sql.DB
	root string
	// sessionCol is the SELECT expression for the session id: the column
	// when the origin has it, else a constant ''. Resolved on first Fetch.
	sessionCol string
}

func OpenPG(ctx context.Context, dsn string) (*PGSource, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("reqlog DSN must be URI form (postgres://user:pass@host:port/db)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("reqlog: ping %s: %w", u.Host, err)
	}
	return &PGSource{db: db, root: u.Host + u.Path}, nil
}

func (p *PGSource) Root() string { return p.root }
func (p *PGSource) Close() error { return p.db.Close() }

// resolveSessionCol checks once whether router_requests has session_id, so a
// pull against a router that predates it selects an empty id rather than
// failing.
func (p *PGSource) resolveSessionCol(ctx context.Context) error {
	if p.sessionCol != "" {
		return nil
	}
	var n int
	err := p.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'router_requests' AND column_name = 'session_id'
		  AND table_schema = ANY (current_schemas(false))`).Scan(&n)
	if err != nil {
		return fmt.Errorf("reqlog: probe session_id column: %w", err)
	}
	p.sessionCol = "''"
	if n > 0 {
		p.sessionCol = "COALESCE(session_id, '')"
	}
	return nil
}

func (p *PGSource) Fetch(ctx context.Context, afterID int64, limit int) ([]Row, error) {
	if err := p.resolveSessionCol(ctx); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT id, COALESCE(request_id, ''), ts, method, path, model,
			COALESCE(backend_model, ''), COALESCE(backend_url, ''),
			COALESCE(resolved_via, ''), COALESCE(api_class, ''),
			via_tool_proxy, stream, status, latency_ms,
			prompt_tokens, completion_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			COALESCE(prefix_hash_chain, ''), COALESCE(error, ''),
			`+p.sessionCol+`
		FROM router_requests
		WHERE id > $1
		ORDER BY id
		LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.RequestID, &r.TS, &r.Method, &r.Path, &r.Model,
			&r.BackendModel, &r.BackendURL, &r.ResolvedVia, &r.APIClass,
			&r.ViaToolProxy, &r.Stream, &r.Status, &r.LatencyMS,
			&r.PromptTokens, &r.CompletionToks,
			&r.CacheCreation, &r.CacheRead,
			&r.PrefixHashChain, &r.Error, &r.SessionID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
