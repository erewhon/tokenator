package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/erewhon/tokenator/internal/analyze"
	"github.com/erewhon/tokenator/internal/store"
)

func testView() *SessionView {
	pts := []ReqPoint{
		{TS: "2026-07-01T10:00:00.000Z", Model: "claude-opus-4-8", PromptSize: 30000, Input: 10, CacheWrite: 29990, Output: 200},
		{TS: "2026-07-01T10:01:00.000Z", Model: "claude-opus-4-8", PromptSize: 32000, Input: 10, CacheRead: 30000, CacheWrite: 1990, Output: 300, Compaction: true},
		{TS: "2026-07-01T12:30:00.000Z", Model: "claude-opus-4-8", PromptSize: 33000, Input: 33000, Output: 100,
			Event: &analyze.Event{Cause: "ttl_expiry", Shortfall: 32000, TS: "2026-07-01T12:30:00.000Z"}},
	}
	return &SessionView{
		Meta:   store.SessionMeta{ID: 1, Key: "abc-123", Project: "proj", Title: "Test session"},
		Models: []string{"claude-opus-4-8"},
		Start:  pts[0].TS, End: pts[2].TS,
		Points: pts,
		Buckets: []Bucket{
			{StartIdx: 0, EndIdx: 1, ByKind: map[string]int64{"tool_result": 5000, "assistant_text": 800}},
			{StartIdx: 2, EndIdx: 2, ByKind: map[string]int64{"meta_text": 1200}},
		},
		Events:    []analyze.Event{*pts[2].Event},
		Reuse:     0.94,
		Shortfall: 32000,
		TotalOut:  600, TotalRead: 30000, TotalWrit: 31980, NewEst: 7000,
		Kinds: []store.AttrRow{
			{Group: "tool_result", Blocks: 3, EstTokens: 5000},
			{Group: "assistant_text", Blocks: 2, EstTokens: 800},
			{Group: "meta_text", Blocks: 1, EstTokens: 1200},
		},
		TopTools: []store.AttrRow{{Group: "Bash", Blocks: 2, EstTokens: 4000}},
		TopFiles: []store.AttrRow{{Group: "/home/user/proj/main.go", Blocks: 1, EstTokens: 1000}},
	}
}

func TestRenderSessionHTML(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderSessionHTML(&buf, testView()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"<svg id=\"c1\"",             // timeline chart
		"<svg id=\"c2\"",             // composition chart
		"ttl expiry",                 // event legend
		"class=\"compact\"",          // compaction marker
		"class=\"ev ev-ttl\"",        // event marker with status class
		"tool_result",                // composition table (relief rule)
		"warm-prefix reuse",          // stat tile
		"Bash",                       // top tools
		"prefers-color-scheme: dark", // dark mode selected, not flipped
		"data-theme=\"dark\"",        // theme-toggle scope
	} {
		if !strings.Contains(out, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	if strings.Contains(out, "<script src=") || strings.Contains(out, "http://") || strings.Contains(out, "https://") {
		t.Error("HTML must be self-contained: found external reference")
	}
}

func TestRenderSessionTerm(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderSessionTerm(&buf, testView()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"proj — Test session",
		"reuse 94.0%",
		"context size per request",
		"ttl_expiry",
		"composition:",
		"Bash",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal view missing %q in:\n%s", want, out)
		}
	}
	// Marker row must flag both the compaction and the event.
	if !strings.Contains(out, "C") || !strings.Contains(out, "!") {
		t.Errorf("marker row missing C/! markers:\n%s", out)
	}
}
