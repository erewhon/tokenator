// Residency attribution: what it costs to KEEP a block in context, not just
// to bring it in. Flow attribution (attr) counts each block once at entry
// size; under prompt caching what a session actually pays scales with how
// many requests a block stays resident for — every later request re-reads it
// (~0.1x warm, full price after an invalidation).
//
//	resident_tokens = est_tokens x requests the block remained in context
//
// The residency window is approximated from timestamps:
//   - a block enters context after its own ts (a tool result produced at T
//     is first sent with the NEXT request), so requests with ts > block.ts
//     count;
//   - a compaction evicts everything before it: the window ends at the first
//     compact boundary after entry (microcompacts emit no boundary and are
//     not modeled — residency overcounts slightly where they fire);
//   - thinking blocks are stripped from context when their turn ends, so
//     their window also ends at the next user_text in the session.
//
// Like flow attribution, resident-token numbers are estimates to be read as
// SHARES; the calibration anchor is metered prompt volume — the sum over
// requests of (input + cache_read + cache_creation), which is exactly "one
// count per request a token was resident for", measured by the API.
package analyze

import "sort"

// ResBlock is one extracted block. TS ordering must be consistent with the
// request stamps of the same session (both come from harness timestamps).
type ResBlock struct {
	SessionID  int64
	Project    string
	SessionKey string
	TS         string
	Kind       string
	Tool       string
	MCPServer  string
	FilePath   string
	EstTokens  int64
}

// Stamp is one request: its timestamp and metered prompt volume
// (input + cache_read + cache_creation).
type Stamp struct {
	SessionID    int64
	TS           string
	PromptTokens int64
}

// BlockResidency is a block with its computed residency.
type BlockResidency struct {
	ResBlock
	Resident  int   // requests the block stayed in context for
	ResTokens int64 // EstTokens * Resident
}

type ResidencyReport struct {
	Items         []BlockResidency
	MeteredPrompt int64 // metered prompt volume over the analyzed sessions
	ExtractedRes  int64 // sum of ResTokens
}

// Residency computes per-block residency. blocks and reqs may arrive in any
// order; compactions maps session id to compact-boundary timestamps.
func Residency(blocks []ResBlock, reqs []Stamp, compactions map[int64][]string) *ResidencyReport {
	rep := &ResidencyReport{Items: make([]BlockResidency, 0, len(blocks))}

	reqTS := map[int64][]string{}
	for _, r := range reqs {
		reqTS[r.SessionID] = append(reqTS[r.SessionID], r.TS)
		rep.MeteredPrompt += r.PromptTokens
	}
	userTS := map[int64][]string{}
	for i := range blocks {
		if blocks[i].Kind == "user_text" {
			userTS[blocks[i].SessionID] = append(userTS[blocks[i].SessionID], blocks[i].TS)
		}
	}
	for _, m := range []map[int64][]string{reqTS, userTS} {
		for _, ts := range m {
			sort.Strings(ts)
		}
	}
	// compaction timestamps arrive sorted from the store; don't rely on it
	sortedComps := map[int64][]string{}
	for id, ts := range compactions {
		c := append([]string(nil), ts...)
		sort.Strings(c)
		sortedComps[id] = c
	}

	for _, b := range blocks {
		end := firstAfter(sortedComps[b.SessionID], b.TS)
		if b.Kind == "thinking" {
			if turnEnd := firstAfter(userTS[b.SessionID], b.TS); turnEnd != "" && (end == "" || turnEnd < end) {
				end = turnEnd
			}
		}
		n := countBetween(reqTS[b.SessionID], b.TS, end)
		item := BlockResidency{ResBlock: b, Resident: n, ResTokens: b.EstTokens * int64(n)}
		rep.ExtractedRes += item.ResTokens
		rep.Items = append(rep.Items, item)
	}
	return rep
}

// firstAfter returns the first timestamp strictly after ts, or "" when none.
func firstAfter(sorted []string, ts string) string {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] > ts })
	if i == len(sorted) {
		return ""
	}
	return sorted[i]
}

// countBetween counts timestamps t with lo < t < hi; hi == "" means no
// upper bound.
func countBetween(sorted []string, lo, hi string) int {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] > lo })
	j := len(sorted)
	if hi != "" {
		j = sort.Search(len(sorted), func(i int) bool { return sorted[i] >= hi })
	}
	if j < i {
		return 0
	}
	return j - i
}

// ResRow is one group in a residency rollup.
type ResRow struct {
	Group      string
	Blocks     int64
	FlowTokens int64 // entry-size tokens (what attr reports)
	ResTokens  int64
}

// MeanResident is the average residency in requests, weighted by size —
// ResTokens / FlowTokens, i.e. "how many requests does a typical token of
// this group sit through".
func (r ResRow) MeanResident() float64 {
	if r.FlowTokens == 0 {
		return 0
	}
	return float64(r.ResTokens) / float64(r.FlowTokens)
}

// RollupResidency groups computed residencies the same way store.AttrRollup
// groups flow: tool (tool_result payloads by tool), mcp (tool traffic by
// server), file (tool_result payloads by path), kind (everything).
func RollupResidency(items []BlockResidency, by string) ([]ResRow, bool) {
	type sel func(b *BlockResidency) (string, bool)
	var pick sel
	switch by {
	case "tool":
		pick = func(b *BlockResidency) (string, bool) {
			if b.Kind != "tool_result" {
				return "", false
			}
			if b.Tool == "" {
				return "(unresolved)", true
			}
			return b.Tool, true
		}
	case "mcp":
		pick = func(b *BlockResidency) (string, bool) { return b.MCPServer, b.MCPServer != "" }
	case "file":
		pick = func(b *BlockResidency) (string, bool) {
			return b.FilePath, b.Kind == "tool_result" && b.FilePath != ""
		}
	case "kind":
		pick = func(b *BlockResidency) (string, bool) { return b.Kind, true }
	default:
		return nil, false
	}
	agg := map[string]*ResRow{}
	for i := range items {
		g, ok := pick(&items[i])
		if !ok {
			continue
		}
		row := agg[g]
		if row == nil {
			row = &ResRow{Group: g}
			agg[g] = row
		}
		row.Blocks++
		row.FlowTokens += items[i].EstTokens
		row.ResTokens += items[i].ResTokens
	}
	rows := make([]ResRow, 0, len(agg))
	for _, r := range agg {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ResTokens > rows[j].ResTokens })
	return rows, true
}

// TopResidents returns the n items with the highest resident-token cost —
// the "what is it costing to keep this around" list for the waste report.
func TopResidents(items []BlockResidency, n int) []BlockResidency {
	sorted := append([]BlockResidency(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ResTokens > sorted[j].ResTokens })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}
