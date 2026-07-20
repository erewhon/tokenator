package bench

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/erewhon/tokenator/internal/store"
)

// trialCtx carries everything a harness needs to run one trial.
type trialCtx struct {
	Spec      *Spec
	Arm       *Arm
	Workspace string // fresh per-trial copy of the template
	Home      string // per-trial isolation dir (CLAUDE_CONFIG_DIR / XDG_DATA_HOME)
	SessionID string // claude_code: pre-assigned before exec; opencode: parsed after
	Stdout    string // captured stdout path
	Stderr    string // captured stderr path

	durationMS int64 // wall time of the harness process, set by the runner
}

// harness abstracts the two runnable agents.
type harness interface {
	Kind() string
	// Seed prepares the isolation dir (credentials, onboarding state).
	Seed(t *trialCtx) error
	// Command returns argv[0], args, and extra env for the trial process.
	Command(t *trialCtx) (name string, args []string, env []string)
	// ParseResult extracts the session id and harness-reported cost from
	// the captured stdout after the process exits.
	ParseResult(t *trialCtx) (sessionID string, costUSD *float64, err error)
	// TranscriptRoot is the ingest root inside the isolation dir.
	TranscriptRoot(t *trialCtx) string
	// Ingest pulls the trial's transcripts into the database.
	Ingest(st *store.Store, root, regime string) error
}

func harnessFor(kind string) (harness, error) {
	switch kind {
	case HarnessClaudeCode:
		return ccHarness{}, nil
	case HarnessOpenCode:
		return ocHarness{}, nil
	}
	return nil, fmt.Errorf("unknown harness %q", kind)
}

// copyFile copies src to dst with the given mode; a missing src is not an
// error (the caller decides whether the artifact is required).
func copyFile(src, dst string, mode os.FileMode) (copied bool, err error) {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return false, err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return false, err
	}
	return true, out.Close()
}
