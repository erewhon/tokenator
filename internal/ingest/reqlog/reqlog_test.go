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

// chatRow is an opencode-style /v1/chat/completions row: the router logs
// prompt/completion tokens and, when the provider reports them, cached
// prompt tokens; there is no cache-creation figure for chat.
func chatRow(id int64, ts time.Time, session string, in, out int64, cached *int64) Row {
	return Row{
		ID: id, RequestID: "c16chars", TS: ts, Method: "POST", Path: "/v1/chat/completions",
		Model: "lightning", BackendURL: "http://talos:5391", ResolvedVia: "static",
		APIClass: "chat", Stream: true, Status: 200, LatencyMS: 900,
		PromptTokens: iptr(in), CompletionToks: iptr(out), CacheRead: cached,
		SessionID: session,
	}
}

// Two concurrent opencode sessions send the same usage tuple; each gateway
// row pairs inside the session its ses_… id names, whatever the timing.
func TestChatPairsWithinSession(t *testing.T) {
	srcID = 0
	st := testStore(t)
	seedRequest(t, st, "ocA", t0.Add(10*time.Second), 35103, 38, 0, 0)
	seedRequest(t, st, "ocB", t0.Add(20*time.Second), 35103, 38, 0, 0)

	src := &fakeSource{rows: []Row{
		chatRow(1, t0.Add(9*time.Second), "sess-ocB", 35103, 38, nil),
		chatRow(2, t0.Add(21*time.Second), "sess-ocA", 35103, 38, nil),
	}}
	stats, err := (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 2 || stats.Unmatched != 0 {
		t.Fatalf("stats = %+v, want Matched=2", stats)
	}
	if got := pairs(t, st); got[1] != "ocB" || got[2] != "ocA" {
		t.Errorf("pairs = %v, want pg 1→ocB and pg 2→ocA", got)
	}
}

// A chat row with no session id is never paired, even when a transcript
// request with the identical tuple sits right next to it: chat traffic
// without a session is curl, pipelines, other machines.
func TestChatWithoutSessionStaysUnpaired(t *testing.T) {
	srcID = 0
	st := testStore(t)
	seedRequest(t, st, "ocA", t0, 35103, 38, 0, 0)

	src := &fakeSource{rows: []Row{chatRow(1, t0, "", 35103, 38, nil)}}
	stats, err := (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 0 {
		t.Fatalf("stats = %+v, want Matched=0", stats)
	}
	if got := pairs(t, st); len(got) != 0 {
		t.Errorf("pairs = %v, want none", got)
	}
	// It is not a candidate at all, so it does not count as unmatched either.
	if stats.Unmatched != 0 {
		t.Errorf("Unmatched = %d, want 0 (session-less chat rows are not candidates)", stats.Unmatched)
	}
}

// The provider reported cached prompt tokens: the router logs the gross
// prompt (35000) plus cached (5000); opencode records input net of cache
// (30000) with cache read 5000. No exact tuple match, but the same session,
// output and prompt total pair on the fallback.
func TestChatCachedTokensFallback(t *testing.T) {
	srcID = 0
	st := testStore(t)
	seedRequest(t, st, "ocA", t0, 30000, 190, 0, 5000)
	// Same session, different output: must not be picked by the fallback.
	seedRequest(t, st, "ocA2", t0.Add(5*time.Second), 30000, 191, 0, 5000)

	src := &fakeSource{rows: []Row{chatRow(1, t0.Add(-2*time.Second), "sess-ocA", 35000, 190, iptr(5000))}}
	stats, err := (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 1 || stats.Unmatched != 0 {
		t.Fatalf("stats = %+v, want Matched=1", stats)
	}
	if got := pairs(t, st); got[1] != "ocA" {
		t.Errorf("pairs = %v, want pg 1→ocA", got)
	}
}

// The fallback never leaves the session either.
func TestChatFallbackNoCrossSession(t *testing.T) {
	srcID = 0
	st := testStore(t)
	seedRequest(t, st, "ocB", t0, 30000, 190, 0, 5000)

	src := &fakeSource{rows: []Row{chatRow(1, t0, "sess-ocA", 35000, 190, iptr(5000))}}
	stats, err := (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 0 || stats.Unmatched != 1 {
		t.Errorf("stats = %+v, want Matched=0 Unmatched=1", stats)
	}
}

// The incremental pass leaves the historical backlog alone: a gateway row
// that has already been tried is not re-tried until a transcript request
// lands in its time window, and rows far from any new request are never
// touched. MatchGwRequestsAll sweeps everything.
func TestMatchIsIncremental(t *testing.T) {
	srcID = 0
	st := testStore(t)
	src := &fakeSource{rows: []Row{
		anthRow(1, t0, 42, 99, 1000, 2000),            // its transcript arrives later
		anthRow(2, t0.Add(-48*time.Hour), 7, 7, 0, 0), // never gets one
	}}
	stats, err := (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 0 || stats.Unmatched != 2 {
		t.Fatalf("first pass = %+v, want Unmatched=2", stats)
	}
	// Nothing new: nothing to consider.
	if m, u, err := st.MatchGwRequests(); err != nil || m != 0 || u != 0 {
		t.Fatalf("idle pass = (%d, %d, %v), want (0, 0)", m, u, err)
	}
	// A transcript request near row 1 makes row 1 (only) a candidate again.
	seedRequest(t, st, "late", t0.Add(2*time.Minute), 42, 99, 1000, 2000)
	if m, u, err := st.MatchGwRequests(); err != nil || m != 1 || u != 0 {
		t.Fatalf("after late transcript = (%d, %d, %v), want (1, 0): row 2 is out of range and must not be scanned", m, u, err)
	}
	if got := pairs(t, st); got[1] != "late" {
		t.Errorf("pairs = %v, want pg 1→late", got)
	}
	// The full sweep still sees the old row.
	if m, u, err := st.MatchGwRequestsAll(); err != nil || m != 0 || u != 1 {
		t.Fatalf("full sweep = (%d, %d, %v), want (0, 1)", m, u, err)
	}
	// A new gateway row is always considered, even with no new transcript.
	src.rows = append(src.rows, anthRow(3, t0.Add(-24*time.Hour), 8, 8, 0, 0))
	stats, err = (&Ingester{Src: src}).Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.New != 1 || stats.Unmatched != 1 {
		t.Fatalf("new gw row pass = %+v, want New=1 Unmatched=1 (only the new row)", stats)
	}
}
