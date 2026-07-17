package claudecode

import (
	"os"
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

// copyFixture copies testdata/projects into a temp dir so tests can mutate
// mtimes without dirtying the repo.
func copyFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "projects")
	src := filepath.Join("testdata", "projects", "-home-user-proj", "11111111-1111-1111-1111-111111111111.jsonl")
	dst := filepath.Join(root, "-home-user-proj", "11111111-1111-1111-1111-111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestIngestFixture(t *testing.T) {
	st := testStore(t)
	root := copyFixture(t)
	ing := &Ingester{Root: root}

	stats, err := ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 || stats.FilesSkipped != 0 {
		t.Errorf("files: got %d/%d skipped, want 1/0", stats.Files, stats.FilesSkipped)
	}
	if stats.Requests != 3 {
		t.Errorf("requests inserted: got %d, want 3", stats.Requests)
	}
	if stats.Duplicates != 2 {
		t.Errorf("duplicates: got %d, want 2 (repeated content-block lines for msg_A and msg_B)", stats.Duplicates)
	}
	if stats.Synthetic != 1 {
		t.Errorf("synthetic: got %d, want 1", stats.Synthetic)
	}
	if stats.Compactions != 1 {
		t.Errorf("compactions: got %d, want 1", stats.Compactions)
	}
	if stats.Sessions != 1 {
		t.Errorf("sessions: got %d, want 1", stats.Sessions)
	}
	if stats.ParseErrors != 0 {
		t.Errorf("parse errors: got %d, want 0", stats.ParseErrors)
	}
	if stats.Blocks != 10 {
		t.Errorf("blocks: got %d, want 10 (2 user_text, 1 meta_text, 2 assistant_text, 1 thinking, 2 tool_use, 2 tool_result)", stats.Blocks)
	}

	rows, err := st.Rollup("project", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rollup rows: got %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Group != "proj" {
		t.Errorf("project: got %q, want proj", r.Group)
	}
	if r.Requests != 3 || r.Input != 35 || r.Output != 15 || r.CacheRead != 300 || r.CacheCreate != 50 {
		t.Errorf("totals: got req=%d in=%d out=%d rd=%d wr=%d, want 3/35/15/300/50",
			r.Requests, r.Input, r.Output, r.CacheRead, r.CacheCreate)
	}

	// Custom title must win over ai-title.
	sess, err := st.Rollup("session", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "11111111-1111-1111-1111-111111111111 — Custom title"
	if sess[0].Group != want {
		t.Errorf("session label: got %q, want %q", sess[0].Group, want)
	}
}

func TestBlockAttribution(t *testing.T) {
	st := testStore(t)
	ing := &Ingester{Root: copyFixture(t)}
	if _, err := ing.Run(st); err != nil {
		t.Fatal(err)
	}

	// By kind: full composition of extracted blocks.
	kinds := map[string][2]int64{} // kind -> {blocks, est}
	kindRows, err := st.AttrRollup("kind", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range kindRows {
		kinds[r.Group] = [2]int64{r.Blocks, r.EstTokens}
	}
	for kind, wantBlocks := range map[string]int64{
		"user_text": 2, "meta_text": 1, "assistant_text": 2,
		"thinking": 1, "tool_use": 2, "tool_result": 2,
	} {
		if kinds[kind][0] != wantBlocks {
			t.Errorf("kind %s: got %d blocks, want %d", kind, kinds[kind][0], wantBlocks)
		}
	}
	// "pondering deeply" = 16 bytes -> 4 est tokens.
	if kinds["thinking"][1] != 4 {
		t.Errorf("thinking est: got %d, want 4", kinds["thinking"][1])
	}

	// By tool: results resolve to the tool that requested them.
	toolRows, err := st.AttrRollup("tool", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]int64{}
	for _, r := range toolRows {
		tools[r.Group] = r.Blocks
	}
	if tools["Bash"] != 1 || tools["Read"] != 1 {
		t.Errorf("tool attribution: got %v, want Bash=1 Read=1", tools)
	}

	// By file: Read result attributed to its path (via tool_use input).
	fileRows, err := st.AttrRollup("file", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fileRows) != 1 || fileRows[0].Group != "/home/user/proj/main.go" {
		t.Errorf("file attribution: %+v", fileRows)
	}

	// Calibration denominators come straight from metered usage.
	cal, err := st.Calibrate("")
	if err != nil {
		t.Fatal(err)
	}
	if cal.InMetered != 85 || cal.OutMetered != 15 {
		t.Errorf("calibration metered: got in=%d out=%d, want 85/15", cal.InMetered, cal.OutMetered)
	}
	if cal.InBlockEst == 0 || cal.OutBlockEst == 0 {
		t.Errorf("calibration estimates should be non-zero: %+v", cal)
	}
}

func TestIngestIdempotent(t *testing.T) {
	st := testStore(t)
	root := copyFixture(t)
	ing := &Ingester{Root: root}

	if _, err := ing.Run(st); err != nil {
		t.Fatal(err)
	}

	// Second run: file unchanged, nothing re-parsed.
	stats, err := ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesSkipped != 1 || stats.Files != 0 || stats.Requests != 0 {
		t.Errorf("unchanged rerun: got files=%d skipped=%d requests=%d, want 0/1/0",
			stats.Files, stats.FilesSkipped, stats.Requests)
	}

	// Touch the file: full re-parse, but global dedupe keeps totals stable.
	path := filepath.Join(root, "-home-user-proj", "11111111-1111-1111-1111-111111111111.jsonl")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	stats, err = ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 0 || stats.Duplicates != 5 {
		t.Errorf("touched rerun: got requests=%d dup=%d, want 0/5", stats.Requests, stats.Duplicates)
	}
	if stats.Blocks != 0 || stats.BlocksUpdated != 10 {
		t.Errorf("touched rerun blocks: got +%d/%d updated, want 0/10", stats.Blocks, stats.BlocksUpdated)
	}

	rows, err := st.Rollup("project", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 3 || rows[0].Input != 35 {
		t.Errorf("totals drifted after re-ingest: %+v", rows)
	}
}
