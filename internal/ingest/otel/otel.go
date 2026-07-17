// Package otel implements a minimal OTLP/HTTP receiver for Claude Code's
// OpenTelemetry export: metrics land in otel_datapoint, log events in
// otel_event. Live-only — there is no backfill; the transcript ingesters
// remain the source of record for sessions and requests, and OTel rows join
// against them via the harness session uuid and request_id.
//
// Only the JSON encoding is accepted (OTEL_EXPORTER_OTLP_PROTOCOL=http/json);
// protobuf and gRPC would each pull in a dependency tree for data we can
// already read with encoding/json. Traces are accepted and dropped so a
// full OTEL_TRACES_EXPORTER=otlp config doesn't error.
//
// Facts this receiver relies on, verified against Claude Code 2.1.212
// (console-exporter probe) and code.claude.com/docs/en/monitoring-usage:
//
//   - Metrics: claude_code.{session.count, token.usage, cost.usage,
//     active_time.total, lines_of_code.count, ...}. token.usage/cost.usage
//     carry model, type (input|output|cacheRead|cacheCreation), query_source,
//     and — when the context is active — agent.name / skill.name /
//     plugin.name / mcp_server.name / mcp_tool.name attribution.
//   - Events arrive as log records whose body is the event name
//     ("claude_code.api_request", ...), with payload in attributes.
//     api_request carries request_id + per-request tokens + cost_usd +
//     duration_ms; request_id matches the requestId in Claude Code JSONL
//     transcripts, so events cross-check transcript-ingested request rows.
//   - Every record repeats identity attributes (session.id, user.*,
//     organization.id, terminal.type) and events carry a per-session
//     event.sequence plus a prompt.id correlating one user turn.
//   - Numeric attribute values arrive as JSON numbers OR strings depending
//     on field and version (duration_ms is "961" on hook events, 2371 on
//     api_request) — promotion coerces both.
package otel

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/erewhon/tokenator/internal/store"
)

const (
	SourceKind  = "otel"
	DefaultAddr = "127.0.0.1:4318"

	// maxBody bounds one OTLP POST. Claude Code batches are tens of KB;
	// this is pure runaway protection.
	maxBody = 32 << 20
)

// Receiver is an OTLP/HTTP JSON endpoint writing straight to the store.
type Receiver struct {
	Addr   string // listen address; empty means DefaultAddr
	Regime string // billing regime recorded on the source row
	Store  *store.Store
	Logf   func(format string, args ...any) // nil means log.Printf

	sourceID int64
}

func (rc *Receiver) logf(format string, args ...any) {
	if rc.Logf != nil {
		rc.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (rc *Receiver) addr() string {
	if rc.Addr == "" {
		return DefaultAddr
	}
	return rc.Addr
}

// Handler registers the source row and returns the OTLP mux. Split from Run
// so tests can drive the receiver through httptest without binding a port.
func (rc *Receiver) Handler() (http.Handler, error) {
	regime := rc.Regime
	if regime == "" {
		regime = "subscription"
	}
	id, err := rc.Store.UpsertSource(SourceKind, rc.addr(), regime)
	if err != nil {
		return nil, err
	}
	rc.sourceID = id
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/metrics", rc.handleMetrics)
	mux.HandleFunc("POST /v1/logs", rc.handleLogs)
	mux.HandleFunc("POST /v1/traces", rc.handleTraces)
	return mux, nil
}

// Run serves until ctx is cancelled.
func (rc *Receiver) Run(ctx context.Context) error {
	h, err := rc.Handler()
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: rc.addr(), Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	case err := <-errc:
		return err
	}
}

// --- HTTP handlers ---

// readJSONBody enforces the JSON encoding and decompresses gzip bodies.
// A false return means a response was already written.
func readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	ct := r.Header.Get("Content-Type")
	if !strings.Contains(ct, "json") {
		http.Error(w,
			fmt.Sprintf("content-type %q not supported: this receiver speaks OTLP/HTTP JSON only — set OTEL_EXPORTER_OTLP_PROTOCOL=http/json", ct),
			http.StatusUnsupportedMediaType)
		return nil, false
	}
	var rd io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(rd)
		if err != nil {
			http.Error(w, "bad gzip body: "+err.Error(), http.StatusBadRequest)
			return nil, false
		}
		defer gz.Close()
		rd = gz
	}
	body, err := io.ReadAll(rd)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

func (rc *Receiver) handleMetrics(w http.ResponseWriter, r *http.Request) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var req metricsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode metrics: "+err.Error(), http.StatusBadRequest)
		return
	}
	rows, skipped := datapointRows(rc.sourceID, req)
	var added int
	err := rc.Store.WithTx(func(tx *store.Tx) error {
		for _, d := range rows {
			isNew, err := tx.UpsertOTelDatapoint(d)
			if err != nil {
				return err
			}
			if isNew {
				added++
			}
		}
		return nil
	})
	if err != nil {
		rc.logf("otel: metrics write failed: %v", err)
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	msg := fmt.Sprintf("metrics: %d datapoints (+%d new)", len(rows), added)
	if skipped > 0 {
		msg += fmt.Sprintf(", %d unsupported metric types skipped", skipped)
	}
	rc.logf("%s", msg)
	writeOK(w)
}

func (rc *Receiver) handleLogs(w http.ResponseWriter, r *http.Request) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var req logsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode logs: "+err.Error(), http.StatusBadRequest)
		return
	}
	rows := eventRows(rc.sourceID, req)
	var added int
	byEvent := map[string]int{}
	err := rc.Store.WithTx(func(tx *store.Tx) error {
		for _, e := range rows {
			isNew, err := tx.InsertOTelEvent(e)
			if err != nil {
				return err
			}
			if isNew {
				added++
				byEvent[e.Event]++
			}
		}
		return nil
	})
	if err != nil {
		rc.logf("otel: logs write failed: %v", err)
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rc.logf("logs: %d events (+%d new)%s", len(rows), added, eventSummary(byEvent))
	writeOK(w)
}

// handleTraces accepts and drops span exports so that a config with
// OTEL_TRACES_EXPORTER=otlp pointed here doesn't produce client errors.
func (rc *Receiver) handleTraces(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxBody))
	writeOK(w)
}

func eventSummary(byEvent map[string]int) string {
	if len(byEvent) == 0 {
		return ""
	}
	parts := make([]string, 0, len(byEvent))
	for name, n := range byEvent {
		parts = append(parts, fmt.Sprintf("%s×%d", name, n))
	}
	// Deterministic enough for logs; ordering is not load-bearing.
	return " [" + strings.Join(parts, " ") + "]"
}

// --- row conversion ---

// tsMillis renders unix nanos as fixed-width RFC3339 UTC milliseconds —
// the same format Claude Code writes in transcripts, and lexically sortable
// (RFC3339Nano's trailing-zero trimming is not).
func tsMillis(nano flexInt) string {
	if nano == 0 {
		return ""
	}
	return time.Unix(0, int64(nano)).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// canonical returns the attribute map as deterministic JSON (Go sorts map
// keys when marshalling) for hashing and residual storage.
func canonical(attrs map[string]any) string {
	b, err := json.Marshal(attrs)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func hashKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// popStr removes and returns a string attribute ("" when absent).
func popStr(attrs map[string]any, key string) string {
	v, ok := attrs[key]
	if !ok {
		return ""
	}
	delete(attrs, key)
	switch s := v.(type) {
	case string:
		return s
	case int64:
		return strconv.FormatInt(s, 10)
	case float64:
		return strconv.FormatFloat(s, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(s)
	}
	return ""
}

// popInt removes and returns an integer attribute, coercing the string and
// float encodings seen on the wire; nil when absent or unparseable.
func popInt(attrs map[string]any, key string) *int64 {
	v, ok := attrs[key]
	if !ok {
		return nil
	}
	delete(attrs, key)
	switch n := v.(type) {
	case int64:
		return &n
	case float64:
		i := int64(n)
		return &i
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return &i
		}
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			i := int64(f)
			return &i
		}
	}
	return nil
}

// popFloat removes and returns a float attribute; nil when absent.
func popFloat(attrs map[string]any, key string) *float64 {
	v, ok := attrs[key]
	if !ok {
		return nil
	}
	delete(attrs, key)
	switch n := v.(type) {
	case float64:
		return &n
	case int64:
		f := float64(n)
		return &f
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return &f
		}
	}
	return nil
}

// merged flattens record attributes over resource attributes into one map.
// Resource attrs (service.version, os.type, ...) are constant per process
// and never collide with record attrs in practice; record wins if they do.
func merged(res resource, recordAttrs []kv) map[string]any {
	out := attrMap(res.Attributes)
	for k, v := range attrMap(recordAttrs) {
		out[k] = v
	}
	return out
}

func datapointRows(sourceID int64, req metricsRequest) (rows []store.OTelDatapoint, skipped int) {
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				data := m.Sum
				if data == nil {
					data = m.Gauge
				}
				if data == nil {
					skipped++ // histogram/summary — nothing Claude Code emits today
					continue
				}
				for _, dp := range data.DataPoints {
					attrs := merged(rm.Resource, dp.Attributes)
					// Dedupe key covers the full attribute set BEFORE
					// promotion plus the series start: one row per series
					// instance, updated in place as a cumulative series
					// re-exports, new rows per export under delta.
					key := hashKey("dp", m.Name,
						strconv.FormatInt(int64(dp.StartTimeUnixNano), 10), canonical(attrs))
					d := store.OTelDatapoint{
						SourceID:    sourceID,
						SessionKey:  popStr(attrs, "session.id"),
						Metric:      m.Name,
						Model:       popStr(attrs, "model"),
						Type:        popStr(attrs, "type"),
						QuerySource: popStr(attrs, "query_source"),
						AgentName:   popStr(attrs, "agent.name"),
						SkillName:   popStr(attrs, "skill.name"),
						PluginName:  popStr(attrs, "plugin.name"),
						MCPServer:   popStr(attrs, "mcp_server.name"),
						MCPTool:     popStr(attrs, "mcp_tool.name"),
						StartTS:     tsMillis(dp.StartTimeUnixNano),
						TS:          tsMillis(dp.TimeUnixNano),
						Value:       dp.value(),
						Attrs:       canonical(attrs),
						DedupeKey:   key,
					}
					rows = append(rows, d)
				}
			}
		}
	}
	return rows, skipped
}

func eventRows(sourceID int64, req logsRequest) []store.OTelEvent {
	var rows []store.OTelEvent
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				attrs := merged(rl.Resource, lr.Attributes)

				name, _ := lr.Body.native().(string)
				if name == "" {
					if s, ok := attrs["event.name"].(string); ok {
						name = s
					}
				}
				name = strings.TrimPrefix(name, "claude_code.")
				if name == "" {
					name = "(unknown)"
				}

				nano := lr.TimeUnixNano
				if nano == 0 {
					nano = lr.ObservedTimeUnixNano
				}
				ts := tsMillis(nano)
				if ts == "" {
					// last resort: Claude Code repeats the timestamp as an
					// RFC3339 string attribute
					if s, ok := attrs["event.timestamp"].(string); ok {
						ts = s
					}
				}

				// Full pre-promotion attribute set makes the key: identical
				// re-delivered records dedupe, everything else is distinct
				// (event.sequence and tool_use_id are inside the map).
				key := hashKey("ev", name,
					strconv.FormatInt(int64(nano), 10), canonical(attrs))

				// event.name/timestamp are redundant with columns; drop
				// from the residual rather than promote.
				delete(attrs, "event.name")
				delete(attrs, "event.timestamp")
				var seq int64
				if p := popInt(attrs, "event.sequence"); p != nil {
					seq = *p
				}
				e := store.OTelEvent{
					SourceID:            sourceID,
					SessionKey:          popStr(attrs, "session.id"),
					Event:               name,
					TS:                  ts,
					Seq:                 seq,
					PromptID:            popStr(attrs, "prompt.id"),
					RequestID:           popStr(attrs, "request_id"),
					Model:               popStr(attrs, "model"),
					ToolName:            popStr(attrs, "tool_name"),
					ToolUseID:           popStr(attrs, "tool_use_id"),
					QuerySource:         popStr(attrs, "query_source"),
					AgentName:           popStr(attrs, "agent.name"),
					SkillName:           popStr(attrs, "skill.name"),
					MCPServer:           popStr(attrs, "mcp_server.name"),
					MCPTool:             popStr(attrs, "mcp_tool.name"),
					DurationMS:          popInt(attrs, "duration_ms"),
					InputTokens:         popInt(attrs, "input_tokens"),
					OutputTokens:        popInt(attrs, "output_tokens"),
					CacheReadTokens:     popInt(attrs, "cache_read_tokens"),
					CacheCreationTokens: popInt(attrs, "cache_creation_tokens"),
					CostUSD:             popFloat(attrs, "cost_usd"),
					Attrs:               canonical(attrs),
					DedupeKey:           key,
				}
				rows = append(rows, e)
			}
		}
	}
	return rows
}
