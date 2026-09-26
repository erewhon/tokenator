package transcript

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func ocDBFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id text PRIMARY KEY, directory text, time_created integer, time_updated integer)`,
		`CREATE TABLE message (id text PRIMARY KEY, session_id text, time_created integer, time_updated integer, data text)`,
		`CREATE TABLE part (id text PRIMARY KEY, message_id text, session_id text, time_created integer, time_updated integer, data text)`,
		`INSERT INTO session VALUES ('ses_1', '/w', 1, 2)`,
		`INSERT INTO message VALUES ('msg_1', 'ses_1', 1751000000000, 1, '{"role":"user","time":{"created":1751000000000}}')`,
		`INSERT INTO message VALUES ('msg_2', 'ses_1', 1751000010000, 1, '{"role":"assistant","time":{"created":1751000010000}}')`,
		`INSERT INTO part VALUES ('prt_1', 'msg_1', 'ses_1', 1, 1, '{"type":"text","text":"Please fix the Login flow"}')`,
		`INSERT INTO part VALUES ('prt_2', 'msg_2', 'ses_1', 1, 1, '{"type":"reasoning","text":"thinking about it"}')`,
		`INSERT INTO part VALUES ('prt_3', 'msg_2', 'ses_1', 1, 1, '{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":"/w/app.py"},"output":"print(1)"}}')`,
		`INSERT INTO part VALUES ('prt_4', 'msg_2', 'ses_1', 1, 1, '{"type":"step-finish"}')`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return path
}

func TestOpenCodeDBLoadAndSearch(t *testing.T) {
	path := ocDBFixture(t)
	if _, err := Locate("opencode", path, "ses_1"); err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if _, err := Locate("opencode", path, "ses_missing"); err == nil {
		t.Fatal("Locate should fail for an unknown session")
	}
	entries, err := Load("opencode", path, "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	// user text, thinking, tool_use, tool_result — step-finish dropped.
	kinds := []string{}
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"user_text", "thinking", "tool_use", "tool_result"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if entries[0].Role != "user" || entries[1].Role != "assistant" || entries[3].Tool != "read" {
		t.Errorf("roles/tools: %+v", entries)
	}

	hits, err := Search("opencode", path, "ses_1", "login FLOW", 3)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Matches != 1 || len(hits.Hits) != 1 || hits.Hits[0].Kind != "user_text" {
		t.Errorf("search: %+v", hits)
	}
	hits, err = Search("opencode", path, "ses_1", "nowhere", 3)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Matches != 0 {
		t.Errorf("no-match search: %+v", hits)
	}
}
