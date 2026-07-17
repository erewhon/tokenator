package report

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// RenderSessionTerm writes the compact terminal session view: header, stat
// line, a context-size sparkline with compaction/invalidation markers, the
// composition split, top tools, and the event list.
func RenderSessionTerm(w io.Writer, v *SessionView) error {
	fmt.Fprintf(w, "%s\n", sessionLabel(v))
	fmt.Fprintf(w, "session %s · %s → %s · %s\n",
		v.Meta.Key, v.Start, v.End, strings.Join(v.Models, ", "))
	fmt.Fprintf(w, "requests %d · out %s · cache rd %s / wr %s · reuse %s · re-processed %s · new content ≈%s\n\n",
		len(v.Points), comma(v.TotalOut), comma(v.TotalRead), comma(v.TotalWrit),
		reusePct(v.Reuse), comma(v.Shortfall), comma(v.NewEst))

	if len(v.Points) > 0 {
		renderSparkline(w, v)
	}

	if len(v.Kinds) > 0 {
		var parts []string
		var total int64
		for _, k := range v.Kinds {
			total += k.EstTokens
		}
		for _, kr := range kindRows(v) {
			parts = append(parts, fmt.Sprintf("%s %s", kr.Kind, kr.Share))
		}
		fmt.Fprintf(w, "composition: %s\n\n", strings.Join(parts, " · "))
	}

	if len(v.TopTools) > 0 {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "TOOL\tCALLS\tERR\tEST TOKENS\n")
		for i, r := range v.TopTools {
			if i >= 5 {
				break
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				truncate(r.Group, 40), comma(r.Blocks), comma(r.Errors), comma(r.EstTokens))
		}
		tw.Flush()
		fmt.Fprintln(w)
	}

	if len(v.Events) > 0 {
		fmt.Fprintf(w, "INVALIDATION EVENTS\n")
		for i, ev := range v.Events {
			if i >= 10 {
				fmt.Fprintf(w, "  … %d more\n", len(v.Events)-10)
				break
			}
			fmt.Fprintf(w, "  %s  %-12s  re-processed %s\n", ev.TS, ev.Cause, comma(ev.Shortfall))
		}
	}
	return nil
}

// renderSparkline draws context size per request, max-pooled to termWidth
// columns, with a marker row beneath: C = compaction, ! = invalidation.
func renderSparkline(w io.Writer, v *SessionView) {
	const termWidth = 72
	n := len(v.Points)
	cols := min(termWidth, n)
	var ymax int64
	for _, p := range v.Points {
		ymax = max(ymax, p.PromptSize)
	}
	if ymax == 0 {
		return
	}
	spark := make([]rune, cols)
	marks := make([]rune, cols)
	for c := range cols {
		lo, hi := c*n/cols, (c+1)*n/cols
		if hi <= lo {
			hi = lo + 1
		}
		var peak int64
		mark := ' '
		for i := lo; i < hi && i < n; i++ {
			peak = max(peak, v.Points[i].PromptSize)
			if v.Points[i].Compaction && mark == ' ' {
				mark = 'C'
			}
			if v.Points[i].Event != nil {
				mark = '!'
			}
		}
		idx := int(peak * int64(len(sparkRunes)-1) / ymax)
		spark[c] = sparkRunes[idx]
		marks[c] = mark
	}
	fmt.Fprintf(w, "context size per request (max %s tokens)\n", comma(ymax))
	fmt.Fprintf(w, "  %s\n", string(spark))
	if strings.TrimSpace(string(marks)) != "" {
		fmt.Fprintf(w, "  %s   (C compaction, ! invalidation)\n", string(marks))
	}
	fmt.Fprintln(w)
}
