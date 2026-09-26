package serve

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// getJSON fetches an API path and decodes the envelope.
func getJSON(t *testing.T, srv *Server, path string, wantStatus int) (data map[string]any, errMsg string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("GET %s = %d, want %d: %s", path, rec.Code, wantStatus, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s: content-type %q", path, ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s: API responses must be no-store", path)
	}
	var env struct {
		Data  map[string]any `json:"data"`
		Error string         `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, rec.Body.String())
	}
	return env.Data, env.Error
}

func TestAPISessionsListAndSearch(t *testing.T) {
	srv := fixtureServer(t)
	d, _ := getJSON(t, srv, "/api/sessions", 200)
	rows := d["rows"].([]any)
	if len(rows) != 1 || d["searched"] != false {
		t.Fatalf("list: %+v", d)
	}
	row := rows[0].(map[string]any)
	if row["key"] != "abc" || row["project"] != "widgets" || row["requests"].(float64) != 1 || row["input"].(float64) != 12 || row["cache_read"].(float64) != 1000 || row["source_kind"] != "claude_code" {
		t.Errorf("row: %+v", row)
	}
	if projects := d["projects"].([]any); len(projects) != 1 || projects[0] != "widgets" {
		t.Errorf("projects: %v", d["projects"])
	}

	d, _ = getJSON(t, srv, "/api/sessions?q=flaky", 200)
	rows = d["rows"].([]any)
	if d["searched"] != true || len(rows) != 1 || d["scanned"].(float64) != 1 {
		t.Fatalf("search: %+v", d)
	}
	row = rows[0].(map[string]any)
	if row["matches"].(float64) < 1 {
		t.Fatalf("search row should carry matches: %+v", row)
	}
	hit := row["hits"].([]any)[0].(map[string]any)
	if !strings.Contains(strings.ToLower(hit["snippet"].(string)), "flaky") || strings.Contains(hit["snippet"].(string), "<mark>") {
		t.Errorf("snippet must be plain text containing the query: %+v", hit)
	}
	if hit["kind"] != "user_text" {
		t.Errorf("hit kind: %+v", hit)
	}

	d, _ = getJSON(t, srv, "/api/sessions?q=nowhere", 200)
	if len(d["rows"].([]any)) != 0 {
		t.Errorf("no-match search should return an empty rows array, got %+v", d["rows"])
	}
	if _, e := getJSON(t, srv, "/api/sessions?since=bogus", 400); !strings.Contains(e, "bad since") {
		t.Errorf("bad since: %q", e)
	}
}

func TestAPISession(t *testing.T) {
	srv := fixtureServer(t)
	d, _ := getJSON(t, srv, "/api/session/abc", 200)
	meta := d["meta"].(map[string]any)
	if meta["key"] != "abc" || meta["title"] != "fix flaky widget test" || meta["project"] != "widgets" {
		t.Errorf("meta: %+v", meta)
	}
	tot := d["totals"].(map[string]any)
	if tot["input"].(float64) != 12 || tot["output"].(float64) != 40 || tot["cache_read"].(float64) != 1000 {
		t.Errorf("totals: %+v", tot)
	}
	if pts := d["points"].([]any); len(pts) != 1 || pts[0].(map[string]any)["model"] != "claude-sonnet-5" {
		t.Errorf("points: %+v", d["points"])
	}
	if models := d["models"].([]any); len(models) != 1 || models[0] != "claude-sonnet-5" {
		t.Errorf("models: %+v", d["models"])
	}
	if d["source_kind"] != "claude_code" {
		t.Errorf("source_kind: %v", d["source_kind"])
	}
	for _, k := range []string{"buckets", "events", "top_tools", "top_files", "kinds"} {
		if _, ok := d[k].([]any); !ok {
			t.Errorf("%s must be an array (possibly empty), got %T", k, d[k])
		}
	}
	// Prefix resolves like the page does.
	if d, _ := getJSON(t, srv, "/api/session/ab", 200); d["meta"].(map[string]any)["key"] != "abc" {
		t.Error("prefix lookup")
	}
	if _, e := getJSON(t, srv, "/api/session/zzz", 404); e == "" {
		t.Error("unknown session must be a 404 with an error message")
	}
}

func TestAPITranscriptPagesAndHighlights(t *testing.T) {
	srv := fixtureServer(t)
	d, _ := getJSON(t, srv, "/api/session/abc/transcript?q=widget", 200)
	if d["total"].(float64) != 4 || d["offset"].(float64) != 0 || d["limit"].(float64) != float64(defaultTranscriptPage) {
		t.Fatalf("transcript envelope: %+v", d)
	}
	entries := d["entries"].([]any)
	if len(entries) != 4 {
		t.Fatalf("entries: %d", len(entries))
	}
	first := entries[0].(map[string]any)
	if first["kind"] != "user_text" || first["role"] != "user" || first["matched"] != true || !strings.Contains(first["text"].(string), "flaky widget") {
		t.Errorf("first entry: %+v", first)
	}
	kinds := []string{}
	matched := 0
	for _, e := range entries {
		m := e.(map[string]any)
		kinds = append(kinds, m["kind"].(string))
		if m["matched"] == true {
			matched++
		}
	}
	if strings.Join(kinds, ",") != "user_text,assistant_text,tool_use,tool_result" {
		t.Errorf("kinds: %v", kinds)
	}
	if matched != 4 { // prompt, reply, tool input (widget_test.go) and the tool result (TestWidget)
		t.Errorf("matched = %d", matched)
	}
	// Paging.
	d, _ = getJSON(t, srv, "/api/session/abc/transcript?offset=2&limit=1", 200)
	if es := d["entries"].([]any); len(es) != 1 || es[0].(map[string]any)["idx"].(float64) != 2 || d["total"].(float64) != 4 {
		t.Errorf("page: %+v", d)
	}
	d, _ = getJSON(t, srv, "/api/session/abc/transcript?offset=99", 200)
	if es := d["entries"].([]any); len(es) != 0 {
		t.Errorf("past the end should be an empty array, got %v", es)
	}
}

func TestAPIModel(t *testing.T) {
	srv := fixtureServer(t)
	seedModelTraffic(t, srv)
	d, _ := getJSON(t, srv, "/api/model/qwen38", 200)
	rows := d["rows"].([]any)
	if d["name"] != "qwen38" || len(rows) != 1 {
		t.Fatalf("model: %+v", d)
	}
	row := rows[0].(map[string]any)
	if row["key"] != "ses_def" || row["via"] != "router" || row["title"] != "gadget refactor" {
		t.Errorf("row: %+v", row)
	}
	tot := d["totals"].(map[string]any)
	if tot["requests"].(float64) != 2 || tot["unpaired"].(float64) != 1 {
		t.Errorf("totals: %+v", tot)
	}
	d, _ = getJSON(t, srv, "/api/model/or%2Fminimax-m3", 200)
	if d["name"] != "or/minimax-m3" || len(d["rows"].([]any)) != 0 {
		t.Errorf("unknown model: %+v", d)
	}
}

// The API lives under the same base path as the pages.
func TestAPIUnderBasePath(t *testing.T) {
	srv := fixtureServer(t)
	srv.BasePath = "/tokens"
	if d, _ := getJSON(t, srv, "/tokens/api/session/abc", 200); d["meta"].(map[string]any)["key"] != "abc" {
		t.Error("API under --base-path")
	}
}
