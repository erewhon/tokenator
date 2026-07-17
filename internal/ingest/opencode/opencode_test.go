package opencode

import (
	"encoding/json"
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

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture builds a storage tree with one session: a user message, a
// completed assistant message, and an incomplete (still streaming,
// zero-token) assistant message.
func fixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "storage")
	ses := "ses_test1"
	writeJSON(t, filepath.Join(root, "session", "proj1", ses+".json"), map[string]any{
		"id": ses, "slug": "quick-fox", "directory": "/home/user/webapp",
		"title": "Fix the login flow",
		"time":  map[string]any{"created": 1751000000000, "updated": 1751000300000},
	})
	writeJSON(t, filepath.Join(root, "message", ses, "msg_user1.json"), map[string]any{
		"id": "msg_user1", "sessionID": ses, "role": "user",
		"time": map[string]any{"created": 1751000000000},
	})
	writeJSON(t, filepath.Join(root, "message", ses, "msg_asst1.json"), map[string]any{
		"id": "msg_asst1", "sessionID": ses, "role": "assistant",
		"modelID": "qwen/qwen3-coder", "providerID": "lmstudio", "cost": 0.0,
		"finish": "stop",
		"time":   map[string]any{"created": 1751000010000, "completed": 1751000020000},
		"tokens": map[string]any{
			"total": 1120, "input": 1000, "output": 100, "reasoning": 20,
			"cache": map[string]any{"read": 5000, "write": 250},
		},
	})
	writeJSON(t, filepath.Join(root, "message", ses, "msg_asst2.json"), map[string]any{
		"id": "msg_asst2", "sessionID": ses, "role": "assistant",
		"modelID": "qwen/qwen3-coder", "providerID": "lmstudio",
		"time": map[string]any{"created": 1751000030000},
		"tokens": map[string]any{"total": 0, "input": 0, "output": 0, "reasoning": 0,
			"cache": map[string]any{"read": 0, "write": 0}},
	})
	// Content parts: user text, assistant text, and a tool call with output.
	writeJSON(t, filepath.Join(root, "part", "msg_user1", "prt_u1.json"), map[string]any{
		"id": "prt_u1", "sessionID": ses, "messageID": "msg_user1",
		"type": "text", "text": "hello opencode",
	})
	writeJSON(t, filepath.Join(root, "part", "msg_asst1", "prt_a1.json"), map[string]any{
		"id": "prt_a1", "sessionID": ses, "messageID": "msg_asst1",
		"type": "text", "text": "here's the fix",
	})
	writeJSON(t, filepath.Join(root, "part", "msg_asst1", "prt_t1.json"), map[string]any{
		"id": "prt_t1", "sessionID": ses, "messageID": "msg_asst1",
		"type": "tool", "tool": "read", "callID": "call_1",
		"state": map[string]any{
			"status": "completed",
			"input":  map[string]any{"filePath": "/home/user/webapp/app.py"},
			"output": "print('hi')",
		},
	})
	// step-start parts are bookkeeping and must not become blocks.
	writeJSON(t, filepath.Join(root, "part", "msg_asst1", "prt_s1.json"), map[string]any{
		"id": "prt_s1", "sessionID": ses, "messageID": "msg_asst1", "type": "step-start",
	})
	return root
}

func TestIngestFixture(t *testing.T) {
	st := testStore(t)
	root := fixture(t)
	ing := &Ingester{Root: root}

	stats, err := ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionFiles != 1 || stats.MessageFiles != 3 {
		t.Errorf("parsed: got %d session / %d message files, want 1/3", stats.SessionFiles, stats.MessageFiles)
	}
	if stats.Requests != 1 || stats.Updated != 0 || stats.Incomplete != 1 {
		t.Errorf("got requests=%d updated=%d incomplete=%d, want 1/0/1",
			stats.Requests, stats.Updated, stats.Incomplete)
	}
	if stats.PartFiles != 4 || stats.Blocks != 3 {
		t.Errorf("parts: got %d files / %d blocks, want 4 files -> 3 blocks (step-start skipped)",
			stats.PartFiles, stats.Blocks)
	}

	// Tool part resolves tool name and file path.
	fileRows, err := st.AttrRollup("file", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fileRows) != 1 || fileRows[0].Group != "/home/user/webapp/app.py" {
		t.Errorf("file attribution: %+v", fileRows)
	}
	toolRows, err := st.AttrRollup("tool", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(toolRows) != 1 || toolRows[0].Group != "read" {
		t.Errorf("tool attribution: %+v", toolRows)
	}

	rows, err := st.Rollup("project", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Group != "webapp" {
		t.Fatalf("rollup: %+v", rows)
	}
	r := rows[0]
	if r.Input != 1000 || r.Output != 100 || r.CacheRead != 5000 || r.CacheCreate != 250 {
		t.Errorf("totals: in=%d out=%d rd=%d wr=%d, want 1000/100/5000/250",
			r.Input, r.Output, r.CacheRead, r.CacheCreate)
	}
}

func TestIngestMutatingMessage(t *testing.T) {
	st := testStore(t)
	root := fixture(t)
	ing := &Ingester{Root: root}

	if _, err := ing.Run(st); err != nil {
		t.Fatal(err)
	}

	// Unchanged rerun parses nothing.
	stats, err := ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionFiles != 0 || stats.MessageFiles != 0 || stats.Requests != 0 {
		t.Errorf("unchanged rerun parsed files: %+v", stats)
	}

	// The incomplete message finishes: file is rewritten with tokens.
	// It must be picked up as a NEW request (it was skipped before).
	ses := "ses_test1"
	msgPath := filepath.Join(root, "message", ses, "msg_asst2.json")
	writeJSON(t, msgPath, map[string]any{
		"id": "msg_asst2", "sessionID": ses, "role": "assistant",
		"modelID": "qwen/qwen3-coder", "providerID": "lmstudio", "cost": 0.0,
		"finish": "stop",
		"time":   map[string]any{"created": 1751000030000, "completed": 1751000040000},
		"tokens": map[string]any{"total": 660, "input": 600, "output": 60, "reasoning": 0,
			"cache": map[string]any{"read": 2000, "write": 0}},
	})
	future := time.Now().Add(time.Hour)
	os.Chtimes(msgPath, future, future)

	// The completed message's usage grows (same id, message file mutated):
	// must UPDATE in place, not double count.
	msg1Path := filepath.Join(root, "message", ses, "msg_asst1.json")
	writeJSON(t, msg1Path, map[string]any{
		"id": "msg_asst1", "sessionID": ses, "role": "assistant",
		"modelID": "qwen/qwen3-coder", "providerID": "lmstudio", "cost": 0.0,
		"finish": "stop",
		"time":   map[string]any{"created": 1751000010000, "completed": 1751000025000},
		"tokens": map[string]any{"total": 1350, "input": 1200, "output": 130, "reasoning": 20,
			"cache": map[string]any{"read": 5000, "write": 250}},
	})
	os.Chtimes(msg1Path, future, future)

	stats, err = ing.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 1 || stats.Updated != 1 {
		t.Errorf("got requests=%d updated=%d, want 1 new (asst2) + 1 updated (asst1)",
			stats.Requests, stats.Updated)
	}

	rows, err := st.Rollup("project", "")
	if err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	// asst1 (updated): 1200/130/5000/250 + asst2 (new): 600/60/2000/0
	if r.Requests != 2 || r.Input != 1800 || r.Output != 190 || r.CacheRead != 7000 || r.CacheCreate != 250 {
		t.Errorf("totals after mutation: req=%d in=%d out=%d rd=%d wr=%d, want 2/1800/190/7000/250",
			r.Requests, r.Input, r.Output, r.CacheRead, r.CacheCreate)
	}
}
