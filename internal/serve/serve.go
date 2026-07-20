// Package serve is `tokenator serve`: a localhost web UI over the tokenator
// database — a session browser with content search, per-session transcript
// reading, and links into the existing session profile page.
//
// Search is two-stage (the database stores no content): store filters narrow
// the candidate sessions, then internal/transcript scans the harness's own
// files for the query. The scan is bounded by ScanLimit sessions per search;
// the page reports how many were scanned so truncation is never silent.
package serve

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erewhon/tokenator/internal/report"
	"github.com/erewhon/tokenator/internal/store"
	"github.com/erewhon/tokenator/internal/transcript"
)

type Server struct {
	Store *store.Store
	// ScanLimit bounds how many sessions a content search will scan
	// (newest first). 0 means 80.
	ScanLimit int
}

func (s *Server) scanLimit() int {
	if s.ScanLimit <= 0 {
		return 80
	}
	return s.ScanLimit
}

// Handler returns the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /session/{key}", s.handleSession)
	mux.HandleFunc("GET /session/{key}/transcript", s.handleTranscript)
	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	log.Printf("session browser on http://%s", addr)
	return http.ListenAndServe(addr, s.Handler())
}

// --- index: browse + search ---

// sessionRow is one row of the browser table, with optional search results.
type sessionRow struct {
	store.SessionListRow
	When       string // display timestamp (day precision)
	TotalToks  string
	OutToks    string
	Matches    int
	Hits       []hitView
	TitleMatch bool // metadata matched but content was not scanned/matched
	ScanErr    string
}

// hitView is a search hit prepared for the template: palette slot resolved
// and the snippet pre-highlighted.
type hitView struct {
	Anchor  int
	TS      string
	Kind    string
	Slot    int
	Tool    string
	Snippet template.HTML
}

type indexData struct {
	Query    string
	Project  string
	Since    string
	Projects []string
	Rows     []sessionRow
	Searched bool
	Scanned  int
	ScanMS   int64
	Limit    int
	Truncated bool
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	project := r.FormValue("project")
	since := r.FormValue("since")
	sinceTS, err := parseSince(since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	projects, err := s.Store.Projects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := indexData{Query: q, Project: project, Since: since, Projects: projects}
	if q == "" {
		list, err := s.Store.SessionList(store.SessionFilter{Project: project, SinceTS: sinceTS, Limit: 100})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Rows = decorate(list)
	} else {
		data.Searched = true
		data.Limit = s.scanLimit()
		list, err := s.Store.SessionList(store.SessionFilter{Project: project, SinceTS: sinceTS, Limit: data.Limit})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Scanned = len(list)
		data.Truncated = len(list) == data.Limit
		start := time.Now()
		data.Rows = s.searchSessions(list, q)
		data.ScanMS = time.Since(start).Milliseconds()
	}
	render(w, indexTmpl, data)
}

// searchSessions content-scans the candidates in parallel, keeping rows that
// match by content or metadata, in the candidates' (recency) order.
func (s *Server) searchSessions(list []store.SessionListRow, q string) []sessionRow {
	rows := decorate(list)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range rows {
		wg.Add(1)
		go func(row *sessionRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			hits, err := transcript.Search(row.SourceKind, row.SourceRoot, row.Key, q, 3)
			if err != nil {
				row.ScanErr = err.Error()
				return
			}
			row.Matches = hits.Matches
			for _, h := range hits.Hits {
				snip, _ := highlight(h.Snippet, q)
				row.Hits = append(row.Hits, hitView{
					Anchor: h.EntryIdx, TS: minuteTS(h.TS), Kind: h.Kind,
					Slot: kindSlots[h.Kind], Tool: h.Tool, Snippet: snip,
				})
			}
		}(&rows[i])
	}
	wg.Wait()
	lq := strings.ToLower(q)
	var out []sessionRow
	for _, row := range rows {
		row.TitleMatch = strings.Contains(strings.ToLower(row.Title), lq) ||
			strings.Contains(strings.ToLower(row.Slug), lq)
		if row.Matches > 0 || row.TitleMatch {
			out = append(out, row)
		}
	}
	return out
}

func decorate(list []store.SessionListRow) []sessionRow {
	rows := make([]sessionRow, len(list))
	for i, l := range list {
		rows[i] = sessionRow{
			SessionListRow: l,
			When:           day(l.EndedAt, l.StartedAt),
			TotalToks:      abbrev(l.Input + l.CacheRead + l.CacheWrite + l.Output),
			OutToks:        abbrev(l.Output),
		}
	}
	return rows
}

// --- session profile (existing page + nav) ---

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	view, err := report.BuildSessionView(s.Store, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	nav := fmt.Sprintf(
		`<p class="meta" style="max-width:960px;margin:0 auto 12px"><a href="/">&larr; sessions</a> &middot; <a href="/session/%s/transcript">transcript</a></p>`,
		url.PathEscape(view.Meta.Key))
	if err := report.RenderSessionHTMLNav(w, view, template.HTML(nav)); err != nil {
		log.Printf("serve: render session %s: %v", key, err)
	}
}

// --- transcript view ---

type transcriptEntry struct {
	Idx      int
	TS       string // clock time only, day changes carry the date
	Kind     string
	Slot     int // palette slot for the kind chip
	Tool     string
	Body     template.HTML
	Open     bool // render <details> expanded
	Collapse bool // body behind <details>
	IsError  bool
	Chars    int
}

type transcriptData struct {
	Meta    store.SessionMeta
	Query   string
	Entries []transcriptEntry
	Total   int
	Err     string
}

// kindSlots mirrors report's fixed palette assignment (s1..s7).
var kindSlots = map[string]int{
	"tool_result": 1, "assistant_text": 2, "thinking": 3,
	"user_text": 4, "meta_text": 5, "tool_use": 6, "image": 7,
}

const collapseOver = 1200 // chars; longer bodies start folded

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	q := strings.TrimSpace(r.FormValue("q"))
	meta, err := s.Store.SessionByPrefix(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	data := transcriptData{Meta: meta, Query: q}
	kind, root, err := s.Store.SessionSource(meta.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	entries, err := transcript.Load(kind, root, meta.Key)
	if err != nil {
		data.Err = err.Error()
	}
	data.Total = len(entries)
	prevDay := ""
	for _, e := range entries {
		te := transcriptEntry{
			Idx: e.Idx, Kind: e.Kind, Slot: kindSlots[e.Kind],
			Tool: e.Tool, IsError: e.IsError, Chars: len(e.Text),
		}
		te.TS, prevDay = clockTime(e.TS, prevDay)
		matched := false
		te.Body, matched = highlight(e.Text, q)
		te.Collapse = len(e.Text) > collapseOver ||
			((e.Kind == "tool_result" || e.Kind == "meta_text") && len(e.Text) > 200)
		te.Open = matched
		data.Entries = append(data.Entries, te)
	}
	render(w, transcriptTmpl, data)
}

// highlight escapes text and wraps case-insensitive query matches in <mark>.
func highlight(text, q string) (template.HTML, bool) {
	if q == "" || text == "" {
		return template.HTML(template.HTMLEscapeString(text)), false
	}
	lt, lq := strings.ToLower(text), strings.ToLower(q)
	var b strings.Builder
	pos, matched := 0, false
	for {
		i := strings.Index(lt[pos:], lq)
		if i < 0 {
			break
		}
		i += pos
		b.WriteString(template.HTMLEscapeString(text[pos:i]))
		b.WriteString("<mark>")
		b.WriteString(template.HTMLEscapeString(text[i : i+len(q)]))
		b.WriteString("</mark>")
		pos = i + len(q)
		matched = true
	}
	b.WriteString(template.HTMLEscapeString(text[pos:]))
	return template.HTML(b.String()), matched
}

// --- helpers ---

func render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		log.Printf("serve: render: %v", err)
	}
}

// parseSince turns "7d", "24h", "30m" into an RFC3339 UTC lower bound.
func parseSince(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "all" {
		return "", nil
	}
	var d time.Duration
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return "", fmt.Errorf("bad since %q", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return "", fmt.Errorf("bad since %q", s)
		}
	}
	return time.Now().UTC().Add(-d).Format(time.RFC3339), nil
}

// minuteTS trims an RFC3339 timestamp to minute precision for display.
func minuteTS(ts string) string {
	if len(ts) >= 16 {
		return ts[:10] + " " + ts[11:16]
	}
	return ts
}

// day extracts YYYY-MM-DD from the first non-empty timestamp.
func day(ts ...string) string {
	for _, t := range ts {
		if len(t) >= 10 {
			return t[:10]
		}
	}
	return ""
}

// clockTime renders HH:MM:SS, prefixing the date when the day changes.
func clockTime(ts, prevDay string) (display, newDay string) {
	if len(ts) < 19 {
		return ts, prevDay
	}
	d, clock := ts[:10], ts[11:19]
	if d != prevDay {
		return d + " " + clock, d
	}
	return clock, d
}

// abbrev renders token counts compactly (12.3M, 456k, 789).
func abbrev(v int64) string {
	switch {
	case v >= 10_000_000:
		return fmt.Sprintf("%.0fM", float64(v)/1e6)
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(v)/1e6)
	case v >= 10_000:
		return fmt.Sprintf("%.0fk", float64(v)/1e3)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", float64(v)/1e3)
	default:
		return fmt.Sprintf("%d", v)
	}
}
