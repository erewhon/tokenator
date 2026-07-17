// Command tokenator is a token profiler for AI coding agents.
// Phase 1: ingest Claude Code transcripts into SQLite and report usage
// with prompt-cache splits. See docs/phase1-profiler.md.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/erewhon/tokenator/internal/analyze"
	"github.com/erewhon/tokenator/internal/ingest/claudecode"
	"github.com/erewhon/tokenator/internal/ingest/opencode"
	"github.com/erewhon/tokenator/internal/report"
	"github.com/erewhon/tokenator/internal/store"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "ingest":
		err = cmdIngest(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "attr":
		err = cmdAttr(os.Args[2:])
	case "waste":
		err = cmdWaste(os.Args[2:])
	case "cache":
		err = cmdCache(os.Args[2:])
	case "session":
		err = cmdSession(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		log.Printf("unknown command %q", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("tokenator %s: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: tokenator <command> [flags]

commands:
  ingest    ingest harness data (Claude Code + OpenCode) into the database
  report    usage rollups with cache splits (--by project|model|session)
  attr      block-level attribution (--by tool|mcp|file|kind)
  waste     repeat reads and oversized tool results
  cache     cache doctor: invalidation events, causes, reuse scores
  session   single-session view: timeline, composition, events
            (session <id-or-slug-prefix> [--html out.html])
  doctor    show database and source status

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
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	claudeRoot := fs.String("claude-root", "", "Claude Code projects dir (default ~/.claude/projects)")
	opencodeRoot := fs.String("opencode-root", "", "OpenCode storage dir (default ~/.local/share/opencode/storage)")
	regime := fs.String("regime", "subscription", "billing regime for the Claude Code source: subscription|metered")
	full := fs.Bool("full", false, "re-parse all files even if unchanged (needed once after schema upgrades)")
	fs.Parse(args)

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

	oc := &opencode.Ingester{Root: *opencodeRoot, Full: *full}
	if root, err := ocRootOrSkip(oc); err == nil && root != "" {
		start = time.Now()
		ocStats, err := oc.Run(st)
		if err != nil {
			return err
		}
		log.Printf("opencode: %s (%.1fs)", ocStats, time.Since(start).Seconds())
	} else {
		log.Print("opencode: storage dir not found, skipping")
	}
	return nil
}

// ocRootOrSkip resolves the OpenCode root and returns "" when it doesn't
// exist (OpenCode not installed on this machine) so ingest can skip quietly.
func ocRootOrSkip(oc *opencode.Ingester) (string, error) {
	root := oc.Root
	if root == "" {
		if x := os.Getenv("XDG_DATA_HOME"); x != "" {
			root = filepath.Join(x, "opencode", "storage")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			root = filepath.Join(home, ".local", "share", "opencode", "storage")
		}
	}
	if _, err := os.Stat(root); err != nil {
		return "", nil
	}
	return root, nil
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	by := fs.String("by", "project", "group by: project|model|session")
	since := fs.String("since", "", "window like 7d, 24h, 30m (default: all time)")
	fs.Parse(args)

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
	fs := flag.NewFlagSet("attr", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	by := fs.String("by", "tool", "group by: tool|mcp|file|kind")
	since := fs.String("since", "", "window like 7d, 24h (default: all time)")
	fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sinceTS, err := parseSince(*since)
	if err != nil {
		return err
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

func cmdWaste(args []string) error {
	fs := flag.NewFlagSet("waste", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	since := fs.String("since", "", "window like 7d, 24h (default: all time)")
	limit := fs.Int("limit", 15, "rows per table")
	fs.Parse(args)

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
	return report.RenderWaste(os.Stdout, reads, big)
}

func cmdCache(args []string) error {
	fs := flag.NewFlagSet("cache", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	since := fs.String("since", "", "window like 7d, 24h (default: all time; applied per session)")
	limit := fs.Int("limit", 10, "worst sessions to list")
	session := fs.String("session", "", "session id or slug prefix: per-request drill-down")
	fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sinceTS, err := parseSince(*since)
	if err != nil {
		return err
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

func cmdSession(args []string) error {
	fs := flag.NewFlagSet("session", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	htmlOut := fs.String("html", "", "write a self-contained HTML page to this path instead of the terminal view")
	// Accept the session prefix before or after flags (stdlib flag stops
	// parsing at the first positional argument).
	fs.Parse(args)
	prefix := fs.Arg(0)
	if fs.NArg() > 1 {
		fs.Parse(fs.Args()[1:])
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

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "database path")
	fs.Parse(args)

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
