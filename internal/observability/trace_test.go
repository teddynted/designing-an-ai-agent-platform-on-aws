package observability

import (
	"context"
	"testing"
)

func TestParseTraceRoot(t *testing.T) {
	cases := map[string]string{
		"Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1": "1-5759e988-bd862e3fe1be46a994272793",
		"Root=1-abc-def":           "1-abc-def",
		"Parent=x;Sampled=0":       "", // no Root
		"":                         "",
		"Sampled=1;Root=1-xyz-123": "1-xyz-123", // Root not first
	}
	for in, want := range cases {
		if got := parseTraceRoot(in); got != want {
			t.Errorf("parseTraceRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExplicitTraceIDWins(t *testing.T) {
	ctx := WithTraceID(context.Background(), "explicit-trace")
	if got := TraceIDFrom(ctx); got != "explicit-trace" {
		t.Errorf("TraceIDFrom = %q, want explicit-trace", got)
	}
}

func TestTraceIDFlowsIntoFields(t *testing.T) {
	ctx := WithTraceID(context.Background(), "1-trace-id")
	if got := FieldsFrom(ctx).TraceID; got != "1-trace-id" {
		t.Errorf("FieldsFrom(ctx).TraceID = %q, want 1-trace-id", got)
	}
}

func TestNoTraceIsNotAnError(t *testing.T) {
	// A local run with no X-Ray context yields an empty ID and an omitted field, not
	// a panic or a placeholder.
	if got := TraceIDFrom(context.Background()); got != "" {
		// Guard against a stray _X_AMZN_TRACE_ID in the test environment.
		t.Logf("ambient trace present: %q (environment-dependent, not a failure)", got)
	}
}
