package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/erewhon/tokenator/internal/store"
)

// Runner executes one bench run: every arm × trial sequentially (duration
// measurements stay honest on a quiet machine), each trial in a fresh
// workspace copy and isolated harness home, transcripts ingested as soon as
// the trial ends so a crash loses nothing.
type Runner struct {
	Store   *store.Store
	Spec    *Spec
	RunsDir string // run dirs live at <RunsDir>/<run-key>/
	Yes     bool   // skip the preflight confirmation
	Arm     string // run only this arm ('' = all)
	Trials  int    // override spec.Trials (0 = spec value)
	Out     io.Writer
	In      io.Reader // confirmation input (default os.Stdin)

	ingestMu sync.Mutex
}

// CheckResult is one check command's outcome, stored in checks_json.
type CheckResult struct {
	Name       string `json:"name"`
	Cmd        string `json:"cmd"`
	Exit       int    `json:"exit"`
	OK         bool   `json:"ok"`
	DurationMS int64  `json:"duration_ms"`
}

func (r *Runner) out() io.Writer {
	if r.Out == nil {
		return os.Stdout
	}
	return r.Out
}

func (r *Runner) trials() int {
	if r.Trials > 0 {
		return r.Trials
	}
	return r.Spec.Trials
}

// Run starts a new bench run and executes it to completion.
func (r *Runner) Run(ctx context.Context) (runKey string, err error) {
	runKey = time.Now().Format("20060102-1504") + "-" + r.Spec.Name
	runDir := filepath.Join(r.RunsDir, runKey)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", err
	}
	snap, err := r.Spec.Snapshot()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(runDir, "spec.json"), []byte(snap), 0o644); err != nil {
		return "", err
	}
	if !r.Yes {
		if ok := r.preflight(); !ok {
			return "", fmt.Errorf("aborted")
		}
	}
	runID, err := r.Store.InsertBenchRun(runKey, r.Spec.Name, snap, runDir, nowTS())
	if err != nil {
		return "", err
	}
	err = r.execute(ctx, runID, runDir, r.workList(nil))
	if ferr := r.Store.FinishBenchRun(runID, nowTS()); err == nil {
		err = ferr
	}
	return runKey, err
}

// Resume re-runs the incomplete trials of an existing run (pending/missing,
// stale running, error, timeout). Completed observations (ok/fail) are kept.
func (r *Runner) Resume(ctx context.Context, run store.BenchRun) error {
	done, err := r.Store.BenchTrials(run.ID)
	if err != nil {
		return err
	}
	skip := map[string]bool{}
	for _, t := range done {
		if t.Status == store.TrialOK || t.Status == store.TrialFail {
			skip[fmt.Sprintf("%s/%d", t.Arm, t.TrialIdx)] = true
		}
	}
	work := r.workList(skip)
	if len(work) == 0 {
		fmt.Fprintln(r.out(), "nothing to resume — all trials completed")
		return nil
	}
	fmt.Fprintf(r.out(), "resuming %s: %d trial(s)\n", run.RunKey, len(work))
	err = r.execute(ctx, run.ID, run.RunDir, work)
	if ferr := r.Store.FinishBenchRun(run.ID, nowTS()); err == nil {
		err = ferr
	}
	return err
}

type workItem struct {
	arm *Arm
	idx int
}

func (r *Runner) workList(skip map[string]bool) []workItem {
	var out []workItem
	for i := range r.Spec.Arms {
		arm := &r.Spec.Arms[i]
		if r.Arm != "" && arm.Name != r.Arm {
			continue
		}
		for idx := 1; idx <= r.trials(); idx++ {
			if skip[fmt.Sprintf("%s/%d", arm.Name, idx)] {
				continue
			}
			out = append(out, workItem{arm: arm, idx: idx})
		}
	}
	return out
}

func (r *Runner) execute(ctx context.Context, runID int64, runDir string, work []workItem) error {
	for i, w := range work {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(r.out(), "[%d/%d] %s trial %d (%s)... ",
			i+1, len(work), w.arm.Name, w.idx, w.arm.Harness)
		t, err := r.runTrial(ctx, runID, runDir, w.arm, w.idx)
		if err != nil {
			return fmt.Errorf("%s/%d: %w", w.arm.Name, w.idx, err)
		}
		fmt.Fprintf(r.out(), "%s (%.0fs)%s\n", t.Status,
			float64(t.DurationMS)/1000, noteSuffix(t.Notes))
	}
	return nil
}

func noteSuffix(n string) string {
	if n == "" {
		return ""
	}
	return " — " + n
}

// runTrial executes one trial start to finish; errors returned here are
// infrastructure failures (DB writes, workspace copy) — agent/check failures
// land in the trial row's status instead.
func (r *Runner) runTrial(ctx context.Context, runID int64, runDir string, arm *Arm, idx int) (store.BenchTrial, error) {
	h, err := harnessFor(arm.Harness)
	if err != nil {
		return store.BenchTrial{}, err
	}
	trialDir := filepath.Join(runDir, "arms", arm.Name, fmt.Sprintf("t%d", idx))
	if err := os.RemoveAll(trialDir); err != nil {
		return store.BenchTrial{}, err
	}
	tc := &trialCtx{
		Spec:      r.Spec,
		Arm:       arm,
		Workspace: filepath.Join(trialDir, "workspace"),
		Home:      filepath.Join(trialDir, "home"),
		Stdout:    filepath.Join(trialDir, "stdout.log"),
		Stderr:    filepath.Join(trialDir, "stderr.log"),
	}
	if arm.Harness == HarnessClaudeCode {
		tc.SessionID = newUUID()
	}
	row := store.BenchTrial{
		RunID: runID, Arm: arm.Name, TrialIdx: idx,
		Harness: arm.Harness, Model: arm.Model,
		HarnessSessionID: tc.SessionID,
		Status:           store.TrialRunning,
		StartedAt:        nowTS(),
		Workspace:        tc.Workspace,
		TranscriptRoot:   h.TranscriptRoot(tc),
	}
	if err := r.Store.UpsertBenchTrial(row); err != nil {
		return row, err
	}

	if err := os.MkdirAll(tc.Workspace, 0o755); err != nil {
		return row, err
	}
	if err := os.CopyFS(tc.Workspace, os.DirFS(r.Spec.Workspace)); err != nil {
		return row, fmt.Errorf("copy workspace: %w", err)
	}
	if err := h.Seed(tc); err != nil {
		return row, fmt.Errorf("seed %s home: %w", arm.Harness, err)
	}

	status, notes, cost := r.execTrial(ctx, h, tc)
	row.HarnessSessionID = tc.SessionID
	row.CostUSD = cost
	row.Status = status
	row.Notes = notes

	if status == "" { // agent completed cleanly → run checks
		results := r.runChecks(tc, trialDir)
		cj, _ := json.Marshal(results)
		row.ChecksJSON = string(cj)
		row.Status = store.TrialOK
		for _, c := range results {
			if !c.OK {
				row.Status = store.TrialFail
				break
			}
		}
	}
	row.DurationMS = tc.durationMS
	row.FinishedAt = nowTS()

	// Ingest immediately regardless of status — even failed trials produced
	// transcripts worth having. Serialized: the ingesters aren't concurrent.
	r.ingestMu.Lock()
	ingErr := h.Ingest(r.Store, row.TranscriptRoot, r.Spec.Regime)
	r.ingestMu.Unlock()
	if ingErr != nil {
		row.Notes = strings.TrimPrefix(row.Notes+"; ingest: "+ingErr.Error(), "; ")
	} else {
		row.Ingested = true
	}
	return row, r.Store.UpsertBenchTrial(row)
}

// execTrial runs the harness process. Empty status means "completed, run
// the checks"; otherwise it is the trial's final status.
func (r *Runner) execTrial(ctx context.Context, h harness, tc *trialCtx) (status, notes string, cost *float64) {
	tctx, cancel := context.WithTimeout(ctx, time.Duration(r.Spec.TimeoutSeconds)*time.Second)
	defer cancel()

	name, args, henv := h.Command(tc)
	cmd := exec.CommandContext(tctx, name, args...)
	cmd.Dir = tc.Workspace
	// Later entries win on duplicates; harness isolation env goes last so
	// nothing can accidentally break the per-trial home.
	env := os.Environ()
	for k, v := range tc.Spec.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range tc.Arm.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = append(env, henv...)

	outF, err := os.Create(tc.Stdout)
	if err != nil {
		return store.TrialError, err.Error(), nil
	}
	defer outF.Close()
	errF, err := os.Create(tc.Stderr)
	if err != nil {
		return store.TrialError, err.Error(), nil
	}
	defer errF.Close()
	cmd.Stdout, cmd.Stderr = outF, errF

	// Kill the whole process group on timeout — harnesses spawn children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 10 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	tc.durationMS = time.Since(start).Milliseconds()

	if tctx.Err() == context.DeadlineExceeded {
		return store.TrialTimeout, fmt.Sprintf("killed after %ds", r.Spec.TimeoutSeconds), nil
	}
	sid, cost, perr := h.ParseResult(tc)
	if sid != "" {
		tc.SessionID = sid
	}
	if runErr != nil {
		return store.TrialError, "harness: " + runErr.Error(), cost
	}
	if perr != nil {
		return store.TrialError, perr.Error(), cost
	}
	return "", "", cost
}

// runChecks executes the spec's checks in the trial workspace, appending
// combined output to checks.log.
func (r *Runner) runChecks(tc *trialCtx, trialDir string) []CheckResult {
	logF, _ := os.Create(filepath.Join(trialDir, "checks.log"))
	if logF != nil {
		defer logF.Close()
	}
	var out []CheckResult
	for _, c := range tc.Spec.Checks {
		cctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(tc.Spec.CheckTimeoutSeconds)*time.Second)
		cmd := exec.CommandContext(cctx, "sh", "-c", c.Cmd)
		cmd.Dir = tc.Workspace
		if logF != nil {
			fmt.Fprintf(logF, "=== %s: %s\n", c.Name, c.Cmd)
			cmd.Stdout, cmd.Stderr = logF, logF
		}
		start := time.Now()
		err := cmd.Run()
		cancel()
		cr := CheckResult{Name: c.Name, Cmd: c.Cmd, OK: err == nil,
			DurationMS: time.Since(start).Milliseconds()}
		if ee, ok := err.(*exec.ExitError); ok {
			cr.Exit = ee.ExitCode()
		} else if err != nil {
			cr.Exit = -1
		}
		if logF != nil {
			fmt.Fprintf(logF, "=== %s: exit %d\n", c.Name, cr.Exit)
		}
		out = append(out, cr)
	}
	return out
}

// preflight prints what is about to run and asks for confirmation.
func (r *Runner) preflight() bool {
	w := r.out()
	fmt.Fprintf(w, "bench run: %s\n", r.Spec.Name)
	fmt.Fprintf(w, "  prompt:    %s\n", firstLine(r.Spec.Prompt))
	fmt.Fprintf(w, "  workspace: %s\n", r.Spec.Workspace)
	fmt.Fprintf(w, "  timeout:   %ds/trial, %d checks\n", r.Spec.TimeoutSeconds, len(r.Spec.Checks))
	n := 0
	for i := range r.Spec.Arms {
		arm := &r.Spec.Arms[i]
		if r.Arm != "" && arm.Name != r.Arm {
			continue
		}
		fmt.Fprintf(w, "  arm %-16s %s model=%s%s\n", arm.Name, arm.Harness,
			orDefault(arm.Model, "(default)"), armFlags(arm))
		n++
	}
	total := n * r.trials()
	fmt.Fprintf(w, "→ %d arms × %d trials = %d agent runs consuming real API/subscription usage.\n",
		n, r.trials(), total)
	fmt.Fprint(w, "proceed? [y/N] ")
	in := r.In
	if in == nil {
		in = os.Stdin
	}
	line, _ := bufio.NewReader(in).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

func armFlags(a *Arm) string {
	var f []string
	if a.Claude != nil {
		if a.Claude.Bare {
			f = append(f, "bare")
		}
		if len(a.Claude.PluginDirs) > 0 {
			f = append(f, fmt.Sprintf("plugins=%d", len(a.Claude.PluginDirs)))
		}
		if len(a.Claude.MCPConfigs) > 0 {
			f = append(f, "strict-mcp")
		}
	}
	if a.OpenCode != nil && a.OpenCode.Pure {
		f = append(f, "pure")
	}
	if len(f) == 0 {
		return ""
	}
	return " [" + strings.Join(f, ",") + "]"
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return truncateStr(s, 100)
}

func nowTS() string { return time.Now().UTC().Format(time.RFC3339) }

// IngestRun re-ingests every trial's transcript root (idempotent — dedupe
// keys are global). For trials whose earlier in-line ingest failed.
func IngestRun(st *store.Store, run store.BenchRun) (ok, failed int, err error) {
	trials, err := st.BenchTrials(run.ID)
	if err != nil {
		return 0, 0, err
	}
	spec, err := FromSnapshot(run.SpecJSON)
	if err != nil {
		return 0, 0, err
	}
	for _, t := range trials {
		if t.TranscriptRoot == "" {
			continue
		}
		h, herr := harnessFor(t.Harness)
		if herr != nil {
			return ok, failed, herr
		}
		if ierr := h.Ingest(st, t.TranscriptRoot, spec.Regime); ierr != nil {
			failed++
			continue
		}
		ok++
		if merr := st.MarkBenchTrialIngested(t.RunID, t.Arm, t.TrialIdx, true); merr != nil {
			return ok, failed, merr
		}
	}
	return ok, failed, nil
}
