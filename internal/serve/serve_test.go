package serve

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erewhon/tokenator/internal/store"
)

const fixtureJSONL = `{"type":"user","sessionId":"abc","timestamp":"2026-07-19T10:00:00Z","message":{"role":"user","content":"please fix the flaky widget test"}}
{"type":"assistant","sessionId":"abc","timestamp":"2026-07-19T10:00:05Z","message":{"id":"msg_1","content":[{"type":"text","text":"Looking at the widget spec now."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"widget_test.go"}}]}}
{"type":"user","sessionId":"abc","timestamp":"2026-07-19T10:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"func TestWidget(t *testing.T) { /* flaky sleep here */ }"}]}]}}
`

func fixtureServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	ccRoot := filepath.Join(dir, "projects")
	if err := os.MkdirAll(filepath.Join(ccRoot, "-proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ccRoot, "-proj", "abc.jsonl"), []byte(fixtureJSONL), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srcID, err := st.UpsertSource("claude_code", ccRoot, "subscription")
	if err != nil {
		t.Fatal(err)
	}
	err = st.WithTx(func(tx *store.Tx) error {
		id, err := tx.UpsertSession(store.Session{
			SourceID: srcID, HarnessID: "abc", Project: "widgets",
			Title: "fix flaky widget test", StartedAt: "2026-07-19T10:00:00Z",
			EndedAt: "2026-07-19T10:05:00Z",
		})
		if err != nil {
			return err
		}
		_, err = tx.InsertRequest(store.Request{
			SessionID: id, TS: "2026-07-19T10:00:05Z", Model: "claude-sonnet-5",
			InputTokens: 12, OutputTokens: 40, CacheReadTokens: 1000,
			DedupeKey: "req1",
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Store: st}
}

func get(t *testing.T, srv *Server, path string) string {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestIndexList(t *testing.T) {
	body := get(t, fixtureServer(t), "/")
	for _, want := range []string{"fix flaky widget test", "widgets", "/session/abc"} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
}

func TestIndexSearch(t *testing.T) {
	body := get(t, fixtureServer(t), "/?q=flaky+sleep")
	if !strings.Contains(body, "<mark>flaky sleep</mark>") {
		t.Error("search results missing highlighted snippet")
	}
	if !strings.Contains(body, "#e3") {
		t.Error("search results missing anchor link to matching entry")
	}
	// Absent term matches nothing.
	body = get(t, fixtureServer(t), "/?q=zebra-unicorn")
	if !strings.Contains(body, "no matches") {
		t.Error("expected no matches")
	}
}

func TestTranscript(t *testing.T) {
	body := get(t, fixtureServer(t), "/session/abc/transcript?q=flaky")
	for _, want := range []string{`id="e0"`, `id="e3"`, "tool_result", "<mark>flaky</mark>", "widget spec"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript missing %q", want)
		}
	}
}

func TestProfilePage(t *testing.T) {
	body := get(t, fixtureServer(t), "/session/abc")
	if !strings.Contains(body, "transcript") || !strings.Contains(body, "fix flaky widget test") {
		t.Error("profile page missing nav or label")
	}
}
