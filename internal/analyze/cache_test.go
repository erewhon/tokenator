package analyze

import "testing"

func i64(v int64) *int64 { return &v }

func TestHealthyAppendOnly(t *testing.T) {
	reqs := []Req{
		{SessionID: 1, TS: "2026-07-01T10:00:00.000Z", Model: "opus", Input: 1000, CacheRead: 0, CacheWrite: 9000},
		{SessionID: 1, TS: "2026-07-01T10:01:00.000Z", Model: "opus", Input: 500, CacheRead: 10000, CacheWrite: 200},
		{SessionID: 1, TS: "2026-07-01T10:02:00.000Z", Model: "opus", Input: 300, CacheRead: 10700, CacheWrite: 100},
	}
	rep := Cache(reqs, nil)
	if len(rep.Events) != 0 {
		t.Fatalf("healthy session produced events: %+v", rep.Events)
	}
	if len(rep.Sessions) != 1 || rep.Sessions[0].Reuse() != 1.0 {
		t.Errorf("want single session with reuse 1.0, got %+v", rep.Sessions)
	}
}

func TestInvalidationHistoryEdit(t *testing.T) {
	reqs := []Req{
		{SessionID: 2, TS: "2026-07-01T10:00:00.000Z", Model: "opus", Input: 100, CacheWrite: 20000},
		{SessionID: 2, TS: "2026-07-01T10:01:00.000Z", Model: "opus", Input: 15000, CacheRead: 1000, CacheWrite: 19000},
	}
	rep := Cache(reqs, nil)
	if len(rep.Events) != 1 {
		t.Fatalf("want 1 event, got %+v", rep.Events)
	}
	ev := rep.Events[0]
	if ev.Cause != "history_edit" || ev.Shortfall != 19100 {
		t.Errorf("got cause=%s shortfall=%d, want history_edit/19100", ev.Cause, ev.Shortfall)
	}
}

func TestInvalidationCompaction(t *testing.T) {
	reqs := []Req{
		{SessionID: 3, TS: "2026-07-01T10:00:00.000Z", Model: "opus", Input: 100, CacheWrite: 20000},
		{SessionID: 3, TS: "2026-07-01T10:05:00.000Z", Model: "opus", Input: 5000, CacheRead: 0, CacheWrite: 6000},
	}
	comps := map[int64][]string{3: {"2026-07-01T10:03:00.000Z"}}
	rep := Cache(reqs, comps)
	if len(rep.Events) != 1 || rep.Events[0].Cause != "compaction" {
		t.Fatalf("want compaction event, got %+v", rep.Events)
	}
}

func TestInvalidationTTL(t *testing.T) {
	// Gap over an hour: expiry regardless of TTL mix.
	reqs := []Req{
		{SessionID: 4, TS: "2026-07-01T10:00:00.000Z", Model: "opus", Input: 100, CacheWrite: 20000},
		{SessionID: 4, TS: "2026-07-01T12:00:00.000Z", Model: "opus", Input: 100, CacheRead: 0, CacheWrite: 20000},
	}
	rep := Cache(reqs, nil)
	if len(rep.Events) != 1 || rep.Events[0].Cause != "ttl_expiry" {
		t.Fatalf("want ttl_expiry (long gap), got %+v", rep.Events)
	}

	// Gap over 5 minutes with 5m-dominant previous writes: also expiry.
	reqs = []Req{
		{SessionID: 5, TS: "2026-07-01T10:00:00.000Z", Model: "opus", Input: 100,
			CacheWrite: 20000, Write5m: i64(20000), Write1h: i64(0)},
		{SessionID: 5, TS: "2026-07-01T10:10:00.000Z", Model: "opus", Input: 100, CacheRead: 0, CacheWrite: 20000},
	}
	rep = Cache(reqs, nil)
	if len(rep.Events) != 1 || rep.Events[0].Cause != "ttl_expiry" {
		t.Fatalf("want ttl_expiry (5m dominant), got %+v", rep.Events)
	}

	// Same 10-minute gap but 1h-dominant writes: NOT expiry.
	reqs = []Req{
		{SessionID: 6, TS: "2026-07-01T10:00:00.000Z", Model: "opus", Input: 100,
			CacheWrite: 20000, Write5m: i64(0), Write1h: i64(20000)},
		{SessionID: 6, TS: "2026-07-01T10:10:00.000Z", Model: "opus", Input: 100, CacheRead: 0, CacheWrite: 20000},
	}
	rep = Cache(reqs, nil)
	if len(rep.Events) != 1 || rep.Events[0].Cause != "history_edit" {
		t.Fatalf("want history_edit (1h writes survive 10m gap), got %+v", rep.Events)
	}
}

func TestNoCacheBackendSkipped(t *testing.T) {
	reqs := []Req{
		{SessionID: 7, TS: "2026-07-01T10:00:00.000Z", Model: "qwen", Input: 10000},
		{SessionID: 7, TS: "2026-07-01T10:01:00.000Z", Model: "qwen", Input: 12000},
	}
	rep := Cache(reqs, nil)
	if len(rep.Events) != 0 || len(rep.Sessions) != 0 {
		t.Fatalf("no-cache stream must not score or flag: %+v %+v", rep.Sessions, rep.Events)
	}
}

func TestModelStreamsIndependent(t *testing.T) {
	// A Haiku interlude (title generation) between two Opus requests must
	// not read as an Opus invalidation.
	reqs := []Req{
		{SessionID: 8, TS: "2026-07-01T10:00:00.000Z", Model: "opus", Input: 1000, CacheWrite: 9000},
		{SessionID: 8, TS: "2026-07-01T10:00:30.000Z", Model: "haiku", Input: 2000, CacheWrite: 100},
		{SessionID: 8, TS: "2026-07-01T10:01:00.000Z", Model: "opus", Input: 500, CacheRead: 10000},
	}
	rep := Cache(reqs, nil)
	if len(rep.Events) != 0 {
		t.Fatalf("interleaved model stream flagged: %+v", rep.Events)
	}
}
