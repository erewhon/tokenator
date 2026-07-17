package report

import (
	"sort"

	"github.com/erewhon/tokenator/internal/analyze"
	"github.com/erewhon/tokenator/internal/store"
)

// KindOrder is the fixed categorical order for block kinds. Fixed order is
// the palette's colorblind-safety mechanism (see the dataviz method): a kind
// always wears the same slot, regardless of which kinds a session contains.
var KindOrder = []string{
	"tool_result", "assistant_text", "thinking",
	"user_text", "meta_text", "tool_use", "image",
}

// ReqPoint is one request on the session timeline.
type ReqPoint struct {
	TS         string
	Model      string
	PromptSize int64 // input + cache_read + cache_creation: context occupancy
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	Event      *analyze.Event // invalidation flagged at this request, if any
	Compaction bool           // a compact boundary fired just before this request
}

// Bucket sums new extracted content (est tokens by kind) for a span of
// consecutive requests.
type Bucket struct {
	StartIdx, EndIdx int // request index range [start, end]
	ByKind           map[string]int64
}

type SessionView struct {
	Meta      store.SessionMeta
	Models    []string
	Start     string
	End       string
	Points    []ReqPoint
	Buckets   []Bucket
	Events    []analyze.Event
	Reuse     float64 // -1 when no cache expectation accrued
	Shortfall int64
	TotalIn   int64
	TotalOut  int64
	TotalRead int64
	TotalWrit int64
	NewEst    int64 // est tokens of all extracted blocks
	TopTools  []store.AttrRow
	TopFiles  []store.AttrRow
	Kinds     []store.AttrRow // composition table (relief for low-contrast slots)
}

const maxBuckets = 60

// BuildSessionView assembles everything the session renderers need.
func BuildSessionView(st *store.Store, prefix string) (*SessionView, error) {
	meta, err := st.SessionByPrefix(prefix)
	if err != nil {
		return nil, err
	}
	rows, err := st.RequestsForCache("", prefix)
	if err != nil {
		return nil, err
	}
	comps, err := st.CompactionTimes()
	if err != nil {
		return nil, err
	}

	var reqs []analyze.Req
	for _, r := range rows {
		if r.SessionID != meta.ID {
			continue
		}
		reqs = append(reqs, analyze.Req{
			SessionID: r.SessionID, Project: r.Project, SessionKey: r.SessionKey,
			Title: r.Title, TS: r.TS, Model: r.Model,
			Input: r.Input, Output: r.Output,
			CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
			Write5m: r.Write5m, Write1h: r.Write1h,
		})
	}
	rep := analyze.Cache(reqs, comps)

	v := &SessionView{Meta: meta, Events: rep.Events, Shortfall: rep.Shortfall, Reuse: -1}
	if rep.Expected > 0 {
		v.Reuse = float64(rep.Read) / float64(rep.Expected)
	}

	eventAt := map[string]*analyze.Event{}
	for i := range rep.Events {
		ev := &rep.Events[i]
		eventAt[ev.TS+"|"+ev.Model] = ev
	}
	compTS := comps[meta.ID]
	models := map[string]bool{}
	prevTS := ""
	for _, r := range reqs {
		models[r.Model] = true
		pt := ReqPoint{
			TS: r.TS, Model: r.Model,
			PromptSize: r.Input + r.CacheRead + r.CacheWrite,
			Input:      r.Input, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
			Output: r.Output,
			Event:  eventAt[r.TS+"|"+r.Model],
		}
		for _, cts := range compTS {
			if cts > prevTS && cts <= r.TS {
				pt.Compaction = true
			}
		}
		v.Points = append(v.Points, pt)
		v.TotalIn += r.Input
		v.TotalOut += r.Output
		v.TotalRead += r.CacheRead
		v.TotalWrit += r.CacheWrite
		prevTS = r.TS
	}
	if len(v.Points) > 0 {
		v.Start, v.End = v.Points[0].TS, v.Points[len(v.Points)-1].TS
	}
	for m := range models {
		v.Models = append(v.Models, m)
	}
	sort.Strings(v.Models)

	blocks, err := st.BlocksForSession(meta.ID)
	if err != nil {
		return nil, err
	}
	v.Buckets = bucketBlocks(v.Points, blocks)
	for _, b := range blocks {
		v.NewEst += b.EstTokens
	}

	if v.TopTools, err = st.AttrRollup("tool", "", meta.ID); err != nil {
		return nil, err
	}
	if v.TopFiles, err = st.AttrRollup("file", "", meta.ID); err != nil {
		return nil, err
	}
	if v.Kinds, err = st.AttrRollup("kind", "", meta.ID); err != nil {
		return nil, err
	}
	const topN = 10
	if len(v.TopTools) > topN {
		v.TopTools = v.TopTools[:topN]
	}
	if len(v.TopFiles) > topN {
		v.TopFiles = v.TopFiles[:topN]
	}
	return v, nil
}

// bucketBlocks assigns each block to the request whose timestamp first
// reaches it (approximately: the request that carried it into context), then
// groups requests into at most maxBuckets spans.
func bucketBlocks(points []ReqPoint, blocks []store.SessionBlock) []Bucket {
	if len(points) == 0 || len(blocks) == 0 {
		return nil
	}
	perReq := make([]map[string]int64, len(points))
	for _, b := range blocks {
		idx := sort.Search(len(points), func(i int) bool { return points[i].TS >= b.TS })
		if idx == len(points) {
			idx = len(points) - 1
		}
		if perReq[idx] == nil {
			perReq[idx] = map[string]int64{}
		}
		perReq[idx][b.Kind] += b.EstTokens
	}
	span := (len(points) + maxBuckets - 1) / maxBuckets
	var out []Bucket
	for start := 0; start < len(points); start += span {
		end := min(start+span-1, len(points)-1)
		bkt := Bucket{StartIdx: start, EndIdx: end, ByKind: map[string]int64{}}
		for i := start; i <= end; i++ {
			for k, v := range perReq[i] {
				bkt.ByKind[k] += v
			}
		}
		out = append(out, bkt)
	}
	return out
}
