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

// RenderAttrResidency writes the residency-weighted attribution table.
// RESIDENT TOK ≈ est tokens × requests the block stayed in context; AVG RES
// is the size-weighted mean residency in requests.
func RenderAttrResidency(w io.Writer, by string, rows []analyze.ResRow, rep *analyze.ResidencyReport) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tBLOCKS\tFLOW TOK\tRESIDENT TOK\tAVG RES\tSHARE\n", strings.ToUpper(by))
	var total analyze.ResRow
	for _, r := range rows {
		total.Blocks += r.Blocks
		total.FlowTokens += r.FlowTokens
		total.ResTokens += r.ResTokens
	}
	for _, r := range rows {
		share := "-"
		if total.ResTokens > 0 {
			share = fmt.Sprintf("%.1f%%", 100*float64(r.ResTokens)/float64(total.ResTokens))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f\t%s\n",
			truncate(r.Group, 56), comma(r.Blocks), comma(r.FlowTokens),
			comma(r.ResTokens), r.MeanResident(), share)
	}
	if len(rows) > 1 {
		fmt.Fprintf(tw, "TOTAL\t%s\t%s\t%s\t%.0f\t\n",
			comma(total.Blocks), comma(total.FlowTokens), comma(total.ResTokens), total.MeanResident())
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nresident tok ≈ est tokens × requests the block stayed in context (compaction-bounded; thinking counted only within its turn) — read as shares\n")
	fmt.Fprintf(w, "coverage: extracted resident %s vs %s metered prompt volume (%s)\n",
		comma(rep.ExtractedRes), comma(rep.MeteredPrompt), pct(rep.ExtractedRes, rep.MeteredPrompt))
	fmt.Fprintf(w, "(the gap is expected: system prompts, tool schemas, and harness injections are resident every request but not extracted)\n")
	return nil
}

// RenderWaste writes the repeat-read, oversized-result, long-resident, and
// stale-passenger tables. staleKnown/staleUnref are resident-token sums over
// tool results with a reference verdict.
func RenderWaste(w io.Writer, reads []store.RepeatReadRow, big []store.BigBlockRow,
	heavy, stale []analyze.BlockResidency, staleKnown, staleUnref int64) error {
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
	if len(heavy) > 0 {
		fmt.Fprintf(tw, "\nLONG-RESIDENT HEAVYWEIGHTS (cost of keeping one block in context)\n")
		fmt.Fprintf(tw, "PROJECT\tKIND\tORIGIN\tENTERED\tEST TOK\tRES×\tRESIDENT TOK\tREF\n")
		for _, h := range heavy {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				truncate(h.Project, 24), h.Kind, truncate(originOf(h), 44),
				h.TS, comma(h.EstTokens), h.Resident, comma(h.ResTokens), refMark(h.Referenced))
		}
	}
	if len(stale) > 0 {
		fmt.Fprintf(tw, "\nSTALE PASSENGERS (tool results likely never referenced after entering)\n")
		fmt.Fprintf(tw, "PROJECT\tTOOL\tORIGIN\tENTERED\tEST TOK\tRES×\tRESIDENT TOK\n")
		for _, s := range stale {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
				truncate(s.Project, 24), s.Tool, truncate(originOf(s), 44),
				s.TS, comma(s.EstTokens), s.Resident, comma(s.ResTokens))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if staleKnown > 0 {
		fmt.Fprintf(w, "\nof %s resident tool-result tokens with a reference verdict, %s (%s) were never referenced by later text, thinking, tool calls, or your messages — identifier-overlap heuristic, read as \"likely\"\n",
			comma(staleKnown), comma(staleUnref), pct(staleUnref, staleKnown))
	}
	return nil
}

// refMark renders a reference verdict: y referenced, n never, ? unanalyzed.
func refMark(referenced int) string {
	switch referenced {
	case 1:
		return "y"
	case 2:
		return "n"
	}
	return "?"
}

// originOf names where a block came from, most specific field first.
func originOf(h analyze.BlockResidency) string {
	switch {
	case h.FilePath != "":
		return h.FilePath
	case h.Tool != "":
		return h.Tool
	case h.MCPServer != "":
		return h.MCPServer
	}
	return ""
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

// RenderOTelStatus writes the OTel capture summary plus the api_request
// cross-check against transcript-ingested request rows.
func RenderOTelStatus(w io.Writer, st store.OTelStatus) error {
	if st.Datapoints == 0 && st.Events == 0 {
		fmt.Fprintf(w, "no OTel data captured — run `tokenator otel` and point Claude Code at it\n")
		return nil
	}
	fmt.Fprintf(w, "OTEL CAPTURE (live receiver data)\n")
	fmt.Fprintf(w, "sessions: %s   datapoints: %s   events: %s\n",
		comma(st.Sessions), comma(st.Datapoints), comma(st.Events))

	if len(st.TokensByType) > 0 {
		fmt.Fprintf(w, "\n")
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "TOKENS (claude_code.token.usage)\t\n")
		for _, nv := range st.TokensByType {
			fmt.Fprintf(tw, "%s\t%s\n", nv.Name, comma(int64(nv.Value)))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if st.CostUSD > 0 {
		fmt.Fprintf(w, "reported cost: $%.4f\n", st.CostUSD)
	}

	if len(st.EventCounts) > 0 {
		fmt.Fprintf(w, "\n")
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "EVENT\tCOUNT\n")
		for _, nv := range st.EventCounts {
			fmt.Fprintf(tw, "%s\t%s\n", nv.Name, comma(int64(nv.Value)))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if st.APIReqEvents > 0 {
		fmt.Fprintf(w, "\nCROSS-CHECK (api_request events vs transcript-ingested requests, by request_id)\n")
		fmt.Fprintf(w, "events: %s   matched: %s (%s)\n",
			comma(st.APIReqEvents), comma(st.APIReqMatched), pct(st.APIReqMatched, st.APIReqEvents))
		if st.APIReqMatched > 0 {
			fmt.Fprintf(w, "matched input tokens: otel %s vs transcript %s   output: otel %s vs transcript %s\n",
				comma(st.OTelInput), comma(st.JSONLInput), comma(st.OTelOutput), comma(st.JSONLOutput))
		}
		fmt.Fprintf(w, "(unmatched events are normal: auxiliary calls like title generation never land in transcripts,\n and transcripts need a fresh `tokenator ingest` to be current)\n")
	}
	return nil
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
