// Package report renders store rollups as terminal tables.
package report

import (
	"fmt"
	"io"
	"strconv"
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
