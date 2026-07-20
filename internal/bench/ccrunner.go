package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/erewhon/tokenator/internal/ingest/claudecode"
	"github.com/erewhon/tokenator/internal/store"
)

// ccHarness runs Claude Code headlessly with full state isolation via
// CLAUDE_CONFIG_DIR. The session id is pre-assigned with --session-id, so
// the trial→session join is exact; transcripts land under
// <home>/projects/... and are ingested per trial.
type ccHarness struct{}

func (ccHarness) Kind() string { return HarnessClaudeCode }

// Seed copies OAuth credentials into the isolated config dir (a fresh
// CLAUDE_CONFIG_DIR has none, which would force interactive login) and marks
// onboarding complete so nothing prompts. ANTHROPIC_API_KEY arms work
// without credentials — missing file is only an error if neither is present.
func (ccHarness) Seed(t *trialCtx) error {
	if err := os.MkdirAll(t.Home, 0o755); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	copied, err := copyFile(filepath.Join(home, ".claude", ".credentials.json"),
		filepath.Join(t.Home, ".credentials.json"), 0o600)
	if err != nil {
		return err
	}
	if !copied && os.Getenv("ANTHROPIC_API_KEY") == "" &&
		t.Spec.Env["ANTHROPIC_API_KEY"] == "" && t.Arm.Env["ANTHROPIC_API_KEY"] == "" {
		return fmt.Errorf("no ~/.claude/.credentials.json to seed and no ANTHROPIC_API_KEY in env")
	}
	// State file lives inside CLAUDE_CONFIG_DIR when it is set. Logged-in
	// state needs oauthAccount from the real state file, not just the
	// credentials — carry over the account identity, nothing else.
	state := map[string]any{
		"hasCompletedOnboarding":        true,
		"bypassPermissionsModeAccepted": true,
	}
	if data, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		var real map[string]any
		if json.Unmarshal(data, &real) == nil {
			for _, k := range []string{"oauthAccount", "userID", "installMethod"} {
				if v, ok := real[k]; ok {
					state[k] = v
				}
			}
		}
	}
	out, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(t.Home, ".claude.json"), out, 0o644)
}

func (ccHarness) Command(t *trialCtx) (string, []string, []string) {
	cc := t.Arm.Claude
	if cc == nil {
		cc = &CCOpts{}
	}
	args := []string{"-p", "--output-format", "json", "--session-id", t.SessionID}
	mode := cc.PermissionMode
	if mode == "" {
		mode = "bypassPermissions"
	}
	args = append(args, "--permission-mode", mode)
	if mode == "bypassPermissions" {
		args = append(args, "--dangerously-skip-permissions")
	}
	if t.Arm.Model != "" {
		args = append(args, "--model", t.Arm.Model)
	}
	if cc.Effort != "" {
		args = append(args, "--effort", cc.Effort)
	}
	if cc.Bare {
		args = append(args, "--bare")
	}
	for _, d := range cc.PluginDirs {
		args = append(args, "--plugin-dir", d)
	}
	if len(cc.MCPConfigs) > 0 {
		args = append(args, "--strict-mcp-config")
		for _, c := range cc.MCPConfigs {
			args = append(args, "--mcp-config", c)
		}
	}
	if cc.Settings != "" {
		args = append(args, "--settings", cc.Settings)
	}
	if cc.SettingSources != "" {
		args = append(args, "--setting-sources", cc.SettingSources)
	}
	args = append(args, t.Arm.ExtraArgs...)
	args = append(args, t.Spec.Prompt)
	env := []string{"CLAUDE_CONFIG_DIR=" + t.Home}
	return "claude", args, env
}

// ccResult is the --output-format json result object.
type ccResult struct {
	Type         string   `json:"type"`
	Subtype      string   `json:"subtype"`
	SessionID    string   `json:"session_id"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
}

// ParseResult scans captured stdout for the result object. Decoding
// object-by-object tolerates any stray non-result output around it.
func (ccHarness) ParseResult(t *trialCtx) (string, *float64, error) {
	f, err := os.Open(t.Stdout)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var last *ccResult
	for {
		var r ccResult
		if err := dec.Decode(&r); err != nil {
			break
		}
		if r.SessionID != "" || r.Type == "result" {
			last = &r
		}
	}
	if last == nil {
		return "", nil, fmt.Errorf("no result JSON in stdout (see %s)", t.Stdout)
	}
	if last.SessionID != "" && last.SessionID != t.SessionID {
		return last.SessionID, last.TotalCostUSD,
			fmt.Errorf("session id mismatch: asked %s, got %s", t.SessionID, last.SessionID)
	}
	if last.IsError {
		return last.SessionID, last.TotalCostUSD,
			fmt.Errorf("agent reported error (%s): %s", last.Subtype, truncateStr(last.Result, 200))
	}
	return last.SessionID, last.TotalCostUSD, nil
}

func (ccHarness) TranscriptRoot(t *trialCtx) string {
	return filepath.Join(t.Home, "projects")
}

func (ccHarness) Ingest(st *store.Store, root, regime string) error {
	ing := &claudecode.Ingester{Root: root, Regime: regime}
	_, err := ing.Run(st)
	return err
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
