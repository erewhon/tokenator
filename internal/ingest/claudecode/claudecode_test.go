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
	if stats.Requests != 2 {
		t.Errorf("requests inserted: got %d, want 2", stats.Requests)
	}
	if stats.Duplicates != 1 {
		t.Errorf("duplicates: got %d, want 1 (repeated content-block line for msg_A)", stats.Duplicates)
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
	if r.Requests != 2 || r.Input != 30 || r.Output != 12 || r.CacheRead != 300 || r.CacheCreate != 50 {
		t.Errorf("totals: got req=%d in=%d out=%d rd=%d wr=%d, want 2/30/12/300/50",
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
	if stats.Requests != 0 || stats.Duplicates != 3 {
		t.Errorf("touched rerun: got requests=%d dup=%d, want 0/3", stats.Requests, stats.Duplicates)
	}

	rows, err := st.Rollup("project", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 2 || rows[0].Input != 30 {
		t.Errorf("totals drifted after re-ingest: %+v", rows)
	}
}
