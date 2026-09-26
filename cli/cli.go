// Package cli is the tokenator command line: flag parsing and subcommand
// wiring over the internal packages, exposed so another module (the pitf
// unified CLI) can mount it as a subcommand. cmd/tokenator is a thin
// wrapper around Run.
//
// tokenator is a token profiler for AI coding agents. Phase 1: ingest
// Claude Code transcripts into SQLite and report usage with prompt-cache
// splits. See docs/phase1-profiler.md.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/erewhon/tokenator/internal/analyze"
	"github.com/erewhon/tokenator/internal/ingest/claudecode"
	"github.com/erewhon/tokenator/internal/ingest/opencode"
	"github.com/erewhon/tokenator/internal/ingest/otel"
	"github.com/erewhon/tokenator/internal/ingest/reqlog"
	"github.com/erewhon/tokenator/internal/report"
	"github.com/erewhon/tokenator/internal/serve"
	"github.com/erewhon/tokenator/internal/store"
)

// Version is what `tokenator version` prints. cmd/tokenator sets it from
// its own main.version, which goreleaser stamps via -ldflags.
var Version = "dev"

// UsageError is returned for a bad invocation: no command, an unknown
// command, or a flag the flag package rejected. By the time Run returns it
// the message and the usage text have already been written to stderr
// (mirroring flag.ContinueOnError), so callers should exit 2 without printing
// it again.
type UsageError struct {
	Err error
}

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// Run executes the tokenator command line given the arguments after the
// program name (os.Args[1:] for the standalone binary).
//
// Return values:
//   - nil: success, including `help`/`--help`/`-h` and `version`;
//   - flag.ErrHelp: a subcommand's -h/--help (usage already printed);
//   - *UsageError: bad invocation, already reported on stderr; exit 2;
//   - anything else: a command failure, formatted "tokenator <cmd>: <err>";
//     exit 1.
//
// ctx cancels the long-running subcommands (otel, reqlog, bench run);
// those also install their own SIGINT/SIGTERM handlers, as they did when
// this code lived in package main, so the standalone binary needs no
// signal handling of its own and Ctrl-C keeps its default effect on the
// other subcommands.
func Run(ctx context.Context, args []string) error {
	log.SetFlags(0)
	if len(args) < 1 {
		usage()
		return &UsageError{Err: errors.New("no command")}
	}
	var err error
	switch args[0] {
	case "ingest":
		err = cmdIngest(args[1:])
	case "report":
		err = cmdReport(args[1:])
	case "attr":
		err = cmdAttr(args[1:])
	case "waste":
		err = cmdWaste(args[1:])
	case "cache":
		err = cmdCache(args[1:])
	case "session":
		err = cmdSession(args[1:])
	case "otel":
		err = cmdOTel(ctx, args[1:])
	case "reqlog":
		err = cmdReqlog(ctx, args[1:])
	case "serve":
		err = cmdServe(args[1:])
	case "bench":
		err = cmdBench(ctx, args[1:])
	case "doctor":
		err = cmdDoctor(args[1:])
	case "version", "-V", "--version":
		fmt.Println("tokenator", Version)
	case "-h", "--help", "help":
		usage()
	default:
		log.Printf("unknown command %q", args[0])
		usage()
		return &UsageError{Err: fmt.Errorf("unknown command %q", args[0])}
	}
	if err != nil {
		var ue *UsageError
		if errors.Is(err, flag.ErrHelp) || errors.As(err, &ue) {
			return err
		}
		return fmt.Errorf("tokenator %s: %w", args[0], err)
	}
	return nil
}

// parseFlags is fs.Parse with the flag package's ExitOnError exits turned
// into return values: -h/--help yields flag.ErrHelp and a bad flag yields a
// *UsageError, in both cases after the flag package has printed exactly
// what it would have printed before exiting.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &UsageError{Err: err}
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: tokenator <command> [flags]

commands:
  ingest    ingest harness data (Claude Code + OpenCode) into the database
  report    usage rollups with cache splits (--by project|model|session)
  attr      block-level attribution (--by tool|mcp|file|kind; --residency
            weights by est tokens × requests kept in context)
  waste     repeat reads, oversized results, long-resident heavyweights
  cache     cache doctor: invalidation events, causes, reuse scores
            (--wire: classify from router prefix hash chains)
  session   single-session view: timeline, composition, events
            (session <id-or-slug-prefix> [--html out.html])
  otel      OTLP/HTTP receiver for Claude Code telemetry (--status for summary)
  reqlog    pull gateway request rows from the LLM router's reqlog Postgres
            (--dsn or $TOKENATOR_REQLOG_DSN; --status for summary)
  serve     localhost web UI: session browser, content search, transcripts
            (--listen 127.0.0.1:8990)
  bench     A/B harness: run arms × trials in isolated sessions and compare
            (bench run <spec.json> | report | list | ingest)
  doctor    show database and source status
  version   print version

common flags:
  -db PATH  database path (default: $XDG_DATA_HOME/tokenator/tokenator.db)
`)
}

func defaultDBPath() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "tokenator", "tokenator.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "tokenator.db"
	}
	return filepath.Join(home, ".local", "share", "tokenator", "tokenator.db")
}

func cmdIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	claudeRoot := fs.String("claude-root", "", "Claude Code projects dir (default ~/.claude/projects)")
	opencodeRoot := fs.String("opencode-root", "", "OpenCode storage dir (default ~/.local/share/opencode/storage; legacy JSON tree)")
	opencodeDB := fs.String("opencode-db", "", "OpenCode database (default ~/.local/share/opencode/opencode.db; OpenCode 1.18+)")
	regime := fs.String("regime", "subscription", "billing regime for the Claude Code source: subscription|metered")
	full := fs.Bool("full", false, "re-parse all files even if unchanged (needed once after schema upgrades)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	cc := &claudecode.Ingester{Root: *claudeRoot, Regime: *regime, Full: *full}
	start := time.Now()
	ccStats, err := cc.Run(st)
	if err != nil {
		return err
	}
	log.Printf("claude_code: %s (%.1fs)", ccStats, time.Since(start).Seconds())

	oc := &opencode.Ingester{Root: *opencodeRoot, DB: *opencodeDB, Full: *full}
	start = time.Now()
	switch ocStats, err := oc.Run(st); {
	case errors.Is(err, opencode.ErrNoStorage):
		log.Print("opencode: neither storage tree nor opencode.db found, skipping")
	case err != nil:
		return err
	default:
		log.Printf("opencode: %s (%.1fs)", ocStats, time.Since(start).Seconds())
	}

	// New transcript rows may pair with already-captured gateway rows.
	if matched, _, err := st.MatchGwRequests(); err != nil {
		return err
	} else if matched > 0 {
		log.Printf("reqlog: matched %d gateway rows to new transcript requests", matched)
	}
	return nil
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	by := fs.String("by", "project", "group by: project|model|session")
	since := fs.String("since", "", "window like 7d, 24h, 30m (default: all time)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sinceTS, err := parseSince(*since)
	if err != nil {
		return err
	}
	rows, err := st.Rollup(*by, sinceTS)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		log.Print("no data — run `tokenator ingest` first")
		return nil
	}
	return report.Render(os.Stdout, *by, rows)
}

func cmdAttr(args []string) error {
	fs := flag.NewFlagSet("attr", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	by := fs.String("by", "tool", "group by: tool|mcp|file|kind")
	since := fs.String("since", "", "window like 7d, 24h (default: all time)")
	residency := fs.Bool("residency", false,
		"weight by residency (est tokens × requests kept in context) instead of entry size; --since applies per session")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sinceTS, err := parseSince(*since)
	if err != nil {
		return err
	}
	if *residency {
		rep, err := residencyReport(st, sinceTS)
		if err != nil {
			return err
		}
		if rep == nil {
			log.Print("no blocks — run `tokenator ingest` (add --full once after upgrading)")
			return nil
		}
		rows, ok := analyze.RollupResidency(rep.Items, *by)
		if !ok {
			return fmt.Errorf("unknown attr group %q (want tool, mcp, file, or kind)", *by)
		}
		return report.RenderAttrResidency(os.Stdout, *by, rows, rep)
	}
	rows, err := st.AttrRollup(*by, sinceTS, 0)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		log.Print("no blocks — run `tokenator ingest` (add --full once after upgrading)")
		return nil
	}
	cal, err := st.Calibrate(sinceTS)
	if err != nil {
		return err
	}
	return report.RenderAttr(os.Stdout, *by, rows, cal)
}

// residencyReport loads the residency inputs and runs the analyzer; nil
// report (no error) means no blocks are extracted yet.
func residencyReport(st *store.Store, sinceTS string) (*analyze.ResidencyReport, error) {
	blocks, err := st.BlocksForResidency(sinceTS)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	stamps, err := st.RequestStamps(sinceTS)
	if err != nil {
		return nil, err
	}
	comps, err := st.CompactionTimes()
	if err != nil {
		return nil, err
	}
	aBlocks := make([]analyze.ResBlock, len(blocks))
	for i, b := range blocks {
		aBlocks[i] = analyze.ResBlock{
			SessionID: b.SessionID, Project: b.Project, SessionKey: b.SessionKey,
			TS: b.TS, Kind: b.Kind, Tool: b.Tool, MCPServer: b.MCPServer,
			FilePath: b.FilePath, EstTokens: b.EstTokens, Referenced: int(b.Referenced),
		}
	}
	aStamps := make([]analyze.Stamp, len(stamps))
	for i, s := range stamps {
		aStamps[i] = analyze.Stamp{SessionID: s.SessionID, TS: s.TS, PromptTokens: s.PromptTokens}
	}
	return analyze.Residency(aBlocks, aStamps, comps), nil
}

func cmdWaste(args []string) error {
	fs := flag.NewFlagSet("waste", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	since := fs.String("since", "", "window like 7d, 24h (default: all time)")
	limit := fs.Int("limit", 15, "rows per table")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sinceTS, err := parseSince(*since)
	if err != nil {
		return err
	}
	reads, err := st.RepeatReads(sinceTS, *limit)
	if err != nil {
		return err
	}
	big, err := st.BiggestResults(sinceTS, *limit)
	if err != nil {
		return err
	}
	var heavy, stale []analyze.BlockResidency
	var staleKnown, staleUnref int64
	if rep, err := residencyReport(st, sinceTS); err != nil {
		return err
	} else if rep != nil {
		heavy = analyze.TopResidents(rep.Items, *limit)
		stale = analyze.StalePassengers(rep.Items, *limit)
		staleKnown, staleUnref = analyze.StaleShare(rep.Items)
	}
	return report.RenderWaste(os.Stdout, reads, big, heavy, stale, staleKnown, staleUnref)
}

func cmdCache(args []string) error {
	fs := flag.NewFlagSet("cache", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	since := fs.String("since", "", "window like 7d, 24h (default: all time; applied per session)")
	limit := fs.Int("limit", 10, "worst sessions to list")
	session := fs.String("session", "", "session id or slug prefix: per-request drill-down")
	wire := fs.Bool("wire", false,
		"wire mode: classify from router prefix hash chains (needs `tokenator reqlog` data) instead of inferring from usage")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sinceTS, err := parseSince(*since)
	if err != nil {
		return err
	}
	if *wire {
		return runWireCache(st, sinceTS, *session, *limit)
	}
	reqs, err := st.RequestsForCache(sinceTS, *session)
	if err != nil {
		return err
	}
	if len(reqs) == 0 {
		log.Print("no matching requests — run `tokenator ingest` first")
		return nil
	}
	comps, err := st.CompactionTimes()
	if err != nil {
		return err
	}
	aReqs := make([]analyze.Req, len(reqs))
	for i, r := range reqs {
		aReqs[i] = analyze.Req{
			SessionID: r.SessionID, Project: r.Project, SessionKey: r.SessionKey,
			Title: r.Title, TS: r.TS, Model: r.Model,
			Input: r.Input, Output: r.Output,
			CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
			Write5m: r.Write5m, Write1h: r.Write1h,
		}
	}
	rep := analyze.Cache(aReqs, comps)
	if *session != "" {
		return report.RenderCacheSession(os.Stdout, aReqs, rep)
	}
	return report.RenderCacheSummary(os.Stdout, rep, *limit)
}

// runWireCache is `tokenator cache --wire`: the chain-grounded doctor over
// gateway-matched requests.
func runWireCache(st *store.Store, sinceTS, sessionPrefix string, limit int) error {
	rows, err := st.GwChainRows(sinceTS, sessionPrefix)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		log.Print("no matched gateway requests — run `tokenator reqlog` then `tokenator ingest` first")
		return nil
	}
	comps, err := st.CompactionTimes()
	if err != nil {
		return err
	}
	reqs := make([]analyze.WireReq, len(rows))
	for i, r := range rows {
		reqs[i] = analyze.WireReq{
			SessionID: r.SessionID, Project: r.Project, SessionKey: r.SessionKey,
			Title: r.Title, TS: r.TS, Model: r.Model, Chain: r.Chain,
			Input: r.Input, Output: r.Output,
			CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
			Write5m: r.Write5m, Write1h: r.Write1h,
		}
	}
	rep := analyze.Wire(reqs, comps)
	if sessionPrefix != "" {
		return report.RenderWireSession(os.Stdout, rep)
	}
	return report.RenderWireSummary(os.Stdout, rep, limit)
}

func cmdSession(args []string) error {
	fs := flag.NewFlagSet("session", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	htmlOut := fs.String("html", "", "write a self-contained HTML page to this path instead of the terminal view")
	// Accept the session prefix before or after flags (stdlib flag stops
	// parsing at the first positional argument).
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	prefix := fs.Arg(0)
	if fs.NArg() > 1 {
		if err := parseFlags(fs, fs.Args()[1:]); err != nil {
			return err
		}
	}
	if prefix == "" {
		return fmt.Errorf("usage: tokenator session [flags] <session-id-or-slug-prefix>")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	view, err := report.BuildSessionView(st, prefix)
	if err != nil {
		return err
	}
	if *htmlOut != "" {
		f, err := os.Create(*htmlOut)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := report.RenderSessionHTML(f, view); err != nil {
			return err
		}
		log.Printf("wrote %s", *htmlOut)
		return nil
	}
	return report.RenderSessionTerm(os.Stdout, view)
}

func cmdOTel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("otel", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	listen := fs.String("listen", otel.DefaultAddr, "OTLP/HTTP listen address")
	regime := fs.String("regime", "subscription", "billing regime for the otel source: subscription|metered")
	status := fs.Bool("status", false, "print captured-data summary and cross-check, then exit")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if *status {
		s, err := st.OTelStatus()
		if err != nil {
			return err
		}
		return report.RenderOTelStatus(os.Stdout, s)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	rc := &otel.Receiver{Addr: *listen, Regime: *regime, Store: st}
	log.Printf("OTLP/HTTP JSON receiver on http://%s (POST /v1/metrics, /v1/logs; traces accepted and dropped)", *listen)
	log.Printf(`point Claude Code here — e.g. in ~/.claude/settings.json "env", or exported in your shell:
  CLAUDE_CODE_ENABLE_TELEMETRY=1
  OTEL_METRICS_EXPORTER=otlp
  OTEL_LOGS_EXPORTER=otlp
  OTEL_EXPORTER_OTLP_PROTOCOL=http/json
  OTEL_EXPORTER_OTLP_ENDPOINT=http://%s
(Ctrl-C to stop; `+"`tokenator otel --status`"+` for a capture summary)`, *listen)
	return rc.Run(ctx)
}

func cmdReqlog(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reqlog", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	dsn := fs.String("dsn", os.Getenv("TOKENATOR_REQLOG_DSN"),
		"reqlog Postgres DSN, URI form (default $TOKENATOR_REQLOG_DSN)")
	regime := fs.String("regime", "subscription",
		"billing regime for this source: subscription|metered|local|unknown")
	status := fs.Bool("status", false, "print captured-data summary, then exit")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if *status {
		gs, err := st.GwStatus()
		if err != nil {
			return err
		}
		return report.RenderGwStatus(os.Stdout, gs)
	}

	if *dsn == "" {
		return fmt.Errorf(`no DSN: set --dsn or TOKENATOR_REQLOG_DSN, e.g.
  postgres://router:$(ho secret get llm-router/reqlog-pg-password)@euclid.m.bcc.sh:5433/router`)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	src, err := reqlog.OpenPG(ctx, *dsn)
	if err != nil {
		return err
	}
	defer src.Close()

	ing := &reqlog.Ingester{Src: src, Regime: *regime}
	start := time.Now()
	stats, err := ing.Run(ctx, st)
	if err != nil {
		return err
	}
	log.Printf("reqlog %s: %s (%.1fs)", src.Root(), stats, time.Since(start).Seconds())
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	listen := fs.String("listen", "127.0.0.1:8990", "listen address (keep it loopback: no auth)")
	scan := fs.Int("scan-limit", 80, "max sessions a content search scans (newest first)")
	monitorURL := fs.String("monitor-url", os.Getenv("TOKENATOR_MONITOR_URL"), "agent-monitor board URL to link from session pages (env: TOKENATOR_MONITOR_URL)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	srv := &serve.Server{Store: st, ScanLimit: *scan, MonitorURL: *monitorURL}
	return srv.ListenAndServe(*listen)
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ver, err := st.SchemaVersion()
	if err != nil {
		return err
	}
	counts, err := st.Counts()
	if err != nil {
		return err
	}
	fmt.Printf("db:          %s (schema v%d)\n", st.Path(), ver)
	fmt.Printf("sources:     %d\n", counts.Sources)
	fmt.Printf("sessions:    %d\n", counts.Sessions)
	fmt.Printf("requests:    %d\n", counts.Requests)
	fmt.Printf("compactions: %d\n", counts.Compactions)
	fmt.Printf("files seen:  %d\n", counts.Files)
	if counts.OTelDatapoints > 0 || counts.OTelEvents > 0 {
		fmt.Printf("otel:        %d datapoints, %d events\n", counts.OTelDatapoints, counts.OTelEvents)
	}
	if counts.GwRequests > 0 {
		fmt.Printf("gateway:     %d reqlog rows\n", counts.GwRequests)
	}

	home, err := os.UserHomeDir()
	if err == nil {
		ccRoot := filepath.Join(home, ".claude", "projects")
		files, _ := filepath.Glob(filepath.Join(ccRoot, "*", "*.jsonl"))
		fmt.Printf("claude_code: %s (%d transcript files)\n", ccRoot, len(files))
		ocRoot := filepath.Join(home, ".local", "share", "opencode", "storage")
		if msgs, _ := filepath.Glob(filepath.Join(ocRoot, "message", "*", "*.json")); len(msgs) > 0 {
			fmt.Printf("opencode:    %s (%d message files)\n", ocRoot, len(msgs))
		}
	}
	return nil
}

// parseSince turns "7d", "24h", "30m" into an RFC3339 UTC lower bound.
// Empty or "all" means no bound.
func parseSince(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "all" {
		return "", nil
	}
	var d time.Duration
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return "", fmt.Errorf("bad --since %q", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return "", fmt.Errorf("bad --since %q", s)
		}
	}
	return time.Now().UTC().Add(-d).Format(time.RFC3339), nil
}
