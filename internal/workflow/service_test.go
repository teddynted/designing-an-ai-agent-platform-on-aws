package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeEngine stands in for n8n. The Service must be fully testable without one —
// if it were not, every test of the platform's orchestration would need an HTTP
// server, and the Engine interface would not be earning its keep.
type fakeEngine struct {
	workflows []string
	result    Result
	err       error

	calls int
	got   Request
}

func (f *fakeEngine) Name() string        { return "fake" }
func (f *fakeEngine) Workflows() []string { return f.workflows }

func (f *fakeEngine) Trigger(_ context.Context, req Request) (Result, error) {
	f.calls++
	f.got = req
	return f.result, f.err
}

func newFake() *fakeEngine {
	return &fakeEngine{
		workflows: []string{"blog-generator", "release-notes"},
		result:    Result{Status: StatusAccepted, ExecutionID: "exec-1", Attempts: 1},
	}
}

func testEvent() Event {
	return Event{
		ID:            "delivery-abc",
		Type:          "push",
		Repository:    "teddynted/platform",
		RepositoryURL: "https://github.com/teddynted/platform",
		Branch:        "main",
		CommitSHA:     "deadbeef",
		CommitMessage: "feat: something",
	}
}

// newService returns a Service and the buffer its logs land in, because the logs
// are a deliverable here, not a side effect.
func newService(engine Engine) (*Service, *strings.Builder) {
	var logs strings.Builder
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewService(engine, log), &logs
}

// logLines decodes the structured log so a test can assert on fields rather than
// grepping strings — which would pass even if the field names were wrong.
func logLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON (it must be, for CloudWatch): %q", line)
		}
		out = append(out, entry)
	}
	return out
}

func find(entries []map[string]any, msg string) map[string]any {
	for _, e := range entries {
		if e["msg"] == msg {
			return e
		}
	}
	return nil
}

// --- the happy path ---------------------------------------------------------

func TestRun(t *testing.T) {
	engine := newFake()
	svc, _ := newService(engine)

	res, err := svc.Run(context.Background(), Request{Workflow: "blog-generator", Event: testEvent()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusAccepted || res.ExecutionID != "exec-1" {
		t.Errorf("result = %+v, want the engine's outcome passed through", res)
	}
	if res.Workflow != "blog-generator" {
		t.Errorf("Workflow = %q, want it stamped on the result", res.Workflow)
	}
	if engine.calls != 1 {
		t.Errorf("engine called %d times, want 1", engine.calls)
	}
}

// The correlation ID is derived from the event, never generated. The same GitHub
// delivery — retried by GitHub, or replayed by hand — must produce the same ID,
// or the duplicate looks like a new event and nothing can deduplicate it.
func TestCorrelationIDIsDerivedAndStable(t *testing.T) {
	engine := newFake()
	svc, _ := newService(engine)

	first, err := svc.Run(context.Background(), Request{Workflow: "blog-generator", Event: testEvent()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	second, err := svc.Run(context.Background(), Request{Workflow: "blog-generator", Event: testEvent()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if first.CorrelationID != second.CorrelationID {
		t.Errorf("the same event produced two correlation IDs: %q and %q",
			first.CorrelationID, second.CorrelationID)
	}
	if !strings.Contains(first.CorrelationID, "delivery-abc") {
		t.Errorf("CorrelationID = %q, want it derived from the event ID", first.CorrelationID)
	}
}

func TestAnExplicitCorrelationIDIsRespected(t *testing.T) {
	engine := newFake()
	svc, _ := newService(engine)

	res, err := svc.Run(context.Background(), Request{
		Workflow:      "blog-generator",
		Event:         testEvent(),
		CorrelationID: "trace-from-upstream",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CorrelationID != "trace-from-upstream" {
		t.Errorf("CorrelationID = %q, want the caller's own ID preserved", res.CorrelationID)
	}
}

// --- validation -------------------------------------------------------------

// A request that cannot succeed must never reach the engine: sending it turns a
// local, obvious failure into a remote, confusing one.
func TestBadRequestsNeverReachTheEngine(t *testing.T) {
	tests := []struct {
		name    string
		req     Request
		wantErr error
	}{
		{
			"no workflow name",
			Request{Event: testEvent()},
			ErrInvalidRequest,
		},
		{
			"no event ID",
			// Without one there is no correlation and no idempotency key, so a
			// retried trigger becomes a duplicate execution nobody can detect.
			Request{Workflow: "blog-generator", Event: Event{Type: "push"}},
			ErrInvalidRequest,
		},
		{
			"payload is not JSON",
			Request{Workflow: "blog-generator", Event: Event{ID: "x", Payload: json.RawMessage(`{oops`)}},
			ErrInvalidRequest,
		},
		{
			"workflow is not registered",
			Request{Workflow: "nope", Event: testEvent()},
			ErrUnknownWorkflow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := newFake()
			svc, _ := newService(engine)

			if _, err := svc.Run(context.Background(), tt.req); !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if engine.calls != 0 {
				t.Error("the engine was called with a request that could not possibly succeed")
			}
		})
	}
}

// The error for an unknown workflow should say what IS registered. Otherwise the
// next thing that happens is someone grepping the codebase for the name.
func TestUnknownWorkflowErrorListsTheRegisteredOnes(t *testing.T) {
	svc, _ := newService(newFake())

	_, err := svc.Run(context.Background(), Request{Workflow: "typo", Event: testEvent()})
	if !strings.Contains(err.Error(), "blog-generator") {
		t.Errorf("error %q should list what is actually configured", err)
	}
}

// --- observability ----------------------------------------------------------

// Every execution logs at least twice, and both lines carry the correlation ID.
// When a blog post fails to appear three hours after a merge, the only question
// that matters is "did the platform ask, and what did the engine say?" — and it
// must be answerable from the GitHub delivery ID alone.
func TestEveryExecutionIsLogged(t *testing.T) {
	svc, logs := newService(newFake())

	if _, err := svc.Run(context.Background(), Request{Workflow: "blog-generator", Event: testEvent()}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries := logLines(t, logs.String())
	requested, completed := find(entries, "workflow requested"), find(entries, "workflow completed")
	if requested == nil || completed == nil {
		t.Fatalf("want both 'workflow requested' and 'workflow completed'; got %d lines", len(entries))
	}

	for _, entry := range []map[string]any{requested, completed} {
		for _, field := range []string{"correlationId", "workflow", "engine", "repository"} {
			if entry[field] == nil || entry[field] == "" {
				t.Errorf("log line %q is missing %q — it cannot be correlated", entry["msg"], field)
			}
		}
	}
	if completed["executionId"] != "exec-1" {
		t.Error("the completion log must carry the engine's execution ID, or you cannot find the run in n8n")
	}
	if _, ok := completed["durationMs"]; !ok {
		t.Error("the completion log must carry a duration")
	}
}

// A failure must be logged with a STABLE classification, so an alert can fire on
// "unauthorized" without pattern-matching an error message someone will reword.
func TestFailuresAreLoggedWithAStableKind(t *testing.T) {
	tests := []struct {
		err      error
		wantKind string
	}{
		{ErrUnauthorized, "unauthorized"},
		{ErrTimeout, "timeout"},
		{ErrUnavailable, "unavailable"},
		{ErrWorkflowFailed, "workflow_failed"},
		{ErrInvalidResponse, "invalid_response"},
	}

	for _, tt := range tests {
		t.Run(tt.wantKind, func(t *testing.T) {
			engine := newFake()
			engine.err = tt.err
			svc, logs := newService(engine)

			if _, err := svc.Run(context.Background(), Request{Workflow: "blog-generator", Event: testEvent()}); err == nil {
				t.Fatal("want an error")
			}

			failed := find(logLines(t, logs.String()), "workflow failed")
			if failed == nil {
				t.Fatal("a failure must be logged")
			}
			if failed["errorKind"] != tt.wantKind {
				t.Errorf("errorKind = %v, want %q", failed["errorKind"], tt.wantKind)
			}
		})
	}
}

// "We gave up retrying" and "what we gave up on" are different questions, and an
// on-call engineer needs both. ErrRetriesExhausted wraps its cause, so the kind
// must still report the cause.
func TestRetriesExhaustedStillReportsTheCause(t *testing.T) {
	engine := newFake()
	engine.err = errors.Join(ErrRetriesExhausted, ErrTimeout)
	engine.result = Result{Attempts: 3}
	svc, logs := newService(engine)

	if _, err := svc.Run(context.Background(), Request{Workflow: "blog-generator", Event: testEvent()}); err == nil {
		t.Fatal("want an error")
	}

	failed := find(logLines(t, logs.String()), "workflow failed")
	if failed["errorKind"] != "timeout" {
		t.Errorf("errorKind = %v, want the cause (timeout), not the wrapper", failed["errorKind"])
	}
	if failed["retriesExhausted"] != true {
		t.Error("retriesExhausted must be logged separately from the cause")
	}
	if failed["attempts"] != float64(3) {
		t.Errorf("attempts = %v, want 3", failed["attempts"])
	}
}

// --- timing -----------------------------------------------------------------

func TestDurationIsMeasured(t *testing.T) {
	engine := newFake()

	// A clock that advances 250ms across the call, so the duration is exact and
	// the test does not sleep.
	var ticks int
	clock := func() time.Time {
		t := time.Unix(0, 0).Add(time.Duration(ticks) * 250 * time.Millisecond)
		ticks++
		return t
	}

	var logs strings.Builder
	svc := NewService(engine, slog.New(slog.NewJSONHandler(&logs, nil)), WithClock(clock))

	res, err := svc.Run(context.Background(), Request{Workflow: "blog-generator", Event: testEvent()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.DurationMS != 250 {
		t.Errorf("DurationMS = %d, want 250", res.DurationMS)
	}
}

// The engine reports how many attempts it took; the Service must pass that
// through, because "it worked, on the third try" is a different fact from "it
// worked" and the difference is a degrading n8n.
func TestAttemptsSurviveToTheResult(t *testing.T) {
	engine := newFake()
	engine.result = Result{Status: StatusAccepted, Attempts: 3}
	svc, _ := newService(engine)

	res, err := svc.Run(context.Background(), Request{Workflow: "blog-generator", Event: testEvent()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
}

// --- the seam ---------------------------------------------------------------

// The whole point of the Engine interface: the platform can swap n8n for anything
// else without touching a line above this boundary. If this test ever needs an
// HTTP server, the abstraction has failed.
func TestTheEngineIsReplaceable(t *testing.T) {
	var engine Engine = newFake()
	svc := NewService(engine, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	if _, err := svc.Run(context.Background(), Request{Workflow: "blog-generator", Event: testEvent()}); err != nil {
		t.Fatalf("Run against a non-n8n engine: %v", err)
	}
	if got := svc.Workflows(); len(got) != 2 {
		t.Errorf("Workflows() = %v, want the engine's list", got)
	}
}
