package observability

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

// A CloudWatch metric unit. These are the subset the platform actually uses; the
// full list is larger, but a metric in a unit nobody graphs is a metric nobody
// reads.
const (
	UnitCount        = "Count"
	UnitMilliseconds = "Milliseconds"
	UnitSeconds      = "Seconds"
	UnitBytes        = "Bytes"
	UnitPercent      = "Percent"
	UnitNone         = "None"
)

// Dimensions are the label set a metric is grouped by — Service, Workflow,
// Provider. CloudWatch treats each distinct combination of dimension *values* as
// its own time series, which is the feature and the cost: dimension on the model
// name and you can graph latency per model; dimension on the request ID and you
// have created a million one-point metrics and a bill to match. So the rule this
// package follows, and documents, is: **dimension on things with a small, bounded
// set of values, never on an identifier.**
type Dimensions map[string]string

// Emitter writes custom metrics in the CloudWatch Embedded Metric Format (EMF).
//
// # Why EMF and not PutMetricData
//
// The obvious way to emit a custom metric is the PutMetricData API. It works, and
// it has three costs the platform would rather not pay: a network call in the hot
// path (so a metric can slow down or fail the thing it measures), an IAM grant
// (cloudwatch:PutMetricData, which every emitting role then holds), and a second
// system to reason about when a metric is missing.
//
// EMF removes all three. A metric is a specially-shaped JSON *log line*. In a
// Lambda it goes to stdout; on EC2 the CloudWatch agent ships it; either way
// CloudWatch reads the `_aws` envelope and extracts real metrics from it —
// asynchronously, at no extra API cost, under the logging permission the process
// already has. The line is *both* a log (searchable, with its correlation IDs
// intact) and a metric (graphable, alarmable). One write, two products.
//
// The trade is that extraction is not instant and a malformed envelope silently
// produces no metric — which is why [Metric.Emit] builds the envelope from typed
// calls rather than letting a caller hand-write JSON, and why the CloudFormation
// stack also defines metric *filters* as a belt-and-braces path for plain
// structured logs.
type Emitter struct {
	namespace string
	service   string
	enabled   bool

	mu  sync.Mutex // serialises writes so two goroutines' EMF lines never interleave
	w   io.Writer
	now func() time.Time
}

// NewEmitter builds an Emitter from a [Config]. When metrics are disabled (no
// namespace, or explicitly off) it returns a no-op emitter: every method still
// works and simply writes nothing, so a caller never guards a metric call with an
// `if cfg.Metrics()`.
func NewEmitter(cfg Config) *Emitter {
	return &Emitter{
		namespace: cfg.MetricsNamespace,
		service:   cfg.Service,
		enabled:   cfg.Metrics(),
		w:         os.Stdout,
		now:       time.Now,
	}
}

// Enabled reports whether this emitter will actually write anything. A caller that
// wants to skip *building* an expensive metric (not just emitting it) can check
// this first.
func (e *Emitter) Enabled() bool { return e != nil && e.enabled }

// Metric accumulates one or more values that share a timestamp and a dimension set
// into a single EMF line. Batching is the point: "workflow completed" carries a
// duration *and* a success count *and* an active-executions gauge, and emitting
// them together is one line, one timestamp, one set of correlation IDs — not three
// lines a query has to stitch back together.
type Metric struct {
	e      *Emitter
	dims   Dimensions
	defs   []metricDef
	values map[string]float64
}

type metricDef struct {
	Name string `json:"Name"`
	Unit string `json:"Unit"`
}

// New starts a metric line. The dimensions default to the service name if the
// caller passes none, so every metric is at least sliceable by which process
// emitted it.
func (e *Emitter) New(dims Dimensions) *Metric {
	if dims == nil {
		dims = Dimensions{}
	}
	if _, ok := dims["Service"]; !ok && e.service != "" {
		dims["Service"] = e.service
	}
	return &Metric{e: e, dims: dims, values: map[string]float64{}}
}

// Put adds a value in an explicit unit. Later calls with the same name overwrite —
// a metric line holds one value per name, because two values for "Duration" in one
// line is a question CloudWatch answers by picking one, silently.
func (m *Metric) Put(name string, value float64, unit string) *Metric {
	if _, seen := m.values[name]; !seen {
		m.defs = append(m.defs, metricDef{Name: name, Unit: unit})
	}
	m.values[name] = value
	return m
}

// Count adds a count (Unit: Count) — an invocation, an error, a retry.
func (m *Metric) Count(name string, value float64) *Metric {
	return m.Put(name, value, UnitCount)
}

// Duration adds a duration in milliseconds, which is how CloudWatch prefers to
// graph latency and how Lambda already reports its own.
func (m *Metric) Duration(name string, d time.Duration) *Metric {
	return m.Put(name, float64(d.Milliseconds()), UnitMilliseconds)
}

// Gauge adds a point-in-time value with no natural unit — active executions, queue
// length.
func (m *Metric) Gauge(name string, value float64) *Metric {
	return m.Put(name, value, UnitNone)
}

// Emit writes the EMF line. The optional message and fields become ordinary log
// attributes on the same line, so the metric and its context travel together: the
// line that records "WorkflowFailures: 1" also says which workflow, for which
// correlation ID, and why.
//
// On a disabled emitter, or a metric with no values, it does nothing — an empty
// EMF envelope is not a zero, it is noise, and a metric filter downstream would
// have to learn to ignore it.
func (m *Metric) Emit(ctx context.Context, message string, extra ...any) {
	if m == nil || m.e == nil || !m.e.enabled || len(m.defs) == 0 {
		return
	}

	// Dimension names in a stable order: CloudWatch keys a time series on the
	// ordered set, and a set that reordered between two emits would fragment one
	// series into two.
	dimNames := make([]string, 0, len(m.dims))
	for k := range m.dims {
		dimNames = append(dimNames, k)
	}
	sort.Strings(dimNames)

	doc := map[string]any{
		"_aws": map[string]any{
			"Timestamp": m.e.now().UnixMilli(),
			"CloudWatchMetrics": []map[string]any{{
				"Namespace":  m.e.namespace,
				"Dimensions": [][]string{dimNames},
				"Metrics":    m.defs,
			}},
		},
	}

	// The dimension values and the metric values are top-level keys, per the EMF
	// contract — the `_aws` block only *names* them.
	for k, v := range m.dims {
		doc[k] = v
	}
	for k, v := range m.values {
		doc[k] = v
	}

	// Correlation IDs from the context, so the metric line is as followable as a
	// log line — the same fields, by the same names.
	for _, kv := range chunk2(FieldsFrom(ctx).attrs()) {
		doc[kv[0]] = kv[1]
	}
	if message != "" {
		doc["message"] = message
	}
	for _, kv := range chunk2(extra) {
		doc[kv[0]] = kv[1]
	}

	line, err := json.Marshal(doc)
	if err != nil {
		// A metric that will not marshal is a bug in the caller's extra fields, not
		// a reason to crash the work being measured. Drop it; the log line that
		// accompanies real work will still be emitted by the logger.
		return
	}

	m.e.mu.Lock()
	defer m.e.mu.Unlock()
	m.e.w.Write(append(line, '\n'))
}

// chunk2 turns slog's alternating key/value slice into [][2]string pairs, skipping
// anything that is not a string key with a string value — EMF top-level context
// fields are strings, and a non-string here is a programming error better dropped
// than emitted as null.
func chunk2(kv []any) [][2]string {
	var out [][2]string
	for i := 0; i+1 < len(kv); i += 2 {
		k, kok := kv[i].(string)
		v, vok := kv[i+1].(string)
		if kok && vok && k != "" {
			out = append(out, [2]string{k, v})
		}
	}
	return out
}
