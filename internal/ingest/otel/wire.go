// OTLP/HTTP JSON wire format, hand-decoded with encoding/json.
//
// Only the fields tokenator reads are declared; everything else is ignored,
// matching the lenient-parsing stance of the transcript ingesters. Two
// protojson quirks are load-bearing:
//   - 64-bit integers (timeUnixNano, intValue, ...) are encoded as JSON
//     strings per proto3 JSON mapping, but some exporters emit bare numbers;
//     flexInt accepts both.
//   - attribute values are tagged unions ({"stringValue": ...}), flattened
//     to native Go values before storage.
package otel

import (
	"encoding/json"
	"strconv"
)

// flexInt is an int64 decoded from either a JSON number or a string.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*f = 0
			return nil
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt(n)
		return nil
	}
	fl, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexInt(fl)
	return nil
}

type kv struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type anyValue struct {
	StringValue *string  `json:"stringValue"`
	IntValue    *flexInt `json:"intValue"`
	DoubleValue *float64 `json:"doubleValue"`
	BoolValue   *bool    `json:"boolValue"`
	BytesValue  *string  `json:"bytesValue"`
	ArrayValue  *struct {
		Values []anyValue `json:"values"`
	} `json:"arrayValue"`
	KvlistValue *struct {
		Values []kv `json:"values"`
	} `json:"kvlistValue"`
}

// native flattens the union to a plain Go value (nil when empty).
func (v anyValue) native() any {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return int64(*v.IntValue)
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.BytesValue != nil:
		return *v.BytesValue // base64 as-is; nothing downstream decodes bytes
	case v.ArrayValue != nil:
		out := make([]any, len(v.ArrayValue.Values))
		for i, e := range v.ArrayValue.Values {
			out[i] = e.native()
		}
		return out
	case v.KvlistValue != nil:
		return attrMap(v.KvlistValue.Values)
	}
	return nil
}

func attrMap(kvs []kv) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, e := range kvs {
		out[e.Key] = e.Value.native()
	}
	return out
}

type resource struct {
	Attributes []kv `json:"attributes"`
}

// --- ExportMetricsServiceRequest ---

type metricsRequest struct {
	ResourceMetrics []struct {
		Resource     resource `json:"resource"`
		ScopeMetrics []struct {
			Metrics []metric `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

// metric reads sum and gauge data identically: the dedupe scheme in the
// store (keyed on start_ts + attributes) makes cumulative, delta, and gauge
// points all land correctly without inspecting aggregationTemporality.
type metric struct {
	Name  string      `json:"name"`
	Sum   *metricData `json:"sum"`
	Gauge *metricData `json:"gauge"`
}

type metricData struct {
	DataPoints []dataPoint `json:"dataPoints"`
}

type dataPoint struct {
	Attributes        []kv     `json:"attributes"`
	StartTimeUnixNano flexInt  `json:"startTimeUnixNano"`
	TimeUnixNano      flexInt  `json:"timeUnixNano"`
	AsInt             *flexInt `json:"asInt"`
	AsDouble          *float64 `json:"asDouble"`
}

func (d dataPoint) value() float64 {
	if d.AsInt != nil {
		return float64(*d.AsInt)
	}
	if d.AsDouble != nil {
		return *d.AsDouble
	}
	return 0
}

// --- ExportLogsServiceRequest ---

type logsRequest struct {
	ResourceLogs []struct {
		Resource  resource `json:"resource"`
		ScopeLogs []struct {
			LogRecords []logRecord `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type logRecord struct {
	TimeUnixNano         flexInt  `json:"timeUnixNano"`
	ObservedTimeUnixNano flexInt  `json:"observedTimeUnixNano"`
	Body                 anyValue `json:"body"`
	Attributes           []kv     `json:"attributes"`
}
