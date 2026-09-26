package serve

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/erewhon/tokenator/internal/analyze"
	"github.com/erewhon/tokenator/internal/report"
	"github.com/erewhon/tokenator/internal/store"
)

// JSON API — the same data the four HTML pages render, for the router
// dashboard's native Tokens tab (phase 2 of the unified web UI). Every
// endpoint is built on the same query functions as its page, so the two
// cannot drift. Envelope: {"data": …} on success, {"error": "…"} otherwise;
// no auth here either — the same rules as the HTML (loopback, or behind
// the front door / the router's proxy). Responses are never cached.
//
//	GET /api/sessions?q=&project=&since=&limit=
//	GET /api/session/{key}
//	GET /api/session/{key}/transcript?q=&offset=&limit=
//	GET /api/model/{name}?limit=

func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions", s.apiSessions)
	mux.HandleFunc("GET /api/session/{key}", s.apiSession)
	mux.HandleFunc("GET /api/session/{key}/transcript", s.apiTranscript)
	mux.HandleFunc("GET /api/model/{name}", s.apiModel)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeData(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}
func writeError(w http.ResponseWriter, err *httpErr) {
	writeJSON(w, err.status(), map[string]any{"error": err.Error()})
}

func intParam(r *http.Request, name string, def int) int {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// --- /api/sessions ---

type apiHit struct {
	Anchor  int    `json:"anchor"`
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	Tool    string `json:"tool,omitempty"`
	Snippet string `json:"snippet"`
}

type apiSessionRow struct {
	Key        string   `json:"key"`
	Slug       string   `json:"slug,omitempty"`
	Project    string   `json:"project"`
	Title      string   `json:"title"`
	Agent      string   `json:"agent,omitempty"`
	SourceKind string   `json:"source_kind"`
	StartedAt  string   `json:"started_at"`
	EndedAt    string   `json:"ended_at"`
	Requests   int64    `json:"requests"`
	Input      int64    `json:"input"`
	Output     int64    `json:"output"`
	CacheRead  int64    `json:"cache_read"`
	CacheWrite int64    `json:"cache_write"`
	Matches    int      `json:"matches,omitempty"`
	TitleMatch bool     `json:"title_match,omitempty"`
	Hits       []apiHit `json:"hits,omitempty"`
	ScanErr    string   `json:"scan_err,omitempty"`
}

type apiSessions struct {
	Query     string          `json:"query"`
	Project   string          `json:"project"`
	Since     string          `json:"since"`
	Projects  []string        `json:"projects"`
	Rows      []apiSessionRow `json:"rows"`
	Searched  bool            `json:"searched"`
	Scanned   int             `json:"scanned,omitempty"`
	ScanMS    int64           `json:"scan_ms,omitempty"`
	Limit     int             `json:"limit,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

func (s *Server) apiSessions(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildIndex(r.FormValue("q"), r.FormValue("project"), r.FormValue("since"), intParam(r, "limit", 0))
	if err != nil {
		writeError(w, err)
		return
	}
	out := apiSessions{Query: data.Query, Project: data.Project, Since: data.Since, Projects: data.Projects,
		Rows: []apiSessionRow{}, Searched: data.Searched, Scanned: data.Scanned, ScanMS: data.ScanMS, Limit: data.Limit, Truncated: data.Truncated}
	if out.Projects == nil {
		out.Projects = []string{}
	}
	for _, row := range data.Rows {
		ar := apiSessionRow{
			Key: row.Key, Slug: row.Slug, Project: row.Project, Title: row.Title, Agent: row.Agent,
			SourceKind: row.SourceKind, StartedAt: row.StartedAt, EndedAt: row.EndedAt,
			Requests: row.Requests, Input: row.Input, Output: row.Output, CacheRead: row.CacheRead, CacheWrite: row.CacheWrite,
			Matches: row.Matches, TitleMatch: row.TitleMatch, ScanErr: row.ScanErr,
		}
		for _, h := range row.Hits {
			ar.Hits = append(ar.Hits, apiHit{Anchor: h.Anchor, TS: h.TS, Kind: h.Kind, Tool: h.Tool, Snippet: h.Raw})
		}
		out.Rows = append(out.Rows, ar)
	}
	writeData(w, out)
}

// --- /api/session/{key} ---

type apiMeta struct {
	Key     string `json:"key"`
	Slug    string `json:"slug,omitempty"`
	Project string `json:"project"`
	Title   string `json:"title"`
	Agent   string `json:"agent,omitempty"`
}

type apiTotals struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

type apiPoint struct {
	TS         string         `json:"ts"`
	Model      string         `json:"model"`
	PromptSize int64          `json:"prompt_size"`
	Input      int64          `json:"input"`
	CacheRead  int64          `json:"cache_read"`
	CacheWrite int64          `json:"cache_write"`
	Output     int64          `json:"output"`
	Event      *apiCacheEvent `json:"event,omitempty"`
	Compaction bool           `json:"compaction,omitempty"`
}

type apiCacheEvent struct {
	TS        string `json:"ts"`
	Model     string `json:"model"`
	Cause     string `json:"cause"`
	Expected  int64  `json:"expected"`
	CacheRead int64  `json:"cache_read"`
	Shortfall int64  `json:"shortfall"`
}

type apiBucket struct {
	StartIdx int              `json:"start_idx"`
	EndIdx   int              `json:"end_idx"`
	ByKind   map[string]int64 `json:"by_kind"`
}

type apiAttr struct {
	Group     string `json:"group"`
	Blocks    int64  `json:"blocks"`
	Errors    int64  `json:"errors"`
	Bytes     int64  `json:"bytes"`
	EstTokens int64  `json:"est_tokens"`
}

type apiSessionView struct {
	Meta       apiMeta         `json:"meta"`
	Models     []string        `json:"models"`
	Start      string          `json:"start"`
	End        string          `json:"end"`
	Totals     apiTotals       `json:"totals"`
	Reuse      float64         `json:"reuse"`     // -1 when no cache expectation accrued
	Shortfall  int64           `json:"shortfall"` // tokens read from cache below expectation
	NewEst     int64           `json:"new_est"`   // est tokens of extracted blocks
	Points     []apiPoint      `json:"points"`    // one per request
	Buckets    []apiBucket     `json:"buckets"`   // new content by kind per request span
	Events     []apiCacheEvent `json:"events"`    // cache invalidations
	TopTools   []apiAttr       `json:"top_tools"`
	TopFiles   []apiAttr       `json:"top_files"`
	Kinds      []apiAttr       `json:"kinds"`
	SourceKind string          `json:"source_kind"`
}

func attrs(rows []store.AttrRow) []apiAttr {
	out := make([]apiAttr, 0, len(rows))
	for _, a := range rows {
		out = append(out, apiAttr{Group: a.Group, Blocks: a.Blocks, Errors: a.Errors, Bytes: a.Bytes, EstTokens: a.EstTokens})
	}
	return out
}

func cacheEvent(e *analyze.Event) *apiCacheEvent {
	if e == nil {
		return nil
	}
	return &apiCacheEvent{TS: e.TS, Model: e.Model, Cause: e.Cause, Expected: e.Expected, CacheRead: e.CacheRead, Shortfall: e.Shortfall}
}

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	view, err := report.BuildSessionView(s.Store, r.PathValue("key"))
	if err != nil {
		writeError(w, notFound(err))
		return
	}
	kind, _, _ := s.Store.SessionSource(view.Meta.ID)
	out := apiSessionView{
		Meta:   apiMeta{Key: view.Meta.Key, Slug: view.Meta.Slug, Project: view.Meta.Project, Title: view.Meta.Title, Agent: view.Meta.Agent},
		Models: view.Models, Start: view.Start, End: view.End,
		Totals: apiTotals{Input: view.TotalIn, Output: view.TotalOut, CacheRead: view.TotalRead, CacheWrite: view.TotalWrit},
		Reuse:  view.Reuse, Shortfall: view.Shortfall, NewEst: view.NewEst,
		Points: make([]apiPoint, 0, len(view.Points)), Buckets: make([]apiBucket, 0, len(view.Buckets)), Events: make([]apiCacheEvent, 0, len(view.Events)),
		TopTools: attrs(view.TopTools), TopFiles: attrs(view.TopFiles), Kinds: attrs(view.Kinds),
		SourceKind: kind,
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	for _, p := range view.Points {
		out.Points = append(out.Points, apiPoint{TS: p.TS, Model: p.Model, PromptSize: p.PromptSize, Input: p.Input,
			CacheRead: p.CacheRead, CacheWrite: p.CacheWrite, Output: p.Output, Event: cacheEvent(p.Event), Compaction: p.Compaction})
	}
	for _, b := range view.Buckets {
		out.Buckets = append(out.Buckets, apiBucket{StartIdx: b.StartIdx, EndIdx: b.EndIdx, ByKind: b.ByKind})
	}
	for i := range view.Events {
		out.Events = append(out.Events, *cacheEvent(&view.Events[i]))
	}
	writeData(w, out)
}

// --- /api/session/{key}/transcript ---

type apiEntry struct {
	Idx     int    `json:"idx"`
	TS      string `json:"ts"`
	Role    string `json:"role"`
	Kind    string `json:"kind"`
	Tool    string `json:"tool,omitempty"`
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
	Matched bool   `json:"matched,omitempty"`
}

type apiTranscript struct {
	Meta    apiMeta    `json:"meta"`
	Query   string     `json:"query,omitempty"`
	Total   int        `json:"total"`
	Offset  int        `json:"offset"`
	Limit   int        `json:"limit"`
	Entries []apiEntry `json:"entries"`
	Err     string     `json:"err,omitempty"` // transcript files unavailable
}

// defaultTranscriptPage bounds one response: transcripts run to thousands
// of entries and megabytes of text.
const defaultTranscriptPage = 500

func (s *Server) apiTranscript(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	meta, entries, loadErr, err := s.loadTranscript(r.PathValue("key"))
	if err != nil {
		writeError(w, err)
		return
	}
	offset := intParam(r, "offset", 0)
	limit := intParam(r, "limit", defaultTranscriptPage)
	if limit == 0 {
		limit = defaultTranscriptPage
	}
	out := apiTranscript{
		Meta:  apiMeta{Key: meta.Key, Slug: meta.Slug, Project: meta.Project, Title: meta.Title, Agent: meta.Agent},
		Query: q, Total: len(entries), Offset: offset, Limit: limit, Entries: []apiEntry{}, Err: loadErr,
	}
	lq := strings.ToLower(q)
	for i := offset; i < len(entries) && i < offset+limit; i++ {
		e := entries[i]
		out.Entries = append(out.Entries, apiEntry{Idx: e.Idx, TS: e.TS, Role: e.Role, Kind: e.Kind, Tool: e.Tool, Text: e.Text,
			IsError: e.IsError, Matched: lq != "" && strings.Contains(strings.ToLower(e.Text), lq)})
	}
	writeData(w, out)
}

// --- /api/model/{name} ---

type apiModelRow struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Project  string `json:"project"`
	Agent    string `json:"agent,omitempty"`
	LastTS   string `json:"last_ts"`
	Requests int64  `json:"requests"`
	Input    int64  `json:"input"`
	Output   int64  `json:"output"`
	Via      string `json:"via"` // transcript | router
}

type apiModel struct {
	Name   string        `json:"name"`
	Rows   []apiModelRow `json:"rows"`
	Totals struct {
		Requests int64 `json:"requests"`
		Input    int64 `json:"input"`
		Output   int64 `json:"output"`
		Unpaired int64 `json:"unpaired"`
	} `json:"totals"`
	Limit   int  `json:"limit"`
	Trimmed bool `json:"trimmed"`
}

func (s *Server) apiModel(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildModel(r.PathValue("name"), intParam(r, "limit", 0))
	if err != nil {
		writeError(w, err)
		return
	}
	out := apiModel{Name: data.Name, Rows: []apiModelRow{}, Limit: data.Limit, Trimmed: data.Trimmed}
	out.Totals.Requests, out.Totals.Input, out.Totals.Output, out.Totals.Unpaired = data.Totals.Requests, data.Totals.Input, data.Totals.Output, data.Totals.Unpaired
	for _, row := range data.Rows {
		out.Rows = append(out.Rows, apiModelRow{Key: row.Key, Title: row.Title, Project: row.Project, Agent: row.Agent,
			LastTS: row.LastTS, Requests: row.Requests, Input: row.Input, Output: row.Output, Via: row.Via})
	}
	writeData(w, out)
}
