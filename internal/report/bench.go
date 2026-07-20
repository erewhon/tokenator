package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/erewhon/tokenator/internal/store"
)

// RenderBench prints the per-arm comparison for one bench run, with an
// optional per-trial detail table. Token medians come from ingested request
// rows (ground truth); duration is wall time; cost prefers the harness's own
// aggregate (CC) and falls back to summed request cost (OC — unreliable for
// custom providers, flagged in the footer).
func RenderBench(w io.Writer, run store.BenchRun, rows []store.BenchTrialUsage, showTrials bool) error {
	fmt.Fprintf(w, "bench %s (%s)\n", run.RunKey, run.Name)
	span := run.StartedAt
	if run.FinishedAt != "" {
		span += " → " + run.FinishedAt
	} else {
		span += " (in progress)"
	}
	fmt.Fprintln(w, span)
	fmt.Fprintln(w)

	arms := groupByArm(rows)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ARM\tHARNESS\tMODEL\tTRIALS\tPASS\tMED DUR\tP90 DUR\tMED IN\tMED OUT\tMED CACHE RD\tMED CACHE WR\tMED COST")
	missingUsage := 0
	for _, a := range arms {
		var durs, ins, outs, rds, wrs, costs []float64
		pass, completed := 0, 0
		for _, u := range a.rows {
			if u.Status == store.TrialOK || u.Status == store.TrialFail {
				completed++
				durs = append(durs, float64(u.DurationMS))
				if u.Status == store.TrialOK {
					pass++
				}
			}
			if u.HasUsage {
				ins = append(ins, float64(u.Input))
				outs = append(outs, float64(u.Output))
				rds = append(rds, float64(u.CacheRead))
				wrs = append(wrs, float64(u.CacheWrite))
			} else {
				missingUsage++
			}
			if c := trialCost(u); c != nil {
				costs = append(costs, *c)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.name, a.harness, orDash(a.model),
			completed, len(a.rows), passRate(pass, completed),
			durStr(median(durs)), durStr(percentile(durs, 0.9)),
			tokStr(median(ins)), tokStr(median(outs)),
			tokStr(median(rds)), tokStr(median(wrs)),
			costStr(median(costs)))
	}
	tw.Flush()

	if showTrials {
		fmt.Fprintln(w)
		tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ARM\tTRIAL\tSTATUS\tDUR\tREQS\tINPUT\tOUTPUT\tCACHE RD\tCACHE WR\tCOST\tSESSION\tFAILED CHECKS")
		for _, a := range arms {
			for _, u := range a.rows {
				in, out, rd, wr := "-", "-", "-", "-"
				if u.HasUsage {
					in, out, rd, wr = comma(u.Input), comma(u.Output), comma(u.CacheRead), comma(u.CacheWrite)
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					u.Arm, u.TrialIdx, u.Status, durStr(float64(u.DurationMS)),
					u.Requests, in, out, rd, wr,
					costStrPtr(trialCost(u)), short(u.HarnessSessionID), failedChecks(u.ChecksJSON))
			}
		}
		tw.Flush()
	}

	fmt.Fprintln(w)
	if missingUsage > 0 {
		fmt.Fprintf(w, "usage missing for %d trial(s) — run `tokenator bench ingest %s`\n",
			missingUsage, run.RunKey)
	}
	fmt.Fprintln(w, "tokens are per-trial medians from ingested requests (incl. subagent sessions);")
	fmt.Fprintln(w, "cost is harness-reported (OpenCode's is unreliable for custom providers)")
	return nil
}

// RenderBenchList prints `bench list`.
func RenderBenchList(w io.Writer, runs []store.BenchRunSummary) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tNAME\tSTARTED\tDONE\tTRIALS")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n", r.RunKey, r.Name, r.StartedAt, r.Done, r.Trials)
	}
	return tw.Flush()
}

type armGroup struct {
	name, harness, model string
	rows                 []store.BenchTrialUsage
}

func groupByArm(rows []store.BenchTrialUsage) []*armGroup {
	var out []*armGroup
	idx := map[string]*armGroup{}
	for _, u := range rows {
		g, ok := idx[u.Arm]
		if !ok {
			g = &armGroup{name: u.Arm, harness: u.Harness, model: u.Model}
			idx[u.Arm] = g
			out = append(out, g)
		}
		g.rows = append(g.rows, u)
	}
	return out
}

func trialCost(u store.BenchTrialUsage) *float64 {
	if u.CostUSD != nil {
		return u.CostUSD
	}
	if u.ReqCostUSD > 0 {
		c := u.ReqCostUSD
		return &c
	}
	return nil
}

func median(v []float64) float64 { return percentile(v, 0.5) }

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return -1
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(p*float64(len(s)) + 0.5)
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func passRate(pass, completed int) string {
	if completed == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", pass, completed)
}

func durStr(ms float64) string {
	if ms < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0fs", ms/1000)
}

func tokStr(v float64) string {
	if v < 0 {
		return "-"
	}
	return comma(int64(v))
}

func costStr(v float64) string {
	if v < 0 {
		return "-"
	}
	return fmt.Sprintf("$%.4f", v)
}

func costStrPtr(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("$%.4f", *v)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "-"
	}
	return s
}

func failedChecks(checksJSON string) string {
	var checks []struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	}
	if json.Unmarshal([]byte(checksJSON), &checks) != nil {
		return "-"
	}
	var failed []string
	for _, c := range checks {
		if !c.OK {
			failed = append(failed, c.Name)
		}
	}
	if len(failed) == 0 {
		return ""
	}
	return strings.Join(failed, ",")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
