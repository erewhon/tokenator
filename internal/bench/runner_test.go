package bench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erewhon/tokenator/internal/store"
)

// fakeClaude is a stand-in claude binary: it extracts --session-id, writes a
// plausible transcript under $CLAUDE_CONFIG_DIR/projects/, creates result.txt
// in the cwd (so checks have something to verify), and prints a result JSON.
const fakeClaude = `#!/bin/sh
sid=""; prev=""
for a in "$@"; do
  [ "$prev" = "--session-id" ] && sid="$a"
  prev="$a"
done
mkdir -p "$CLAUDE_CONFIG_DIR/projects/-bench"
cat > "$CLAUDE_CONFIG_DIR/projects/-bench/$sid.jsonl" <<EOF
{"type":"user","sessionId":"$sid","timestamp":"2026-07-19T10:00:00Z","cwd":"$PWD","message":{"role":"user","content":"task"}}
{"type":"assistant","sessionId":"$sid","timestamp":"2026-07-19T10:00:05Z","requestId":"req_$sid","message":{"id":"msg_$sid","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":100,"cache_creation_input_tokens":5},"content":[{"type":"text","text":"did it"}]}}
EOF
echo done > result.txt
echo "{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"$sid\",\"total_cost_usd\":0.0123,\"is_error\":false,\"result\":\"ok\"}"
`

// fakeOpenCode writes OpenCode storage under $XDG_DATA_HOME/opencode/storage
// and prints an event stream carrying the session id.
const fakeOpenCode = `#!/bin/sh
root="$XDG_DATA_HOME/opencode/storage"
ses="ses_bench$$"
msg="msg_$$"
mkdir -p "$root/session/proj" "$root/message/$ses" "$root/part/$msg"
cat > "$root/session/proj/$ses.json" <<EOF
{"id":"$ses","slug":"bench-run","directory":"$PWD","title":"bench task","time":{"created":1752900000000,"updated":1752900100000}}
EOF
cat > "$root/message/$ses/$msg.json" <<EOF
{"id":"$msg","sessionID":"$ses","role":"assistant","modelID":"claude-sonnet-5","providerID":"anthropic","finish":"stop","time":{"created":1752900010000,"completed":1752900020000},"tokens":{"total":60,"input":40,"output":20,"reasoning":0,"cache":{"read":200,"write":10}}}
EOF
cat > "$root/part/$msg/prt_1.json" <<EOF
{"id":"prt_1","sessionID":"$ses","messageID":"$msg","type":"text","text":"done"}
EOF
echo done > result.txt
echo "{\"type\":\"start\",\"properties\":{\"info\":{\"id\":\"$msg\",\"sessionID\":\"$ses\"}}}"
echo "{\"type\":\"finish\"}"
`

const fakeSleeper = `#!/bin/sh
sleep 30
`

// benchEnv builds a temp world: fake binaries on PATH, HOME with harness
// credentials, a workspace template, a store, and a resolved spec.
type benchEnv struct {
	st      *store.Store
	spec    *Spec
	runsDir string
}

func setup(t *testing.T, claudeScript string, trials int) *benchEnv {
	t.Helper()
	dir := t.TempDir()

	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	for name, body := range map[string]string{"claude": claudeScript, "opencode": fakeOpenCode} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	home := filepath.Join(dir, "home")
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"tok":"x"}`), 0o600)
	os.MkdirAll(filepath.Join(home, ".local", "share", "opencode"), 0o755)
	os.WriteFile(filepath.Join(home, ".local", "share", "opencode", "auth.json"), []byte(`{}`), 0o600)
	t.Setenv("HOME", home)

	ws := filepath.Join(dir, "template")
	os.MkdirAll(ws, 0o755)
	os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("template file"), 0o644)

	st, err := store.Open(filepath.Join(dir, "bench.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	spec := &Spec{
		Name: "t", Prompt: "do it", Workspace: ws,
		Trials: trials, TimeoutSeconds: 5, CheckTimeoutSeconds: 5,
		Checks: []Check{{Name: "made-file", Cmd: "test -f result.txt"}},
		Arms: []Arm{
			{Name: "cc", Harness: HarnessClaudeCode, Model: "sonnet"},
			{Name: "oc", Harness: HarnessOpenCode, Model: "anthropic/claude-sonnet-5"},
		},
	}
	if err := spec.resolve(dir); err != nil {
		t.Fatal(err)
	}
	return &benchEnv{st: st, spec: spec, runsDir: filepath.Join(dir, "runs")}
}

func TestRunnerEndToEnd(t *testing.T) {
	env := setup(t, fakeClaude, 2)
	r := &Runner{Store: env.st, Spec: env.spec, RunsDir: env.runsDir, Yes: true, Out: testWriter{t}}
	runKey, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, err := env.st.BenchRunByPrefix(runKey)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := env.st.BenchReportRows(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 { // 2 arms × 2 trials
		t.Fatalf("got %d trial rows, want 4: %+v", len(rows), rows)
	}
	for _, u := range rows {
		if u.Status != store.TrialOK {
			t.Errorf("%s/%d status = %s (want ok)", u.Arm, u.TrialIdx, u.Status)
		}
		if !u.HasUsage || u.Requests == 0 {
			t.Errorf("%s/%d has no joined usage (ingest broken?)", u.Arm, u.TrialIdx)
		}
		switch u.Arm {
		case "cc":
			if u.Input != 10 || u.Output != 20 || u.CacheRead != 100 || u.CacheWrite != 5 {
				t.Errorf("cc usage = %+v", u)
			}
			if u.CostUSD == nil || *u.CostUSD != 0.0123 {
				t.Errorf("cc cost = %v", u.CostUSD)
			}
			if u.HarnessSessionID == "" {
				t.Error("cc missing session id")
			}
		case "oc":
			if u.Input != 40 || u.Output != 20 {
				t.Errorf("oc usage = %+v", u)
			}
			if !strings.HasPrefix(u.HarnessSessionID, "ses_bench") {
				t.Errorf("oc session id = %q", u.HarnessSessionID)
			}
		}
		var checks []CheckResult
		json.Unmarshal([]byte(u.ChecksJSON), &checks)
		if len(checks) != 1 || !checks[0].OK {
			t.Errorf("%s/%d checks = %s", u.Arm, u.TrialIdx, u.ChecksJSON)
		}
	}
	// Workspace copies are independent per trial and got the template file.
	seed := filepath.Join(env.runsDir, runKey, "arms", "cc", "t1", "workspace", "seed.txt")
	if _, err := os.Stat(seed); err != nil {
		t.Errorf("workspace template not copied: %v", err)
	}
}

func TestRunnerCheckFail(t *testing.T) {
	env := setup(t, fakeClaude, 1)
	env.spec.Arms = env.spec.Arms[:1] // cc only
	env.spec.Checks = []Check{{Name: "impossible", Cmd: "test -f does-not-exist"}}
	r := &Runner{Store: env.st, Spec: env.spec, RunsDir: env.runsDir, Yes: true, Out: testWriter{t}}
	runKey, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, _ := env.st.BenchRunByPrefix(runKey)
	rows, _ := env.st.BenchReportRows(run.ID)
	if len(rows) != 1 || rows[0].Status != store.TrialFail {
		t.Fatalf("rows = %+v, want single fail", rows)
	}
}

func TestRunnerTimeoutAndResume(t *testing.T) {
	env := setup(t, fakeSleeper, 1)
	env.spec.Arms = env.spec.Arms[:1] // cc only
	env.spec.TimeoutSeconds = 1
	r := &Runner{Store: env.st, Spec: env.spec, RunsDir: env.runsDir, Yes: true, Out: testWriter{t}}
	runKey, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, _ := env.st.BenchRunByPrefix(runKey)
	trials, _ := env.st.BenchTrials(run.ID)
	if len(trials) != 1 || trials[0].Status != store.TrialTimeout {
		t.Fatalf("trials = %+v, want single timeout", trials)
	}
	if trials[0].DurationMS < 900 || trials[0].DurationMS > 4000 {
		t.Errorf("timeout duration = %dms, want ~1000", trials[0].DurationMS)
	}

	// Fix the binary, resume: the timed-out trial reruns and completes.
	bin := filepath.Join(filepath.Dir(env.runsDir), "bin")
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	env.spec.TimeoutSeconds = 5
	if err := (&Runner{Store: env.st, Spec: env.spec, RunsDir: env.runsDir, Yes: true, Out: testWriter{t}}).Resume(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	trials, _ = env.st.BenchTrials(run.ID)
	if len(trials) != 1 || trials[0].Status != store.TrialOK {
		t.Fatalf("after resume: %+v, want ok", trials)
	}

	// Resuming again finds nothing to do (ok trials never rerun).
	var buf strings.Builder
	if err := (&Runner{Store: env.st, Spec: env.spec, RunsDir: env.runsDir, Yes: true, Out: &buf}).Resume(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nothing to resume") {
		t.Errorf("second resume output: %s", buf.String())
	}
}

func TestCCCommandAssembly(t *testing.T) {
	spec := &Spec{Prompt: "the task"}
	arm := &Arm{Name: "a", Harness: HarnessClaudeCode, Model: "opus",
		Claude: &CCOpts{Bare: true, PluginDirs: []string{"/p1", "/p2"},
			MCPConfigs: []string{"/m.json"}, Settings: `{"x":1}`},
		ExtraArgs: []string{"--verbose"}}
	tc := &trialCtx{Spec: spec, Arm: arm, Home: "/h", SessionID: "uuid-1"}
	name, args, env := ccHarness{}.Command(tc)
	joined := strings.Join(args, " ")
	if name != "claude" {
		t.Errorf("name = %s", name)
	}
	for _, want := range []string{
		"-p", "--output-format json", "--session-id uuid-1",
		"--permission-mode bypassPermissions", "--dangerously-skip-permissions",
		"--model opus", "--bare",
		"--plugin-dir /p1", "--plugin-dir /p2",
		"--strict-mcp-config", "--mcp-config /m.json",
		`--settings {"x":1}`, "--verbose",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	if args[len(args)-1] != "the task" {
		t.Errorf("prompt not last arg: %v", args)
	}
	if env[0] != "CLAUDE_CONFIG_DIR=/h" {
		t.Errorf("env = %v", env)
	}
}

func TestOCSessionIDExtraction(t *testing.T) {
	dir := t.TempDir()
	stdout := filepath.Join(dir, "out.log")
	os.WriteFile(stdout, []byte(
		`{"type":"noise","data":[1,2]}
{"type":"message.updated","properties":{"info":{"id":"msg_1","sessionID":"ses_abc123","role":"assistant"}}}
{"type":"finish"}`), 0o644)
	sid, cost, err := ocHarness{}.ParseResult(&trialCtx{Stdout: stdout})
	if err != nil || sid != "ses_abc123" || cost != nil {
		t.Fatalf("sid=%q cost=%v err=%v", sid, cost, err)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
