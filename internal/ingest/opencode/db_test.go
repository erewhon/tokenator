package opencode

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

// The subset of OpenCode 1.18's schema the ingester reads.
const ocSchema = `
CREATE TABLE session (id text PRIMARY KEY, project_id text NOT NULL, parent_id text, slug text NOT NULL,
  directory text NOT NULL, title text NOT NULL, version text NOT NULL, time_created integer NOT NULL,
  time_updated integer NOT NULL);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL,
  time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL,
  time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
`

func openFixtureDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func execJSON(t *testing.T, db *sql.DB, q string, v any, args ...any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(q, append(args, string(data))...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// dbFixture mirrors fixture(): one session, a user message, a completed
// assistant message with parts, and a streaming (zero-token) assistant
// message — as opencode.db rows. Ids and sessionID/messageID are columns,
// not in the JSON, exactly as OpenCode 1.18 writes them.
func dbFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db := openFixtureDB(t, path)
	if _, err := db.Exec(ocSchema); err != nil {
		t.Fatal(err)
	}
	ses := "ses_test1"
	if _, err := db.Exec(`INSERT INTO session VALUES (?, 'proj1', NULL, 'quick-fox', '/home/user/webapp', 'Fix the login flow', '1.18.30', 1751000000000, 1751000300000)`, ses); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`
	execJSON(t, db, ins, map[string]any{"role": "user", "time": map[string]any{"created": 1751000000000}},
		"msg_user1", ses, 1751000000000, 1751000000000)
	execJSON(t, db, ins, map[string]any{
		"role": "assistant", "modelID": "qwen/qwen3-coder", "providerID": "lmstudio", "cost": 0.0, "finish": "stop",
		"time": map[string]any{"created": 1751000010000, "completed": 1751000020000},
		"tokens": map[string]any{"total": 1120, "input": 1000, "output": 100, "reasoning": 20,
			"cache": map[string]any{"read": 5000, "write": 250}},
	}, "msg_asst1", ses, 1751000010000, 1751000020000)
	execJSON(t, db, ins, map[string]any{
		"role": "assistant", "modelID": "qwen/qwen3-coder", "providerID": "lmstudio",
		"time":   map[string]any{"created": 1751000030000},
		"tokens": map[string]any{"total": 0, "input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
	}, "msg_asst2", ses, 1751000030000, 1751000030000)
	pins := `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`
	execJSON(t, db, pins, map[string]any{"type": "text", "text": "hello opencode"}, "prt_u1", "msg_user1", ses, 1751000000000, 1751000000000)
	execJSON(t, db, pins, map[string]any{"type": "text", "text": "here's the fix"}, "prt_a1", "msg_asst1", ses, 1751000011000, 1751000011000)
	execJSON(t, db, pins, map[string]any{"type": "tool", "tool": "read", "callID": "call_1",
		"state": map[string]any{"status": "completed", "input": map[string]any{"filePath": "/home/user/webapp/app.py"}, "output": "print('hi')"}},
		"prt_t1", "msg_asst1", ses, 1751000012000, 1751000012000)
	execJSON(t, db, pins, map[string]any{"type": "step-start"}, "prt_s1", "msg_asst1", ses, 1751000010500, 1751000010500)
	return path
}

func TestIngestDBFixture(t *testing.T) {
	st := testStore(t)
	path := dbFixture(t)
	ing := &Ingester{Root: filepath.Join(t.TempDir(), "no-tree"), DB: path}

	stats, err := ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionFiles != 1 || stats.MessageFiles != 3 || stats.PartFiles != 4 {
		t.Errorf("parsed rows: %+v", stats)
	}
	if stats.Requests != 1 || stats.Updated != 0 || stats.Incomplete != 1 || stats.Blocks != 3 {
		t.Errorf("got requests=%d updated=%d incomplete=%d blocks=%d, want 1/0/1/3", stats.Requests, stats.Updated, stats.Incomplete, stats.Blocks)
	}
	fileRows, err := st.AttrRollup("file", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fileRows) != 1 || fileRows[0].Group != "/home/user/webapp/app.py" {
		t.Errorf("file attribution: %+v", fileRows)
	}
	rows, err := st.Rollup("project", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Group != "webapp" || rows[0].Input != 1000 || rows[0].Output != 100 || rows[0].CacheRead != 5000 || rows[0].CacheCreate != 250 {
		t.Fatalf("rollup: %+v", rows)
	}

	// Unchanged rerun reads nothing.
	stats, err = ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionFiles+stats.MessageFiles+stats.PartFiles != 0 || stats.Requests != 0 || stats.FilesSkipped != 8 {
		t.Errorf("unchanged rerun: %+v", stats)
	}
}

func TestIngestDBMutatingMessage(t *testing.T) {
	st := testStore(t)
	path := dbFixture(t)
	ing := &Ingester{Root: filepath.Join(t.TempDir(), "no-tree"), DB: path}
	if _, err := ing.Run(st); err != nil {
		t.Fatal(err)
	}
	db := openFixtureDB(t, path)
	upd := `UPDATE message SET data = ?, time_updated = ? WHERE id = ?`
	// The streaming message completes: new request.
	execJSON2 := func(v any, updated int64, id string) {
		data, _ := json.Marshal(v)
		if _, err := db.Exec(upd, string(data), updated, id); err != nil {
			t.Fatal(err)
		}
	}
	execJSON2(map[string]any{
		"role": "assistant", "modelID": "qwen/qwen3-coder", "providerID": "lmstudio", "cost": 0.0, "finish": "stop",
		"time":   map[string]any{"created": 1751000030000, "completed": 1751000040000},
		"tokens": map[string]any{"total": 660, "input": 600, "output": 60, "reasoning": 0, "cache": map[string]any{"read": 2000, "write": 0}},
	}, 1751000040000, "msg_asst2")
	// The completed one grows in place: update, not a double count.
	execJSON2(map[string]any{
		"role": "assistant", "modelID": "qwen/qwen3-coder", "providerID": "lmstudio", "cost": 0.0, "finish": "stop",
		"time":   map[string]any{"created": 1751000010000, "completed": 1751000025000},
		"tokens": map[string]any{"total": 1350, "input": 1200, "output": 130, "reasoning": 20, "cache": map[string]any{"read": 5000, "write": 250}},
	}, 1751000025000, "msg_asst1")

	stats, err := ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.MessageFiles != 2 || stats.Requests != 1 || stats.Updated != 1 {
		t.Errorf("got messages=%d requests=%d updated=%d, want 2/1/1", stats.MessageFiles, stats.Requests, stats.Updated)
	}
	rows, err := st.Rollup("project", "")
	if err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	if r.Requests != 2 || r.Input != 1800 || r.Output != 190 || r.CacheRead != 7000 || r.CacheCreate != 250 {
		t.Errorf("totals after mutation: req=%d in=%d out=%d rd=%d wr=%d, want 2/1800/190/7000/250", r.Requests, r.Input, r.Output, r.CacheRead, r.CacheCreate)
	}
}

func TestRunNeitherStorage(t *testing.T) {
	st := testStore(t)
	dir := t.TempDir()
	ing := &Ingester{Root: filepath.Join(dir, "storage"), DB: filepath.Join(dir, "opencode.db")}
	if _, err := ing.Run(st); !errors.Is(err, ErrNoStorage) {
		t.Fatalf("want ErrNoStorage, got %v", err)
	}
}

func TestRunTreeAndDBCountMessageOnce(t *testing.T) {
	st := testStore(t)
	// The same session in both layouts (a host that migrated): two sources,
	// one request row per message.
	ing := &Ingester{Root: fixture(t), DB: dbFixture(t)}
	stats, err := ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 1 || stats.Updated != 1 {
		t.Errorf("tree+db: requests=%d updated=%d, want 1 new + 1 refreshed", stats.Requests, stats.Updated)
	}
	rows, err := st.Rollup("project", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("message must be counted once across sources: %+v", rows)
	}
}
