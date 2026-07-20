package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

const fixtureJSONL = `{"type":"user","sessionId":"abc","timestamp":"2026-07-19T10:00:00Z","message":{"role":"user","content":"find the bug in parser.go"}}
{"type":"assistant","sessionId":"abc","timestamp":"2026-07-19T10:00:05Z","message":{"id":"msg_1","content":[{"type":"thinking","thinking":"let me look"},{"type":"text","text":"Checking parser.go now."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"parser.go"}}]}}
{"type":"user","sessionId":"abc","timestamp":"2026-07-19T10:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"func Parse() { panic(\"boom\") }"}]}]}}
{"type":"user","sessionId":"abc","timestamp":"2026-07-19T10:00:07Z","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"<system-reminder>hook noise</system-reminder>"}]}}
{"type":"system","subtype":"compact_boundary","timestamp":"2026-07-19T10:01:00Z","compactMetadata":{"trigger":"auto","preTokens":150000}}
{"type":"summary","summary":"irrelevant"}
`

func writeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "-home-user-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "abc.jsonl"), []byte(fixtureJSONL), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadClaudeCode(t *testing.T) {
	root := writeFixture(t)
	entries, err := Load("claude_code", root, "abc")
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"user_text", "thinking", "assistant_text", "tool_use", "tool_result", "meta_text", "compaction"}
	if len(entries) != len(wantKinds) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(wantKinds), entries)
	}
	for i, k := range wantKinds {
		if entries[i].Kind != k {
			t.Errorf("entry %d kind = %q, want %q", i, entries[i].Kind, k)
		}
		if entries[i].Idx != i {
			t.Errorf("entry %d Idx = %d", i, entries[i].Idx)
		}
	}
	if entries[3].Tool != "Read" {
		t.Errorf("tool_use tool = %q, want Read", entries[3].Tool)
	}
	if entries[4].Tool != "Read" {
		t.Errorf("tool_result tool = %q, want Read (via toolu id)", entries[4].Tool)
	}
	if want := `func Parse() { panic("boom") }`; entries[4].Text != want {
		t.Errorf("tool_result text = %q, want %q", entries[4].Text, want)
	}
}

func TestSearch(t *testing.T) {
	root := writeFixture(t)
	hits, err := Search("claude_code", root, "abc", "PANIC", 10)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Matches != 1 || len(hits.Hits) != 1 {
		t.Fatalf("matches = %d hits = %d, want 1/1", hits.Matches, len(hits.Hits))
	}
	h := hits.Hits[0]
	if h.Kind != "tool_result" || h.EntryIdx != 4 {
		t.Errorf("hit = %+v, want tool_result at idx 4", h)
	}
	if h.Snippet == "" {
		t.Error("empty snippet")
	}

	// No match: the raw fast path must not false-positive on absent text.
	hits, err = Search("claude_code", root, "abc", "zebra unicorn", 10)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Matches != 0 {
		t.Fatalf("matches = %d, want 0", hits.Matches)
	}

	// Query with JSON-escaped chars takes the parse path and still matches.
	hits, err = Search("claude_code", root, "abc", `panic("boom")`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Matches != 1 {
		t.Fatalf(`escaped-char query matches = %d, want 1`, hits.Matches)
	}
}

func TestSearchQuickSkip(t *testing.T) {
	// A term absent from raw bytes must short-circuit before parsing.
	root := writeFixture(t)
	m, err := rawMatch("claude_code", root, "abc", "reminder")
	if err != nil || !m {
		t.Fatalf("rawMatch(reminder) = %v, %v; want true", m, err)
	}
	m, err = rawMatch("claude_code", root, "abc", "nonexistent-term-xyz")
	if err != nil || m {
		t.Fatalf("rawMatch(absent) = %v, %v; want false", m, err)
	}
}
