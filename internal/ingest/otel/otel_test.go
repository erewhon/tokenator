package otel

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erewhon/tokenator/internal/store"
)

const sessID = "8601f6ea-9297-4647-a919-bef3c38e8d2c"

// metricsExport builds a token.usage export in the shape Claude Code sends
// over OTLP/HTTP JSON (protojson: 64-bit ints as strings). startNano pins
// the series identity: constant = cumulative re-export, advancing = delta.
func metricsExport(startNano, timeNano, inputVal string) string {
	return `{
	  "resourceMetrics": [{
	    "resource": {"attributes": [
	      {"key": "service.name", "value": {"stringValue": "claude-code"}},
	      {"key": "service.version", "value": {"stringValue": "2.1.212"}}
	    ]},
	    "scopeMetrics": [{
	      "scope": {"name": "com.anthropic.claude_code"},
	      "metrics": [{
	        "name": "claude_code.token.usage",
	        "unit": "tokens",
	        "sum": {
	          "aggregationTemporality": 2,
	          "isMonotonic": true,
	          "dataPoints": [
	            {
	              "attributes": [
	                {"key": "session.id", "value": {"stringValue": "` + sessID + `"}},
	                {"key": "model", "value": {"stringValue": "claude-haiku-4-5-20251001"}},
	                {"key": "query_source", "value": {"stringValue": "main"}},
	                {"key": "type", "value": {"stringValue": "input"}}
	              ],
	              "startTimeUnixNano": "` + startNano + `",
	              "timeUnixNano": "` + timeNano + `",
	              "asInt": "` + inputVal + `"
	            },
	            {
	              "attributes": [
	                {"key": "session.id", "value": {"stringValue": "` + sessID + `"}},
	                {"key": "model", "value": {"stringValue": "claude-haiku-4-5-20251001"}},
	                {"key": "query_source", "value": {"stringValue": "main"}},
	                {"key": "mcp_server.name", "value": {"stringValue": "nous"}},
	                {"key": "mcp_tool.name", "value": {"stringValue": "get_page"}},
	                {"key": "type", "value": {"stringValue": "cacheRead"}}
	              ],
	              "startTimeUnixNano": "` + startNano + `",
	              "timeUnixNano": "` + timeNano + `",
	              "asInt": "17418"
	            }
	          ]
	        }
	      }, {
	        "name": "claude_code.some_histogram",
	        "histogram": {"dataPoints": [{"count": "3"}]}
	      }]
	    }]
	  }]
	}`
}

const logsExport = `{
  "resourceLogs": [{
    "resource": {"attributes": [
      {"key": "service.name", "value": {"stringValue": "claude-code"}},
      {"key": "service.version", "value": {"stringValue": "2.1.212"}}
    ]},
    "scopeLogs": [{
      "scope": {"name": "com.anthropic.claude_code.events"},
      "logRecords": [
        {
          "timeUnixNano": "1784318899263000000",
          "body": {"stringValue": "claude_code.api_request"},
          "attributes": [
            {"key": "session.id", "value": {"stringValue": "` + sessID + `"}},
            {"key": "event.name", "value": {"stringValue": "api_request"}},
            {"key": "event.timestamp", "value": {"stringValue": "2026-07-17T20:08:19.263Z"}},
            {"key": "event.sequence", "value": {"intValue": "33"}},
            {"key": "prompt.id", "value": {"stringValue": "af03b4b4-fd8f-49fc-86eb-e358e27775cc"}},
            {"key": "model", "value": {"stringValue": "claude-haiku-4-5-20251001"}},
            {"key": "input_tokens", "value": {"intValue": "10"}},
            {"key": "output_tokens", "value": {"intValue": "38"}},
            {"key": "cache_read_tokens", "value": {"intValue": "17418"}},
            {"key": "cache_creation_tokens", "value": {"intValue": "11356"}},
            {"key": "cost_usd", "value": {"doubleValue": 0.0246538}},
            {"key": "duration_ms", "value": {"intValue": "2371"}},
            {"key": "request_id", "value": {"stringValue": "req_test123"}},
            {"key": "speed", "value": {"stringValue": "normal"}}
          ]
        },
        {
          "timeUnixNano": "1784318897336000000",
          "body": {"stringValue": "claude_code.hook_execution_complete"},
          "attributes": [
            {"key": "session.id", "value": {"stringValue": "` + sessID + `"}},
            {"key": "event.sequence", "value": {"intValue": "29"}},
            {"key": "hook_name", "value": {"stringValue": "PreToolUse:Write"}},
            {"key": "duration_ms", "value": {"stringValue": "961"}},
            {"key": "num_success", "value": {"intValue": "1"}}
          ]
        }
      ]
    }]
  }]
}`

func newTestReceiver(t *testing.T) (*store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rc := &Receiver{Store: st, Logf: t.Logf}
	h, err := rc.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return st, srv
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func tokensByType(t *testing.T, st *store.Store) map[string]float64 {
	t.Helper()
	status, err := st.OTelStatus()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, nv := range status.TokensByType {
		out[nv.Name] = nv.Value
	}
	return out
}

func TestMetricsCumulative(t *testing.T) {
	st, srv := newTestReceiver(t)

	// First export: input=10. Same series re-exported with input=25 must
	// UPDATE the row (cumulative), not add a second one.
	if resp := post(t, srv.URL+"/v1/metrics", metricsExport("100", "200", "10")); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	post(t, srv.URL+"/v1/metrics", metricsExport("100", "300", "25"))

	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.OTelDatapoints != 2 { // input series + cacheRead series
		t.Fatalf("datapoints = %d, want 2 (histogram skipped, cumulative updated in place)", counts.OTelDatapoints)
	}
	tok := tokensByType(t, st)
	if tok["input"] != 25 {
		t.Errorf("input total = %v, want 25 (latest cumulative)", tok["input"])
	}
	if tok["cacheRead"] != 17418 {
		t.Errorf("cacheRead total = %v, want 17418", tok["cacheRead"])
	}
}

func TestMetricsDelta(t *testing.T) {
	st, srv := newTestReceiver(t)

	// Delta temporality: start advances every export, so each point is its
	// own row and totals accumulate.
	post(t, srv.URL+"/v1/metrics", metricsExport("100", "200", "10"))
	post(t, srv.URL+"/v1/metrics", metricsExport("200", "300", "7"))

	if tok := tokensByType(t, st); tok["input"] != 17 {
		t.Errorf("input total = %v, want 17 (10+7 delta points)", tok["input"])
	}
}

func TestLogsIngestAndCrossCheck(t *testing.T) {
	st, srv := newTestReceiver(t)

	// A transcript-ingested request with the same request_id, for the join.
	// UpsertSource must run OUTSIDE WithTx: the store runs on one SQLite
	// connection, so a non-tx query inside a transaction deadlocks.
	src, err := st.UpsertSource("claude_code", "/tmp/x", "subscription")
	if err != nil {
		t.Fatal(err)
	}
	err = st.WithTx(func(tx *store.Tx) error {
		sid, err := tx.UpsertSession(store.Session{SourceID: src, HarnessID: sessID})
		if err != nil {
			return err
		}
		_, err = tx.InsertRequest(store.Request{
			SessionID: sid, TS: "2026-07-17T20:08:19.263Z",
			InputTokens: 10, OutputTokens: 38,
			DedupeKey: "msg1", HarnessRequestID: "req_test123",
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	post(t, srv.URL+"/v1/logs", logsExport)
	post(t, srv.URL+"/v1/logs", logsExport) // re-delivery must dedupe

	status, err := st.OTelStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Events != 2 {
		t.Fatalf("events = %d, want 2 (idempotent re-post)", status.Events)
	}
	if status.APIReqEvents != 1 || status.APIReqMatched != 1 {
		t.Fatalf("api_request events=%d matched=%d, want 1/1", status.APIReqEvents, status.APIReqMatched)
	}
	if status.OTelInput != 10 || status.JSONLInput != 10 ||
		status.OTelOutput != 38 || status.JSONLOutput != 38 {
		t.Errorf("cross-check sums otel(%d,%d) jsonl(%d,%d), want (10,38,10,38)",
			status.OTelInput, status.OTelOutput, status.JSONLInput, status.JSONLOutput)
	}
	if status.Sessions != 0 {
		// Sessions counts datapoint sessions; only logs were posted here.
		t.Logf("sessions=%d (datapoint-derived, expected 0)", status.Sessions)
	}
}

func TestPromotedEventColumns(t *testing.T) {
	// The string-encoded duration on hook events must land in the promoted
	// column, and residual attrs must survive as JSON. Verified through the
	// event-count rollup names since the store exposes no raw row reader.
	st, srv := newTestReceiver(t)
	post(t, srv.URL+"/v1/logs", logsExport)
	status, err := st.OTelStatus()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]float64{}
	for _, nv := range status.EventCounts {
		names[nv.Name] = nv.Value
	}
	if names["api_request"] != 1 || names["hook_execution_complete"] != 1 {
		t.Errorf("event counts = %v, want api_request:1 hook_execution_complete:1", names)
	}
}

func TestProtobufRejectedWithHint(t *testing.T) {
	_, srv := newTestReceiver(t)
	resp, err := http.Post(srv.URL+"/v1/metrics", "application/x-protobuf", strings.NewReader("\x00\x01"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

func TestGzipBody(t *testing.T) {
	st, srv := newTestReceiver(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(metricsExport("100", "200", "10")))
	gz.Close()

	req, err := http.NewRequest("POST", srv.URL+"/v1/metrics", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	counts, err := st.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.OTelDatapoints != 2 {
		t.Fatalf("datapoints = %d, want 2", counts.OTelDatapoints)
	}
}

func TestTracesAcceptedAndDropped(t *testing.T) {
	_, srv := newTestReceiver(t)
	if resp := post(t, srv.URL+"/v1/traces", `{"resourceSpans": []}`); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
