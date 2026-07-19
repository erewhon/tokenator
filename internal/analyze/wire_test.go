package analyze

import (
	"testing"
	"time"
)

var w0 = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

// wreq builds a WireReq with a healthy-looking usage profile: everything
// previously sent reads back from cache unless the test overrides.
func wreq(sess int64, model string, ts time.Time, chain string, read, write int64) WireReq {
	return WireReq{
		SessionID: sess, Project: "p", SessionKey: "sess", Model: model,
		TS: ts.Format(time.RFC3339), Chain: chain,
		Input: 10, CacheRead: read, CacheWrite: write,
	}
}

func classes(rep *WireReport) []string {
	var out []string
	for _, tr := range rep.Transitions {
		out = append(out, tr.Class)
	}
	return out
}

func TestWireAppendHealthy(t *testing.T) {
	reqs := []WireReq{
		wreq(1, "m", w0, "t,s,m0", 0, 50000),
		wreq(1, "m", w0.Add(time.Minute), "t,s,m0,m1,m2", 50010, 3000),
	}
	rep := Wire(reqs, nil)
	if got := classes(rep); len(got) != 1 || got[0] != "append" {
		t.Fatalf("classes = %v, want [append]", got)
	}
	tr := rep.Transitions[0]
	if tr.Event {
		t.Errorf("healthy append flagged as event: %+v", tr)
	}
	if tr.Common != 3 || tr.PrevSegs != 3 || tr.CurSegs != 5 {
		t.Errorf("segs = %d/%d common %d, want 3/5 common 3", tr.PrevSegs, tr.CurSegs, tr.Common)
	}
	if rep.Streams != 1 || rep.Requests != 2 {
		t.Errorf("streams=%d requests=%d, want 1/2", rep.Streams, rep.Requests)
	}
}

func TestWireTTLAndUnexplained(t *testing.T) {
	// Chain appends cleanly but the cache misses: after a 10-minute idle
	// gap that's ttl_expiry; after 30 seconds it's unexplained.
	for _, tc := range []struct {
		gap  time.Duration
		want string
	}{
		{10 * time.Minute, "ttl_expiry"},
		{30 * time.Second, "unexplained_miss"},
	} {
		reqs := []WireReq{
			wreq(1, "m", w0, "t,s,m0", 0, 50000),
			wreq(1, "m", w0.Add(tc.gap), "t,s,m0,m1", 0, 51000),
		}
		rep := Wire(reqs, nil)
		tr := rep.Transitions[0]
		if tr.Class != tc.want || !tr.Event {
			t.Errorf("gap %v: class=%s event=%v, want %s event", tc.gap, tr.Class, tr.Event, tc.want)
		}
		if tr.Shortfall != 50010 {
			t.Errorf("gap %v: shortfall = %d, want 50010", tc.gap, tr.Shortfall)
		}
	}
}

func TestWireDivergenceNaming(t *testing.T) {
	for _, tc := range []struct {
		cur       string
		want      string
		editIndex int
	}{
		{"T2,s,m0,m1,m2", "tools_changed", -1},
		{"t,S2,m0,m1,m2", "system_changed", -1},
		{"t,s,M9,m1,m2", "history_edit", 0},
		{"t,s,m0,M9,extra", "history_edit", 1},
		{"t,s", "truncated", -1},
		// Both wire-unchanged shapes reclassify to unexplained_miss under
		// the miss gates (this usage profile reads nothing back).
		{"t,s,m0,m1,m2", "unexplained_miss", -1}, // identical
		{"t,s,m0,m1,M9", "unexplained_miss", -1}, // marker_rotation
	} {
		reqs := []WireReq{
			wreq(1, "m", w0, "t,s,m0,m1,m2", 0, 50000),
			wreq(1, "m", w0.Add(time.Minute), tc.cur, 0, 50000),
		}
		rep := Wire(reqs, nil)
		tr := rep.Transitions[0]
		if tr.Class != tc.want || tr.EditIndex != tc.editIndex {
			t.Errorf("cur=%q: class=%s edit=%d, want %s/%d",
				tc.cur, tr.Class, tr.EditIndex, tc.want, tc.editIndex)
		}
	}
}

func TestWireMarkerRotation(t *testing.T) {
	// The previous request's final message re-serializes (cache_control
	// marker moved) while the cache stays warm: benign, not an edit.
	reqs := []WireReq{
		wreq(1, "m", w0, "t,s,m0,m1", 0, 50000),
		wreq(1, "m", w0.Add(time.Minute), "t,s,m0,M9,m2", 50008, 900),
	}
	rep := Wire(reqs, nil)
	tr := rep.Transitions[0]
	if tr.Class != "marker_rotation" || tr.Event || tr.EditIndex != -1 {
		t.Errorf("transition = %+v, want benign marker_rotation", tr)
	}

	// Same shape after an hour idle with a cold read: the rotation is
	// cosmetic and the miss is the gap's fault — ttl_expiry.
	reqs[1] = wreq(1, "m", w0.Add(70*time.Minute), "t,s,m0,M9,m2", 0, 51000)
	rep = Wire(reqs, nil)
	tr = rep.Transitions[0]
	if tr.Class != "ttl_expiry" || !tr.Event {
		t.Errorf("idle rotation = %+v, want ttl_expiry event", tr)
	}
}

func TestWireEventCausePrecedence(t *testing.T) {
	// A real divergence explains a quick miss; a TTL-exceeding idle gap
	// outranks it (the miss was inevitable either way).
	for _, tc := range []struct {
		gap  time.Duration
		want string
	}{
		{time.Minute, "tools_changed"},
		{2 * time.Hour, "ttl_expiry"},
	} {
		reqs := []WireReq{
			wreq(1, "m", w0, "t,s,m0,m1,m2", 0, 50000),
			wreq(1, "m", w0.Add(tc.gap), "T2,s,m0,m1,m2", 0, 50000),
		}
		rep := Wire(reqs, nil)
		tr := rep.Transitions[0]
		if tr.Class != tc.want || !tr.Event {
			t.Errorf("gap %v: class=%s event=%v, want %s event", tc.gap, tr.Class, tr.Event, tc.want)
		}
	}

	// The 5m-TTL-dominant path: divergence + 10m gap normally keeps the
	// divergence class, but 5m-dominant previous writes make TTL the cause.
	five, one := int64(40000), int64(0)
	prev := wreq(1, "m", w0, "t,s,m0,m1,m2", 0, 50000)
	prev.Write5m, prev.Write1h = &five, &one
	reqs := []WireReq{prev, wreq(1, "m", w0.Add(10*time.Minute), "T2,s,m0,m1,m2", 0, 50000)}
	rep := Wire(reqs, nil)
	if tr := rep.Transitions[0]; tr.Class != "ttl_expiry" {
		t.Errorf("5m-dominant divergence: class=%s, want ttl_expiry", tr.Class)
	}
}

func TestWireCompactionPromotion(t *testing.T) {
	comps := map[int64][]string{1: {w0.Add(30 * time.Second).Format(time.RFC3339)}}
	reqs := []WireReq{
		wreq(1, "m", w0, "t,s,m0,m1,m2", 0, 50000),
		wreq(1, "m", w0.Add(time.Minute), "t,s,M9", 0, 48000),
	}
	rep := Wire(reqs, comps)
	tr := rep.Transitions[0]
	if tr.Class != "compaction" || tr.EditIndex != 0 {
		t.Errorf("class=%s edit=%d, want compaction/0", tr.Class, tr.EditIndex)
	}
	// Same shape without a boundary stays history_edit.
	rep = Wire(reqs, nil)
	if tr := rep.Transitions[0]; tr.Class != "history_edit" {
		t.Errorf("without boundary: class=%s, want history_edit", tr.Class)
	}
}

func TestWireModelStreamsIndependent(t *testing.T) {
	// A haiku interlude must not read as a divergence of the main stream.
	reqs := []WireReq{
		wreq(1, "main", w0, "t,s,m0", 0, 50000),
		wreq(1, "haiku", w0.Add(10*time.Second), "x,y", 0, 500),
		wreq(1, "main", w0.Add(20*time.Second), "t,s,m0,m1", 50010, 800),
	}
	rep := Wire(reqs, nil)
	if len(rep.Transitions) != 1 || rep.Transitions[0].Class != "append" {
		t.Fatalf("transitions = %v, want one append", classes(rep))
	}
	if rep.Streams != 1 {
		t.Errorf("streams = %d, want 1 (haiku never got a second request)", rep.Streams)
	}
}

func TestWireSessionScores(t *testing.T) {
	reqs := []WireReq{
		// session 1: healthy
		wreq(1, "m", w0, "t,s,m0", 0, 50000),
		wreq(1, "m", w0.Add(time.Minute), "t,s,m0,m1", 50010, 900),
		// session 2: an expensive history edit
		wreq(2, "m", w0, "t,s,m0", 0, 90000),
		wreq(2, "m", w0.Add(time.Minute), "t,s,M9", 1000, 89000),
	}
	rep := Wire(reqs, nil)
	if len(rep.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(rep.Sessions))
	}
	// Worst first: session 2's shortfall dominates.
	if rep.Sessions[0].SessionID != 2 || rep.Sessions[0].Events != 1 {
		t.Errorf("worst session = %+v, want session 2 with 1 event", rep.Sessions[0])
	}
	if rep.Sessions[1].Events != 0 {
		t.Errorf("healthy session has events: %+v", rep.Sessions[1])
	}
	if rep.ByClass["history_edit"] == nil || rep.ByClass["history_edit"].Shortfall != 89010 {
		t.Errorf("history_edit agg = %+v", rep.ByClass["history_edit"])
	}
}
