// Package report renders store rollups as terminal tables.
package report

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

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
