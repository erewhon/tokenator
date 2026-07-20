package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/erewhon/tokenator/internal/ingest/opencode"
	"github.com/erewhon/tokenator/internal/store"
)

// ocHarness runs OpenCode headlessly with storage isolated via
// XDG_DATA_HOME (storage lands at <home>/opencode/storage). OpenCode picks
// its own session id; it is parsed from the --format json event stream.
type ocHarness struct{}

func (ocHarness) Kind() string { return HarnessOpenCode }

// Seed copies provider credentials (auth.json) into the isolated data dir —
// a fresh XDG_DATA_HOME would otherwise have no logged-in providers.
func (ocHarness) Seed(t *trialCtx) error {
	if err := os.MkdirAll(filepath.Join(t.Home, "opencode"), 0o755); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	_, err = copyFile(filepath.Join(home, ".local", "share", "opencode", "auth.json"),
		filepath.Join(t.Home, "opencode", "auth.json"), 0o600)
	return err
}

func (ocHarness) Command(t *trialCtx) (string, []string, []string) {
	oc := t.Arm.OpenCode
	if oc == nil {
		oc = &OCOpts{}
	}
	args := []string{"run", "--format", "json", "--auto"}
	if t.Arm.Model != "" {
		args = append(args, "-m", t.Arm.Model)
	}
	if oc.Pure {
		args = append(args, "--pure")
	}
	if oc.Agent != "" {
		args = append(args, "--agent", oc.Agent)
	}
	if oc.Variant != "" {
		args = append(args, "--variant", oc.Variant)
	}
	args = append(args, t.Arm.ExtraArgs...)
	args = append(args, t.Spec.Prompt)
	env := []string{"XDG_DATA_HOME=" + t.Home}
	if oc.ConfigFile != "" {
		env = append(env, "OPENCODE_CONFIG="+oc.ConfigFile)
	}
	return "opencode", args, env
}

// ParseResult decodes the JSON event stream and deep-searches each event for
// a session id (key sessionID, value ses_*). OpenCode reports no aggregate
// cost in the stream we rely on; cost comes from ingested request rows.
func (ocHarness) ParseResult(t *trialCtx) (string, *float64, error) {
	f, err := os.Open(t.Stdout)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			break
		}
		if id := findSessionID(v, 0); id != "" {
			return id, nil, nil
		}
	}
	return "", nil, fmt.Errorf("no sessionID found in event stream (see %s)", t.Stdout)
}

// findSessionID walks decoded JSON looking for a ses_* string under a
// sessionID-ish key, bounded to a few nesting levels.
func findSessionID(v any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			lk := strings.ToLower(k)
			if lk == "sessionid" || lk == "session_id" || (lk == "id" && depth == 0) {
				if s, ok := val.(string); ok && strings.HasPrefix(s, "ses_") {
					return s
				}
			}
		}
		for _, val := range x {
			if id := findSessionID(val, depth+1); id != "" {
				return id
			}
		}
	case []any:
		for _, val := range x {
			if id := findSessionID(val, depth+1); id != "" {
				return id
			}
		}
	}
	return ""
}

func (ocHarness) TranscriptRoot(t *trialCtx) string {
	return filepath.Join(t.Home, "opencode", "storage")
}

func (ocHarness) Ingest(st *store.Store, root, regime string) error {
	ing := &opencode.Ingester{Root: root, Regime: regime}
	_, err := ing.Run(st)
	return err
}
