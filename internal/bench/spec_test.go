package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSpec(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSpec(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	os.MkdirAll(ws, 0o755)
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("do the task\ncarefully"), 0o644)

	p := writeSpec(t, dir, `{
		"name": "exp1",
		"prompt_file": "prompt.txt",
		"workspace": "ws",
		"checks": [{"cmd": "true"}],
		"arms": [
			{"name": "a", "harness": "claude_code", "model": "sonnet", "claude": {"bare": true}},
			{"name": "b", "harness": "opencode", "opencode": {"pure": true}}
		]
	}`)
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Trials != 3 || s.TimeoutSeconds != 900 || s.Regime != "subscription" {
		t.Errorf("defaults not applied: %+v", s)
	}
	if s.Prompt != "do the task\ncarefully" || s.PromptFile != "" {
		t.Errorf("prompt_file not inlined: %q", s.Prompt)
	}
	if !filepath.IsAbs(s.Workspace) {
		t.Errorf("workspace not absolutized: %s", s.Workspace)
	}
	if s.Checks[0].Name != "check1" {
		t.Errorf("check default name = %q", s.Checks[0].Name)
	}
	// Snapshot round-trip preserves the resolved form.
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := FromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Prompt != s.Prompt || len(s2.Arms) != 2 {
		t.Error("snapshot round-trip lost data")
	}
}

func TestLoadSpecErrors(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	os.MkdirAll(ws, 0o755)
	cases := []struct {
		name, body, want string
	}{
		{"no-arms", `{"name":"x","prompt":"p","workspace":"ws","arms":[]}`, "at least one arm"},
		{"bad-harness", `{"name":"x","prompt":"p","workspace":"ws","arms":[{"name":"a","harness":"cursor"}]}`, "unknown harness"},
		{"both-prompts", `{"name":"x","prompt":"p","prompt_file":"f","workspace":"ws","arms":[{"name":"a","harness":"opencode"}]}`, "exactly one of"},
		{"dup-arm", `{"name":"x","prompt":"p","workspace":"ws","arms":[{"name":"a","harness":"opencode"},{"name":"a","harness":"opencode"}]}`, "duplicate arm"},
		{"cross-opts", `{"name":"x","prompt":"p","workspace":"ws","arms":[{"name":"a","harness":"opencode","claude":{"bare":true}}]}`, "claude options"},
		{"missing-ws", `{"name":"x","prompt":"p","workspace":"nope","arms":[{"name":"a","harness":"opencode"}]}`, "not a directory"},
	}
	for _, c := range cases {
		_, err := Load(writeSpec(t, dir, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}
