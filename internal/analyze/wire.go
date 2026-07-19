// Wire cache analysis: the ground-truth counterpart to Cache. Where the
// offline doctor infers invalidations from usage sequences, the wire doctor
// reads the router's prefix hash chain — cumulative per-segment digests of
// the rendered prompt in cache order (tools → system → each message) — and
// names the exact segment where consecutive requests diverged:
//
//	segment 0    tools changed (tool set edits invalidate everything)
//	segment 1    system changed
//	segment k≥2  message k-2 rewritten — history_edit, promoted to
//	             compaction when a compact_boundary sits between the two
//	             requests
//
// One wrinkle the raw chain can't hide from usage: Claude Code moves its
// cache_control breakpoint marker to the newest message every request, so
// the previous request's FINAL message always re-serializes differently —
// the chain "diverges" at exactly PrevSegs-1 while the cacheable content is
// untouched (observed: shortfall of 1-2 tokens across hundreds of such
// transitions). Tail-only divergence is therefore classified
// marker_rotation and treated as append-equivalent for cause analysis.
//
// Event causes follow the offline doctor's precedence: compaction first
// (the boundary names the rewrite), then TTL — a gap past the cache
// lifetime makes the miss inevitable no matter what else changed on the
// wire (observed: tools-segment divergences behind 2-4h idle gaps, where
// blaming the tool change would suggest a fixable cost that isn't) — and
// only then the divergence class itself. A miss with no content divergence
// and no TTL story is unexplained_miss (provider-side eviction).
//
// Divergences that do NOT cost anything keep their chain class but are
// usually cosmetic re-serialization, not content changes: a tools_changed
// with a fully warm cache read proves the tools bytes differed while the
// cacheable content didn't (cache_control placement again).
//
// The segment→name mapping assumes tools and system are both present, which
// holds for every Claude Code request; a client that omits either would
// shift the indices.
//
// Streams are per (session, model) like the offline doctor, and the cost
// gates (minPrefixTokens, minShortfall, readRatioBar) are shared, so the
// two reports' event counts and shortfalls are directly comparable.
package analyze

import (
	"sort"
	"strings"
	"time"
)

// WireReq is one matched gateway request in analyzer order. Rows must be
// pre-sorted by (SessionID, TS); Chain is the comma-joined hash chain.
type WireReq struct {
	SessionID  int64
	Project    string
	SessionKey string
	Title      string
	TS         string
	Model      string
	Chain      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Write5m    *int64 // TTL split from the matched transcript row, when reported
	Write1h    *int64
}

// WireTransition is one consecutive request pair within a (session, model)
// stream, classified from the chain diff.
type WireTransition struct {
	SessionID  int64
	Project    string
	SessionKey string
	TS         string // of the later request
	Model      string
	GapSec     int64
	PrevSegs   int
	CurSegs    int
	Common     int    // segments shared before divergence
	Class      string // append|identical|marker_rotation|tools_changed|system_changed|history_edit|compaction|truncated|ttl_expiry|unexplained_miss
	EditIndex  int    // message index for history_edit/compaction; -1 otherwise
	Expected   int64  // prev prompt total — what a fully-warm append would read
	CacheRead  int64
	Shortfall  int64
	Event      bool // cleared the cost gates (material token loss)
}

type WireClassAgg struct {
	Transitions int
	Events      int
	Shortfall   int64
}

type WireReport struct {
	Streams     int // (session, model) streams with ≥2 chained requests
	Requests    int
	Sessions    []SessionScore // sessions that accrued expectations, worst first
	Transitions []WireTransition
	ByClass     map[string]*WireClassAgg
	Expected    int64
	Read        int64
	Shortfall   int64
}

// Wire runs the analyzer. reqs must be sorted by (SessionID, TS);
// compactions maps session id to compact-boundary timestamps.
func Wire(reqs []WireReq, compactions map[int64][]string) *WireReport {
	rep := &WireReport{ByClass: map[string]*WireClassAgg{}}
	byClass := func(c string) *WireClassAgg {
		a := rep.ByClass[c]
		if a == nil {
			a = &WireClassAgg{}
			rep.ByClass[c] = a
		}
		return a
	}

	var score *SessionScore
	prevByModel := map[string]*WireReq{}
	chainByModel := map[string][]string{}
	streamLen := map[string]int{}
	flush := func() {
		if score != nil && score.Expected > 0 {
			rep.Sessions = append(rep.Sessions, *score)
		}
		score = nil
		prevByModel = map[string]*WireReq{}
		chainByModel = map[string][]string{}
		streamLen = map[string]int{}
	}

	for i := range reqs {
		cur := &reqs[i]
		rep.Requests++
		if score == nil || score.SessionID != cur.SessionID {
			flush()
			score = &SessionScore{
				SessionID: cur.SessionID, Project: cur.Project,
				SessionKey: cur.SessionKey, Title: cur.Title,
			}
		}
		score.Requests++

		curChain := strings.Split(cur.Chain, ",")
		prev, prevChain := prevByModel[cur.Model], chainByModel[cur.Model]
		prevByModel[cur.Model] = cur
		chainByModel[cur.Model] = curChain
		streamLen[cur.Model]++
		if streamLen[cur.Model] == 2 {
			rep.Streams++
		}
		if prev == nil {
			continue // cold start of this model stream
		}

		tr := diffChains(prev, cur, prevChain, curChain, compactions[cur.SessionID])

		expected := prev.Input + prev.CacheRead + prev.CacheWrite
		if expected >= minPrefixTokens && prev.CacheRead+prev.CacheWrite+cur.CacheRead+cur.CacheWrite > 0 {
			tr.Expected = expected
			tr.Shortfall = expected - cur.CacheRead
			read := min(cur.CacheRead, expected)
			score.Expected += expected
			score.Read += read
			rep.Expected += expected
			rep.Read += read
			if tr.Shortfall >= minShortfall && float64(cur.CacheRead) < readRatioBar*float64(expected) {
				tr.Event = true
				tr.Class = eventCause(tr.Class, tr.GapSec, prev)
				score.Events++
				score.Shortfall += tr.Shortfall
				rep.Shortfall += tr.Shortfall
			}
		}
		if tr.Shortfall < 0 {
			tr.Shortfall = 0
		}

		rep.Transitions = append(rep.Transitions, tr)
		agg := byClass(tr.Class)
		agg.Transitions++
		if tr.Event {
			agg.Events++
			agg.Shortfall += tr.Shortfall
		}
	}
	flush()

	sort.Slice(rep.Sessions, func(i, j int) bool {
		return rep.Sessions[i].Shortfall > rep.Sessions[j].Shortfall
	})
	return rep
}

// eventCause resolves what a material miss should be blamed on, given the
// chain-diff class. Compaction keeps its name (the boundary explains the
// rewrite); a TTL-exceeding idle gap outranks any divergence (the miss was
// inevitable, and under a dead cache a divergence may itself be cosmetic);
// wire-unchanged shapes with no TTL story are provider-side evictions;
// remaining divergence classes stand on their own.
func eventCause(class string, gapSec int64, prev *WireReq) string {
	if class == "compaction" {
		return class
	}
	gap := time.Duration(gapSec) * time.Second
	if gap > ttlLong || (gap > ttlShort && shortTTLDominantWire(prev)) {
		return "ttl_expiry"
	}
	if class == "append" || class == "identical" || class == "marker_rotation" {
		if gap > ttlShort {
			return "ttl_expiry"
		}
		return "unexplained_miss"
	}
	return class
}

// shortTTLDominantWire mirrors shortTTLDominant for wire rows.
func shortTTLDominantWire(r *WireReq) bool {
	if r.Write5m == nil || r.Write1h == nil {
		return false
	}
	return *r.Write5m > *r.Write1h
}

// diffChains classifies one transition from the chain diff alone; usage
// gates and event-cause resolution happen in the caller.
func diffChains(prev, cur *WireReq, prevChain, curChain []string, compactionTS []string) WireTransition {
	tr := WireTransition{
		SessionID: cur.SessionID, Project: cur.Project, SessionKey: cur.SessionKey,
		TS: cur.TS, Model: cur.Model,
		PrevSegs: len(prevChain), CurSegs: len(curChain),
		CacheRead: cur.CacheRead, EditIndex: -1,
	}
	if pt, err := time.Parse(time.RFC3339, prev.TS); err == nil {
		if ct, err := time.Parse(time.RFC3339, cur.TS); err == nil {
			tr.GapSec = int64(ct.Sub(pt) / time.Second)
		}
	}
	common := 0
	for common < len(prevChain) && common < len(curChain) && prevChain[common] == curChain[common] {
		common++
	}
	tr.Common = common
	switch {
	case common == len(prevChain) && common == len(curChain):
		tr.Class = "identical"
	case common == len(prevChain):
		tr.Class = "append"
	case common == len(curChain):
		tr.Class = "truncated"
	case common == len(prevChain)-1 && len(prevChain) >= 4:
		// Only the previous request's final message re-serialized: the
		// cache_control marker moved off it. Content-equivalent to append.
		// The length guard keeps degenerate short chains (tools/system
		// only) on the segment-naming path below.
		tr.Class = "marker_rotation"
	case common == 0:
		tr.Class = "tools_changed"
	case common == 1:
		tr.Class = "system_changed"
	default:
		tr.Class = "history_edit"
		tr.EditIndex = common - 2
		for _, cts := range compactionTS {
			if cts > prev.TS && cts <= cur.TS {
				tr.Class = "compaction"
				break
			}
		}
	}
	return tr
}
