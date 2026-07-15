package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

// emitterTo builds an enabled emitter writing to buf, with a fixed clock so the
// timestamp is assertable.
func emitterTo(buf *bytes.Buffer, service string) *Emitter {
	return &Emitter{
		namespace: "aiap/app",
		service:   service,
		enabled:   true,
		w:         buf,
		now:       func() time.Time { return time.UnixMilli(1_700_000_000_000) },
	}
}

func TestEMFEnvelopeIsWellFormed(t *testing.T) {
	var buf bytes.Buffer
	e := emitterTo(&buf, "workflow")

	e.New(Dimensions{"Workflow": "blog-generator"}).
		Duration("WorkflowDurationMs", 1500*time.Millisecond).
		Count("WorkflowSuccess", 1).
		Emit(context.Background(), "workflow completed")

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("EMF line is not valid JSON: %v\n%s", err, buf.String())
	}

	// The _aws envelope: namespace, dimensions, metric definitions.
	aws, ok := doc["_aws"].(map[string]any)
	if !ok {
		t.Fatal("no _aws envelope")
	}
	if aws["Timestamp"].(float64) != 1_700_000_000_000 {
		t.Errorf("timestamp = %v", aws["Timestamp"])
	}
	cw := aws["CloudWatchMetrics"].([]any)[0].(map[string]any)
	if cw["Namespace"] != "aiap/app" {
		t.Errorf("namespace = %v", cw["Namespace"])
	}

	// Dimensions must include Service (added by default) and Workflow, in sorted
	// order so the time series is stable.
	dims := cw["Dimensions"].([]any)[0].([]any)
	if len(dims) != 2 || dims[0] != "Service" || dims[1] != "Workflow" {
		t.Errorf("dimensions = %v, want [Service Workflow]", dims)
	}

	// The metric values live at the top level, not inside _aws.
	if doc["WorkflowDurationMs"].(float64) != 1500 {
		t.Errorf("duration = %v, want 1500", doc["WorkflowDurationMs"])
	}
	if doc["WorkflowSuccess"].(float64) != 1 {
		t.Errorf("success = %v, want 1", doc["WorkflowSuccess"])
	}
	if doc["Service"] != "workflow" || doc["Workflow"] != "blog-generator" {
		t.Errorf("dimension values missing: Service=%v Workflow=%v", doc["Service"], doc["Workflow"])
	}
	if doc["message"] != "workflow completed" {
		t.Errorf("message = %v", doc["message"])
	}
}

func TestEMFCarriesCorrelationFields(t *testing.T) {
	var buf bytes.Buffer
	e := emitterTo(&buf, "workflow")

	ctx := WithFields(context.Background(), Fields{CorrelationID: "push:abc", WorkflowID: "blog"})
	e.New(nil).Count("AIRequests", 1).Emit(ctx, "")

	var doc map[string]any
	json.Unmarshal(buf.Bytes(), &doc)
	if doc[FieldCorrelationID] != "push:abc" {
		t.Errorf("correlationId missing from metric line: %v", doc[FieldCorrelationID])
	}
}

// A disabled emitter writes nothing at all — no envelope, no blank line — so a CLI
// or a test is not polluted with metric noise.
func TestDisabledEmitterIsSilent(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(Config{}) // no namespace -> disabled
	e.w = &buf
	if e.Enabled() {
		t.Fatal("emitter with no namespace should be disabled")
	}
	e.New(Dimensions{"X": "y"}).Count("Foo", 1).Emit(context.Background(), "msg")
	if buf.Len() != 0 {
		t.Errorf("disabled emitter wrote: %s", buf.String())
	}
}

// A metric with no values is not a zero — it is nothing — and must not emit an
// empty envelope that a metric filter would then have to learn to ignore.
func TestEmptyMetricIsNotEmitted(t *testing.T) {
	var buf bytes.Buffer
	e := emitterTo(&buf, "svc")
	e.New(Dimensions{"X": "y"}).Emit(context.Background(), "msg")
	if buf.Len() != 0 {
		t.Errorf("empty metric emitted: %s", buf.String())
	}
}
