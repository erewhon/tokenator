// Package analyze derives higher-level findings from ingested data.
//
// Cache analysis (offline approximation; see docs/phase1-profiler.md):
// without the wire-level prefix (that's the router phase), invalidations are
// inferred from usage sequences. In a healthy append-only conversation, each
// request's cache_read covers at least the previous request's whole prompt
// (input + cache_read + cache_creation) — the prefix only grows. A
// cache_read materially below that expectation means the cached prefix was
// lost, and what sits between the two requests names the likely cause:
//
//	compaction    a compact_boundary fired between them (legitimate rewrite)
//	ttl_expiry    the gap exceeded the cache TTL (1h hard; >5m when the
//	              previous writes were 5-minute-TTL dominant)
//	history_edit  none of the above: the request body itself changed
//	              (includes microcompact/context-editing, mode switches,
//	              anything that rewrites earlier messages)
//
// Requests are analyzed per (session, model) stream: caches are per-model,
// and background interludes (e.g. Haiku title generation) must not read as
// invalidations of the main stream. Streams with no cache activity at all
// (local models) are skipped. A model stream's cold start is not an event.
package analyze

import (
	"sort"
	"time"
)

// Tunables. Conservative on purpose: expected-prefix excludes the previous
// output (the true prefix grows by it), so a healthy request always clears
// the bar and small legitimate trims (context editing) need to be material
// before they flag.
const (
	minPrefixTokens = 1024 // below the cacheable minimum — ignore
	minShortfall    = 4096 // ignore sub-4k dips as noise
	readRatioBar    = 0.8  // event when rd < 80% of expected
	ttlShort        = 5 * time.Minute
	ttlLong         = time.Hour
)

// Req is one request in analyzer order. TS must be RFC3339 (millisecond
// precision fine); rows must be pre-sorted by (SessionID, TS).
type Req struct {
	SessionID  int64
	Project    string
	SessionKey string
	Title      string
	TS         string
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Write5m    *int64
	Write1h    *int64
}

type Event struct {
	SessionID  int64
	Project    string
	SessionKey string
	TS         string
	Model      string
	Cause      string
	Expected   int64
	CacheRead  int64
	Shortfall  int64
}

type SessionScore struct {
	SessionID  int64
	Project    string
	SessionKey string
	Title      string
	Requests   int
	Events     int
	Expected   int64
	Read       int64 // min(cache_read, expected) summed — reuse numerator
	Shortfall  int64
}

// Reuse returns the warm-prefix reuse ratio in [0,1], or -1 when no
// expectation accrued (single-request or no-cache sessions).
func (s SessionScore) Reuse() float64 {
	if s.Expected == 0 {
		return -1
	}
	return float64(s.Read) / float64(s.Expected)
}

type CacheReport struct {
	Sessions  []SessionScore // only sessions that accrued expectations
	Events    []Event
	ByCause   map[string]*CauseAgg
	Expected  int64
	Read      int64
	Shortfall int64
}

type CauseAgg struct {
	Events    int
	Shortfall int64
}

// Cache runs the analyzer. reqs must be sorted by (SessionID, TS);
// compactions maps session id to compact-boundary timestamps.
func Cache(reqs []Req, compactions map[int64][]string) *CacheReport {
	rep := &CacheReport{ByCause: map[string]*CauseAgg{}}
	byCause := func(c string) *CauseAgg {
		a := rep.ByCause[c]
		if a == nil {
			a = &CauseAgg{}
			rep.ByCause[c] = a
		}
		return a
	}

	var score *SessionScore
	prevByModel := map[string]*Req{}
	flush := func() {
		if score != nil && score.Expected > 0 {
			rep.Sessions = append(rep.Sessions, *score)
		}
		score = nil
		prevByModel = map[string]*Req{}
	}

	for i := range reqs {
		cur := &reqs[i]
		if score == nil || score.SessionID != cur.SessionID {
			flush()
			score = &SessionScore{
				SessionID: cur.SessionID, Project: cur.Project,
				SessionKey: cur.SessionKey, Title: cur.Title,
			}
		}
		score.Requests++

		prev := prevByModel[cur.Model]
		prevByModel[cur.Model] = cur
		if prev == nil {
			continue // cold start of this model stream
		}
		// No cache signal on either side: a backend that doesn't report
		// caching (local models) — reuse math would be meaningless.
		if prev.CacheRead+prev.CacheWrite+cur.CacheRead+cur.CacheWrite == 0 {
			continue
		}
		expected := prev.Input + prev.CacheRead + prev.CacheWrite
		if expected < minPrefixTokens {
			continue
		}
		read := min(cur.CacheRead, expected)
		score.Expected += expected
		score.Read += read
		rep.Expected += expected
		rep.Read += read

		shortfall := expected - cur.CacheRead
		if shortfall < minShortfall || float64(cur.CacheRead) >= readRatioBar*float64(expected) {
			continue
		}
		cause := classify(prev, cur, compactions[cur.SessionID])
		ev := Event{
			SessionID: cur.SessionID, Project: cur.Project, SessionKey: cur.SessionKey,
			TS: cur.TS, Model: cur.Model, Cause: cause,
			Expected: expected, CacheRead: cur.CacheRead, Shortfall: shortfall,
		}
		rep.Events = append(rep.Events, ev)
		rep.Shortfall += shortfall
		score.Events++
		score.Shortfall += shortfall
		agg := byCause(cause)
		agg.Events++
		agg.Shortfall += shortfall
	}
	flush()

	sort.Slice(rep.Sessions, func(i, j int) bool {
		return rep.Sessions[i].Shortfall > rep.Sessions[j].Shortfall
	})
	return rep
}

func classify(prev, cur *Req, compactionTS []string) string {
	for _, cts := range compactionTS {
		if cts > prev.TS && cts <= cur.TS {
			return "compaction"
		}
	}
	pt, errP := time.Parse(time.RFC3339, prev.TS)
	ct, errC := time.Parse(time.RFC3339, cur.TS)
	if errP == nil && errC == nil {
		gap := ct.Sub(pt)
		if gap > ttlLong {
			return "ttl_expiry"
		}
		if gap > ttlShort && shortTTLDominant(prev) {
			return "ttl_expiry"
		}
	}
	return "history_edit"
}

// shortTTLDominant reports whether the previous request's cache writes were
// mostly 5-minute-TTL entries (so a >5m gap plausibly expired them).
func shortTTLDominant(r *Req) bool {
	if r.Write5m == nil || r.Write1h == nil {
		return false
	}
	return *r.Write5m > *r.Write1h
}
