// Package report renders store rollups as terminal tables.
package report

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/erewhon/tokenator/internal/analyze"
	"github.com/erewhon/tokenator/internal/store"
)

// Render writes a rollup as an aligned table. The cached%% column is the
// share of prompt tokens served from cache:
// cache_read / (input + cache_read + cache_creation).
func Render(w io.Writer, by string, rows []store.RollupRow) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tREQS\tINPUT\tOUTPUT\tCACHE RD\tCACHE WR\tCACHED%%\n", headerFor(by))
	var tot store.RollupRow
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			truncate(r.Group, 48), comma(r.Requests), comma(r.Input), comma(r.Output),
			comma(r.CacheRead), comma(r.CacheCreate), cachedPct(r))
		tot.Requests += r.Requests
		tot.Input += r.Input
		tot.Output += r.Output
		tot.CacheRead += r.CacheRead
		tot.CacheCreate += r.CacheCreate
	}
	if len(rows) > 1 {
		fmt.Fprintf(tw, "TOTAL\t%s\t%s\t%s\t%s\t%s\t%s\n",
			comma(tot.Requests), comma(tot.Input), comma(tot.Output),
			comma(tot.CacheRead), comma(tot.CacheCreate), cachedPct(tot))
	}
	return tw.Flush()
}

// RenderAttr writes a block-attribution table plus the calibration footer.
// SHARE is each group's slice of the listed est-token total.
func RenderAttr(w io.Writer, by string, rows []store.AttrRow, cal store.Calibration) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tBLOCKS\tERR\tEST TOKENS\tSHARE\n", strings.ToUpper(by))
	var total store.AttrRow
	for _, r := range rows {
		total.Blocks += r.Blocks
		total.Errors += r.Errors
		total.EstTokens += r.EstTokens
	}
	for _, r := range rows {
		share := "-"
		if total.EstTokens > 0 {
			share = fmt.Sprintf("%.1f%%", 100*float64(r.EstTokens)/float64(total.EstTokens))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			truncate(r.Group, 56), comma(r.Blocks), comma(r.Errors), comma(r.EstTokens), share)
	}
	if len(rows) > 1 {
		fmt.Fprintf(tw, "TOTAL\t%s\t%s\t%s\t\n", comma(total.Blocks), comma(total.Errors), comma(total.EstTokens))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nest tokens are bytes/4 estimates — read them as shares, not absolutes\n")
	fmt.Fprintf(w, "coverage: input-side blocks %s est vs %s metered new-input (%s); output-side %s est vs %s metered output (%s)\n",
		comma(cal.InBlockEst), comma(cal.InMetered), pct(cal.InBlockEst, cal.InMetered),
		comma(cal.OutBlockEst), comma(cal.OutMetered), pct(cal.OutBlockEst, cal.OutMetered))
	fmt.Fprintf(w, "(coverage under 100%% is expected: system prompts, tool schemas, and harness injections are metered but not extracted)\n")
	return nil
}

// RenderWaste writes the repeat-read and oversized-result tables.
func RenderWaste(w io.Writer, reads []store.RepeatReadRow, big []store.BigBlockRow) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "REPEAT READS (same file entering the same session more than once)\n")
	fmt.Fprintf(tw, "PROJECT\tFILE\tREADS\tVERSIONS\tEST TOTAL\tEST WASTED\n")
	for _, r := range reads {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			truncate(r.Project, 24), truncate(r.FilePath, 56),
			comma(r.Reads), comma(r.Versions), comma(r.EstTotal), comma(r.EstWasted))
	}
	if len(reads) == 0 {
		fmt.Fprintf(tw, "(none)\t\t\t\t\t\n")
	}
	fmt.Fprintf(tw, "\nBIGGEST SINGLE TOOL RESULTS\n")
	fmt.Fprintf(tw, "PROJECT\tTOOL\tFILE\tEST TOKENS\tWHEN\n")
	for _, b := range big {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			truncate(b.Project, 24), b.Tool, truncate(b.FilePath, 48), comma(b.EstTokens), b.TS)
	}
	return tw.Flush()
}

func pct(num, den int64) string {
	if den == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(num)/float64(den))
}

// RenderCacheSummary writes the cache doctor's aggregate view.
func RenderCacheSummary(w io.Writer, rep *analyze.CacheReport, limit int) error {
	fmt.Fprintf(w, "CACHE DOCTOR (offline analysis from usage sequences)\n")
	fmt.Fprintf(w, "sessions with cache activity: %s   invalidation events: %s\n",
		comma(int64(len(rep.Sessions))), comma(int64(len(rep.Events))))
	if rep.Expected > 0 {
		fmt.Fprintf(w, "warm-prefix reuse: %.1f%%   tokens re-processed after invalidations: %s\n",
			100*float64(rep.Read)/float64(rep.Expected), comma(rep.Shortfall))
	}

	fmt.Fprintf(w, "\n")
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "CAUSE\tEVENTS\tTOKENS RE-PROCESSED\n")
	causes := make([]string, 0, len(rep.ByCause))
	for c := range rep.ByCause {
		causes = append(causes, c)
	}
	sort.Slice(causes, func(i, j int) bool {
		return rep.ByCause[causes[i]].Shortfall > rep.ByCause[causes[j]].Shortfall
	})
	for _, c := range causes {
		a := rep.ByCause[c]
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c, comma(int64(a.Events)), comma(a.Shortfall))
	}
	if len(causes) == 0 {
		fmt.Fprintf(tw, "(no invalidation events)\t\t\n")
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\nWORST SESSIONS (by tokens re-processed)\n")
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "PROJECT\tSESSION\tEVENTS\tRE-PROCESSED\tREUSE\n")
	n := 0
	for _, s := range rep.Sessions {
		if s.Events == 0 || n >= limit {
			continue
		}
		n++
		label := s.SessionKey
		if s.Title != "" {
			label = truncate(s.SessionKey, 12) + " — " + s.Title
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.1f%%\n",
			truncate(s.Project, 24), truncate(label, 52),
			comma(int64(s.Events)), comma(s.Shortfall), 100*s.Reuse())
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\ncauses: history_edit = request body changed (incl. microcompact/context edits); ttl_expiry = idle gap outlived the cache; compaction = expected rewrite at a compact boundary\n")
	return nil
}

// RenderCacheSession writes the per-request drill-down for one session (or
// a small set matching a prefix), marking invalidation events inline.
func RenderCacheSession(w io.Writer, reqs []analyze.Req, rep *analyze.CacheReport) error {
	events := map[string]analyze.Event{}
	for _, ev := range rep.Events {
		events[ev.TS+"|"+ev.Model] = ev
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "TS\tMODEL\tINPUT\tCACHE RD\tCACHE WR\tNOTE\n")
	for _, r := range reqs {
		note := ""
		if ev, ok := events[r.TS+"|"+r.Model]; ok {
			note = fmt.Sprintf("⚠ %s (re-processed %s)", ev.Cause, comma(ev.Shortfall))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.TS, truncate(r.Model, 24), comma(r.Input), comma(r.CacheRead), comma(r.CacheWrite), note)
	}
	return tw.Flush()
}

func headerFor(by string) string {
	switch by {
	case "model":
		return "MODEL"
	case "session":
		return "SESSION"
	default:
		return "PROJECT"
	}
}

func cachedPct(r store.RollupRow) string {
	prompt := r.Input + r.CacheRead + r.CacheCreate
	if prompt == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(r.CacheRead)/float64(prompt))
}

func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	lead := len(s) % 3
	if lead > 0 {
		out = append(out, s[:lead]...)
	}
	for i := lead; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
