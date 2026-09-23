package reqlog

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/erewhon/tokenator/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeSource serves canned rows and records the cursors it was asked for.
type fakeSource struct {
	rows    []Row
	cursors []int64
}

func (f *fakeSource) Root() string { return "pg.test:5433/router" }
func (f *fakeSource) Close() error { return nil }

func (f *fakeSource) Fetch(_ context.Context, afterID int64, limit int) ([]Row, error) {
	f.cursors = append(f.cursors, afterID)
	var out []Row
	for _, r := range f.rows {
		if r.ID > afterID {
			out = append(out, r)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func iptr(n int64) *int64 { return &n }

var t0 = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

func anthRow(id int64, ts time.Time, in, out, ccre, cred int64) Row {
	return Row{
		ID: id, RequestID: "r16chars", TS: ts, Method: "POST", Path: "/v1/messages",
		Model: "claude-fable-5", BackendURL: "https://api.anthropic.com",
		ResolvedVia: "anthropic-gateway", APIClass: "anthropic", Stream: true,
		Status: 200, LatencyMS: 1500,
		PromptTokens: iptr(in), CompletionToks: iptr(out),
		CacheCreation: iptr(ccre), CacheRead: iptr(cred),
		PrefixHashChain: "aaaaaaaaaaaaaaaa,bbbbbbbbbbbbbbbb,cccccccccccccccc",
	}
}

// seedRequest writes a session + transcript request row with the given usage
// and returns the request's dedupe key.
func seedRequest(t *testing.T, st *store.Store, key string, ts time.Time, in, out, ccre, cred int64) string {
	t.Helper()
	// Resolve the source outside the tx: the store holds a single SQLite
	// connection, so a non-tx store call inside WithTx deadlocks.
	sourceID := mustSource(t, st)
	err := st.WithTx(func(tx *store.Tx) error {
		sid, err := tx.UpsertSession(store.Session{SourceID: sourceID, HarnessID: "sess-" + key})
		if err != nil {
			return err
		}
		_, err = tx.InsertRequest(store.Request{
			SessionID: sid, TS: ts.Format(time.RFC3339), Model: "claude-fable-5",
			InputTokens: in, OutputTokens: out,
			CacheCreationTokens: ccre, CacheReadTokens: cred,
			DedupeKey: key,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

var srcID int64

func mustSource(t *testing.T, st *store.Store) int64 {
	t.Helper()
	if srcID == 0 {
		id, err := st.UpsertSource("claude_code", "/tmp/cc", "subscription")
		if err != nil {
			t.Fatal(err)
		}
		srcID = id
	}
	return srcID
}

func TestIngestAndMatch(t *testing.T) {
	srcID = 0
	st := testStore(t)

	// Transcript request that should pair with gw row 3.
	seedRequest(t, st, "req-match", t0.Add(90*time.Second), 10, 73, 87517, 0)

	src := &fakeSource{rows: []Row{
		// chat-class row: ingested, never matched.
		{ID: 1, TS: t0, Method: "POST", Path: "/v1/chat/completions",
			Model: "qwen3", BackendModel: "qwen3-30b", APIClass: "chat", Status: 200,
			PromptTokens: iptr(10), CompletionToks: iptr(73)},
		// count_tokens probe: input only, excluded from matching.
		{ID: 2, TS: t0, Method: "POST", Path: "/v1/messages/count_tokens",
			Model: "claude-fable-5", APIClass: "anthropic", Status: 200,
			PromptTokens: iptr(4200)},
		anthRow(3, t0, 10, 73, 87517, 0),
		// Same tuple far outside the window: stays unmatched.
		anthRow(4, t0.Add(2*time.Hour), 10, 73, 87517, 0),
	}}

	ing := &Ingester{Src: src, Regime: "subscription"}
	stats, err := ing.Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.New != 4 || stats.Anthropic != 3 {
		t.Errorf("stats = %+v, want New=4 Anthropic=3", stats)
	}
	if stats.Matched != 1 || stats.Unmatched != 1 {
		t.Errorf("match stats = %+v, want Matched=1 Unmatched=1", stats)
	}
	if len(src.cursors) == 0 || src.cursors[0] != 0 {
		t.Errorf("first cursor = %v, want 0", src.cursors)
	}

	gs, err := st.GwStatus()
	if err != nil {
		t.Fatal(err)
	}
	if gs.Rows != 4 || gs.AnthRows != 3 || gs.AnthMatched != 1 {
		t.Errorf("GwStatus = %+v, want Rows=4 AnthRows=3 AnthMatched=1", gs)
	}
	if gs.ChainMax != 3 {
		t.Errorf("ChainMax = %d, want 3 (three comma-joined segments)", gs.ChainMax)
	}

	// Re-run: incremental cursor picks up after the last pg id, nothing new.
	stats2, err := ing.Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats2.New != 0 {
		t.Errorf("re-run New = %d, want 0", stats2.New)
	}
	last := src.cursors[len(src.cursors)-1]
	if last != 4 {
		t.Errorf("re-run cursor = %d, want 4 (max ingested pg id)", last)
	}
}

func TestMatchIsOneToOne(t *testing.T) {
	srcID = 0
	st := testStore(t)
	seedRequest(t, st, "req-single", t0, 500, 200, 0, 12000)

	src := &fakeSource{rows: []Row{
		anthRow(1, t0.Add(-30*time.Second), 500, 200, 0, 12000),
		anthRow(2, t0.Add(60*time.Second), 500, 200, 0, 12000),
	}}
	stats, err := (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	// One gw row wins the transcript request; the identical twin stays
	// unmatched rather than double-claiming.
	if stats.Matched != 1 || stats.Unmatched != 1 {
		t.Errorf("match stats = %+v, want Matched=1 Unmatched=1", stats)
	}
}

func TestLateTranscriptMatches(t *testing.T) {
	srcID = 0
	st := testStore(t)

	src := &fakeSource{rows: []Row{anthRow(1, t0, 42, 99, 1000, 2000)}}
	stats, err := (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 0 || stats.Unmatched != 1 {
		t.Errorf("pre-transcript stats = %+v, want Matched=0 Unmatched=1", stats)
	}

	// The transcript lands later (files are ingested after the fact);
	// the next match pass pairs it.
	seedRequest(t, st, "req-late", t0.Add(3*time.Minute), 42, 99, 1000, 2000)
	matched, unmatched, err := st.MatchGwRequests()
	if err != nil {
		t.Fatal(err)
	}
	if matched != 1 || unmatched != 0 {
		t.Errorf("late match = (%d, %d), want (1, 0)", matched, unmatched)
	}
}

func sessRow(id int64, ts time.Time, session string) Row {
	r := anthRow(id, ts, 700, 50, 0, 9000)
	r.SessionID = session
	return r
}

// Two sessions send look-alike requests (same usage tuple) seconds apart.
// By time alone gw row 1 is closest to session A's request; its session id
// says it belongs to B, and that wins.
func TestSessionIDPairsWithinSession(t *testing.T) {
	srcID = 0
	st := testStore(t)
	seedRequest(t, st, "A", t0.Add(10*time.Second), 700, 50, 0, 9000)
	seedRequest(t, st, "B", t0.Add(20*time.Second), 700, 50, 0, 9000)

	src := &fakeSource{rows: []Row{
		sessRow(1, t0.Add(9*time.Second), "sess-B"),
		sessRow(2, t0.Add(21*time.Second), "sess-A"),
	}}
	stats, err := (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 2 || stats.Unmatched != 0 {
		t.Fatalf("stats = %+v, want Matched=2", stats)
	}
	got := pairs(t, st)
	if got[1] != "B" || got[2] != "A" {
		t.Errorf("pairs = %v, want pg 1→B and pg 2→A", got)
	}
}

// A row that names its session never falls back to another session's
// request, even when that one is the only tuple match in the window.
func TestSessionIDNoCrossSessionFallback(t *testing.T) {
	srcID = 0
	st := testStore(t)
	seedRequest(t, st, "A", t0, 700, 50, 0, 9000)

	src := &fakeSource{rows: []Row{sessRow(1, t0, "sess-other")}}
	stats, err := (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 0 || stats.Unmatched != 1 {
		t.Errorf("stats = %+v, want Matched=0 Unmatched=1", stats)
	}
	// Its own transcript lands later and pairs on the next pass.
	seedRequest(t, st, "other", t0.Add(time.Minute), 700, 50, 0, 9000)
	if matched, _, err := st.MatchGwRequests(); err != nil || matched != 1 {
		t.Fatalf("late match = %d, %v; want 1", matched, err)
	}
	if got := pairs(t, st); got[1] != "other" {
		t.Errorf("pairs = %v, want pg 1→other", got)
	}
}

// pairs maps gw pg_id → the transcript request key it paired with.
func pairs(t *testing.T, st *store.Store) map[int64]string {
	t.Helper()
	out := map[int64]string{}
	// A second connection: the store holds its only one.
	db, err := sql.Open("sqlite", st.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT pg_id, request_key FROM gw_request WHERE request_key != ''`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			t.Fatal(err)
		}
		out[id] = key
	}
	return out
}
