package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/erewhon/tokenator/internal/bench"
	"github.com/erewhon/tokenator/internal/report"
	"github.com/erewhon/tokenator/internal/store"
)

func cmdBench(ctx context.Context, args []string) error {
	if len(args) == 0 {
		benchUsage()
		return fmt.Errorf("bench needs a subcommand")
	}
	switch args[0] {
	case "run":
		return cmdBenchRun(ctx, args[1:])
	case "report":
		return cmdBenchReport(args[1:])
	case "list":
		return cmdBenchList(args[1:])
	case "ingest":
		return cmdBenchIngest(args[1:])
	case "-h", "--help", "help":
		benchUsage()
		return nil
	}
	benchUsage()
	return fmt.Errorf("unknown bench subcommand %q", args[0])
}

func benchUsage() {
	fmt.Fprint(os.Stderr, `usage: tokenator bench <subcommand> [flags]

subcommands:
  run <spec.json>   execute an experiment: arms × trials in isolated sessions
                    (--trials N, --arm NAME, --yes, --resume RUN-KEY, --runs-dir DIR)
  report [run-key]  per-arm comparison for a run (default: latest; --trials for detail)
  list              recent bench runs
  ingest <run-key>  re-ingest trial transcripts whose inline ingest failed

Each trial consumes real API/subscription usage. See docs/bench.md.
`)
}

func defaultRunsDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "bench")
}

func cmdBenchRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench run", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	runsDir := fs.String("runs-dir", "", "run directories root (default: <db dir>/bench)")
	trials := fs.Int("trials", 0, "override the spec's trials-per-arm")
	arm := fs.String("arm", "", "run only this arm")
	yes := fs.Bool("yes", false, "skip the preflight confirmation")
	resume := fs.String("resume", "", "resume an existing run by run-key prefix (spec file not needed)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if *runsDir == "" {
		*runsDir = defaultRunsDir(*dbPath)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *resume != "" {
		run, err := st.BenchRunByPrefix(*resume)
		if err != nil {
			return err
		}
		spec, err := bench.FromSnapshot(run.SpecJSON)
		if err != nil {
			return err
		}
		r := &bench.Runner{Store: st, Spec: spec, RunsDir: *runsDir,
			Yes: *yes, Arm: *arm, Trials: *trials}
		if err := r.Resume(ctx, run); err != nil {
			return err
		}
		return reportRun(st, run.RunKey, false)
	}

	if fs.NArg() != 1 {
		return fmt.Errorf("usage: tokenator bench run [flags] <spec.json>")
	}
	spec, err := bench.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	r := &bench.Runner{Store: st, Spec: spec, RunsDir: *runsDir,
		Yes: *yes, Arm: *arm, Trials: *trials}
	runKey, err := r.Run(ctx)
	if err != nil {
		return err
	}
	return reportRun(st, runKey, false)
}

func cmdBenchReport(args []string) error {
	fs := flag.NewFlagSet("bench report", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	showTrials := fs.Bool("trials", false, "include the per-trial detail table")
	// Accept the run key before or after flags (stdlib flag stops parsing
	// at the first positional argument).
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	prefix := fs.Arg(0)
	if fs.NArg() > 1 {
		if err := parseFlags(fs, fs.Args()[1:]); err != nil {
			return err
		}
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	return reportRun(st, prefix, *showTrials)
}

func reportRun(st *store.Store, prefix string, showTrials bool) error {
	run, err := st.BenchRunByPrefix(prefix)
	if err != nil {
		return err
	}
	rows, err := st.BenchReportRows(run.ID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no trials recorded for", run.RunKey)
		return nil
	}
	return report.RenderBench(os.Stdout, run, rows, showTrials)
}

func cmdBenchList(args []string) error {
	fs := flag.NewFlagSet("bench list", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	limit := fs.Int("limit", 20, "max runs")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	runs, err := st.ListBenchRuns(*limit)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("no bench runs yet — see docs/bench.md")
		return nil
	}
	return report.RenderBenchList(os.Stdout, runs)
}

func cmdBenchIngest(args []string) error {
	fs := flag.NewFlagSet("bench ingest", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: tokenator bench ingest <run-key-prefix>")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	run, err := st.BenchRunByPrefix(fs.Arg(0))
	if err != nil {
		return err
	}
	ok, failed, err := bench.IngestRun(st, run)
	if err != nil {
		return err
	}
	fmt.Printf("%s: ingested %d trial root(s), %d failed\n", run.RunKey, ok, failed)
	return nil
}
