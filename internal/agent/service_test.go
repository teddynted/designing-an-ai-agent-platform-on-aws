package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRuntime stands in for OpenClaw. The Service must be fully testable without
// one — if it were not, every test of the platform's agent orchestration would need
// an HTTP server, and the Runtime interface would not be earning its keep.
type fakeRuntime struct {
	tasks []TaskType

	// statuses are returned by successive calls to Status, so a test can script an
	// execution moving from queued → running → succeeded.
	statuses  []Status
	statusIdx int32

	exec      Execution
	result    Result
	submitErr error
	statusErr error
	resultErr error

	submits   int32
	cancels   int32
	statusHit int32
}

func (f *fakeRuntime) Name() string      { return "fake" }
func (f *fakeRuntime) Tasks() []TaskType { return f.tasks }

func (f *fakeRuntime) Submit(_ context.Context, req Request) (Execution, error) {
	atomic.AddInt32(&f.submits, 1)
	if f.submitErr != nil {
		return Execution{Attempts: 1}, f.submitErr
	}
	e := f.exec
	e.TaskType = req.Task.Type
	return e, nil
}

func (f *fakeRuntime) Status(_ context.Context, id string) (Execution, error) {
	atomic.AddInt32(&f.statusHit, 1)
	if f.statusErr != nil {
		return Execution{}, f.statusErr
	}
	e := f.exec
	e.ID = id
	i := int(atomic.AddInt32(&f.statusIdx, 1)) - 1
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	e.Status = f.statuses[i]
	return e, nil
}

func (f *fakeRuntime) Result(_ context.Context, id string) (Result, error) {
	if f.resultErr != nil {
		return Result{}, f.resultErr
	}
	return f.result, nil
}

func (f *fakeRuntime) Cancel(_ context.Context, id string) error {
	atomic.AddInt32(&f.cancels, 1)
	return nil
}

func newFake() *fakeRuntime {
	return &fakeRuntime{
		tasks:     []TaskType{TaskBlogDraft, TaskRepoAnalysis},
		statuses:  []Status{StatusSucceeded},
		exec:      Execution{ID: "exec-1", Agent: "writer", Status: StatusQueued, Attempts: 1},
		result:    Result{Output: Output{Content: "# A draft"}},
		statusIdx: 0,
	}
}

func testRequest() Request {
	return Request{
		CorrelationID:       "push:delivery-abc",
		WorkflowExecutionID: "n8n-42",
		Task: Task{
			Type:         TaskBlogDraft,
			Instructions: "Draft a post.",
			Repository:   Repository{Name: "teddynted/platform", URL: "https://github.com/teddynted/platform", CommitSHA: "deadbeef"},
			Limits:       Limits{MaxSteps: 10, MaxDuration: time.Minute, MaxOutputBytes: 1000},
		},
	}
}

func newService(rt Runtime) (*Service, *strings.Builder) {
	var logs strings.Builder
	return NewService(rt, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))), &logs
}

// The logs are a deliverable, not a side effect: assert on FIELDS, not substrings.
// A grep would pass even if the field names were wrong, and the field names are the
// contract with CloudWatch.
func logLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("log line is not JSON (it must be, for CloudWatch): %q", line)
		}
		out = append(out, e)
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

func instantWait() WaitPolicy {
	return WaitPolicy{
		Interval: time.Millisecond,
		Timeout:  time.Minute,
		sleep:    func(context.Context, time.Duration) error { return nil },
	}
}

// --- submit -----------------------------------------------------------------

func TestSubmitReturnsWithoutWaiting(t *testing.T) {
	rt := newFake()
	svc, _ := newService(rt)

	exec, err := svc.Submit(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if exec.ID != "exec-1" {
		t.Errorf("ID = %q, want the runtime's execution ID", exec.ID)
	}
	// The whole shape of this milestone: submit is fast, the agent is slow. If Submit
	// waited, nothing in the platform could call it.
	if exec.Status.Terminal() {
		t.Error("Submit must not wait for the agent to finish")
	}
	// The chain must be stamped on: GitHub delivery → n8n run → this execution.
	if exec.CorrelationID != "push:delivery-abc" || exec.WorkflowExecutionID != "n8n-42" {
		t.Errorf("execution = %+v, want the correlation chain preserved", exec)
	}
}

// A request that cannot succeed never reaches the runtime. For an agent that is not
// merely tidy: submitting it would start something that costs money.
func TestBadRequestsNeverReachTheRuntime(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Request)
		wantErr error
	}{
		{"no task type", func(r *Request) { r.Task.Type = "" }, ErrInvalidRequest},
		{"no instructions", func(r *Request) { r.Task.Instructions = "" }, ErrInvalidRequest},
		{"no repository", func(r *Request) { r.Task.Repository.URL = "" }, ErrInvalidRequest},
		{"no correlation ID", func(r *Request) { r.CorrelationID = "" }, ErrInvalidRequest},
		{"no limits", func(r *Request) { r.Task.Limits = Limits{} }, ErrInvalidRequest},
		{"unregistered task", func(r *Request) { r.Task.Type = "nope" }, ErrUnknownTask},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newFake()
			svc, _ := newService(rt)

			req := testRequest()
			tt.mutate(&req)

			if _, err := svc.Submit(context.Background(), req); !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if atomic.LoadInt32(&rt.submits) != 0 {
				t.Error("the runtime was asked to start work that could not possibly succeed")
			}
		})
	}
}

// An agent with no limits is a machine for turning money into tokens. There must be
// no way to submit one, including by forgetting.
func TestAnExecutionCannotBeSubmittedWithoutLimits(t *testing.T) {
	req := testRequest()
	req.Task.Limits.MaxSteps = 0

	if err := req.Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal("a task with no step limit must be rejected")
	}
	req = testRequest()
	req.Task.Limits.MaxDuration = 0
	if err := req.Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal("a task with no duration limit must be rejected")
	}
}

// --- waiting ----------------------------------------------------------------

func TestWaitPollsUntilTerminalThenFetchesTheResult(t *testing.T) {
	rt := newFake()
	rt.statuses = []Status{StatusQueued, StatusRunning, StatusRunning, StatusSucceeded}
	svc, logs := newService(rt)

	res, err := svc.Wait(context.Background(), "exec-1", instantWait())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Output.Content != "# A draft" {
		t.Errorf("content = %q, want the agent's output", res.Output.Content)
	}
	if got := atomic.LoadInt32(&rt.statusHit); got != 4 {
		t.Errorf("polled %d times, want 4 (it must keep asking until terminal)", got)
	}

	completed := find(logLines(t, logs.String()), "agent execution completed")
	if completed == nil {
		t.Fatal("a completed execution must be logged")
	}
	for _, field := range []string{"executionId", "correlationId", "taskType", "agent", "durationMs", "polls"} {
		if _, ok := completed[field]; !ok {
			t.Errorf("the completion log is missing %q", field)
		}
	}
}

// The distinction that matters most in this method: OUR patience running out does
// not mean the AGENT stopped. It is still running, and still spending.
func TestGivingUpWaitingDoesNotMeanTheAgentStopped(t *testing.T) {
	rt := newFake()
	rt.statuses = []Status{StatusRunning} // never finishes
	svc, logs := newService(rt)

	policy := WaitPolicy{
		Interval: time.Millisecond,
		Timeout:  time.Minute,
		// A sleep that reports the deadline expiring, which is what a real timeout
		// looks like from inside the loop.
		sleep: func(context.Context, time.Duration) error { return context.DeadlineExceeded },
	}

	_, err := svc.Wait(context.Background(), "exec-1", policy)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("the error must say the execution is still running; got %q", err)
	}

	warned := find(logLines(t, logs.String()), "gave up waiting; the execution is still running")
	if warned == nil {
		t.Error("giving up must be logged as a warning — the agent is still spending money")
	}
	if atomic.LoadInt32(&rt.cancels) != 0 {
		t.Error("Wait must not cancel on its own; that is the caller's decision to make")
	}
}

// An agent that ran and failed is a RESULT, not a transport failure. It must never
// be retried by us — it has already spent the money, and it may already have opened
// a pull request.
func TestATerminalFailureIsAResultNotATransportError(t *testing.T) {
	tests := []struct {
		status   Status
		wantErr  error
		wantKind string
	}{
		{StatusFailed, ErrAgentFailed, "agent_failed"},
		{StatusTimedOut, ErrExecutionTimeout, "execution_timeout"},
		{StatusCancelled, ErrCancelled, "cancelled"},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			rt := newFake()
			rt.statuses = []Status{tt.status}
			rt.exec = Execution{ID: "exec-1", Agent: "writer", Steps: 40, Cost: 1.80, Error: "it went wrong"}
			svc, logs := newService(rt)

			_, err := svc.Wait(context.Background(), "exec-1", instantWait())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}

			failed := find(logLines(t, logs.String()), "agent execution failed")
			if failed == nil {
				t.Fatal("a failed execution must be logged")
			}
			if failed["errorKind"] != tt.wantKind {
				t.Errorf("errorKind = %v, want %q", failed["errorKind"], tt.wantKind)
			}
			// "It failed after 40 steps and $1.80" is a different problem from "it failed
			// immediately", and only one of them is a bug in the prompt.
			if failed["steps"] != float64(40) || failed["costUsd"] != 1.80 {
				t.Errorf("the failure log must report what was spent; got steps=%v cost=%v",
					failed["steps"], failed["costUsd"])
			}
		})
	}
}

// The agent succeeded and we could not read what it produced. The work is done and
// paid for, and we are about to throw it away — that must be loud, and it must carry
// the execution ID so a human can recover it.
func TestSucceededButUnreadableResultIsLoud(t *testing.T) {
	rt := newFake()
	rt.statuses = []Status{StatusSucceeded}
	rt.resultErr = ErrInvalidResponse
	svc, logs := newService(rt)

	if _, err := svc.Wait(context.Background(), "exec-1", instantWait()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
	entry := find(logLines(t, logs.String()), "agent succeeded but its result could not be read")
	if entry == nil {
		t.Fatal("losing a successful, paid-for result must be logged loudly")
	}
	if entry["executionId"] != "exec-1" {
		t.Error("the log must carry the execution ID, or the result cannot be recovered by hand")
	}
}

// A status check failing does not mean the execution failed — we simply cannot see
// it. Concluding anything about the agent from it would be wrong.
func TestAStatusFailureIsNotAnAgentFailure(t *testing.T) {
	rt := newFake()
	rt.statusErr = ErrUnavailable
	svc, _ := newService(rt)

	_, err := svc.Wait(context.Background(), "exec-1", instantWait())
	if errors.Is(err, ErrAgentFailed) {
		t.Error("an unreachable runtime must not be reported as a failed agent")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

// --- observability ----------------------------------------------------------

func TestEveryExecutionIsLoggedWithTheCorrelationChain(t *testing.T) {
	rt := newFake()
	svc, logs := newService(rt)

	if _, err := svc.Submit(context.Background(), testRequest()); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	entries := logLines(t, logs.String())
	requested, started := find(entries, "agent execution requested"), find(entries, "agent execution started")
	if requested == nil || started == nil {
		t.Fatal("want both 'requested' and 'started'")
	}
	for _, e := range []map[string]any{requested, started} {
		// Without the chain, "why did this pull request appear?" has no answer.
		for _, field := range []string{"correlationId", "workflowExecutionId", "taskType", "runtime"} {
			if e[field] == nil || e[field] == "" {
				t.Errorf("log %q is missing %q", e["msg"], field)
			}
		}
	}
	if started["executionId"] != "exec-1" {
		t.Error("'started' must carry the execution ID — it is the first moment the run exists outside our intention")
	}
	// The budget is logged at request time, so a bill can be traced to what authorised it.
	if requested["maxSteps"] != float64(10) {
		t.Errorf("the request log must record the limits; got %v", requested["maxSteps"])
	}
}

func TestSubmitFailureIsLoggedWithAStableKind(t *testing.T) {
	rt := newFake()
	rt.submitErr = ErrUnauthorized
	svc, logs := newService(rt)

	if _, err := svc.Submit(context.Background(), testRequest()); err == nil {
		t.Fatal("want an error")
	}
	failed := find(logLines(t, logs.String()), "agent execution failed to start")
	if failed == nil || failed["errorKind"] != "unauthorized" {
		t.Errorf("want errorKind=unauthorized, got %v", failed)
	}
}

// ErrRetriesExhausted wraps its cause, so the KIND must still report the cause —
// "we gave up" is far less useful on call than "we gave up on a timeout".
func TestKindReportsTheCauseNotTheWrapper(t *testing.T) {
	if got := Kind(errors.Join(ErrRetriesExhausted, ErrTimeout)); got != "timeout" {
		t.Errorf("Kind = %q, want timeout (the cause), not retries_exhausted (the wrapper)", got)
	}
	if got := Kind(ErrRetriesExhausted); got != "retries_exhausted" {
		t.Errorf("Kind = %q, want retries_exhausted when there is no cause to report", got)
	}
}

// --- the seam ---------------------------------------------------------------

// The point of the Runtime interface: the platform can swap OpenClaw for anything
// else without touching a line above this boundary. If this test ever needs an HTTP
// server, the abstraction has failed.
func TestTheRuntimeIsReplaceable(t *testing.T) {
	var rt Runtime = newFake()
	svc := NewService(rt, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	if _, err := svc.Execute(context.Background(), testRequest(), instantWait()); err != nil {
		t.Fatalf("Execute against a non-OpenClaw runtime: %v", err)
	}
	if len(svc.Tasks()) != 2 {
		t.Errorf("Tasks() = %v, want the runtime's list", svc.Tasks())
	}
}

func TestCancelIsPassedThrough(t *testing.T) {
	rt := newFake()
	svc, logs := newService(rt)

	if err := svc.Cancel(context.Background(), "exec-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if atomic.LoadInt32(&rt.cancels) != 1 {
		t.Error("Cancel must reach the runtime — an agent gone wrong is still spending")
	}
	if find(logLines(t, logs.String()), "agent execution cancelled") == nil {
		t.Error("a cancellation must be logged")
	}
}

func TestStatusTerminality(t *testing.T) {
	for _, s := range []Status{StatusSucceeded, StatusFailed, StatusCancelled, StatusTimedOut} {
		if !s.Terminal() {
			t.Errorf("%q must be terminal, or polling never stops", s)
		}
	}
	for _, s := range []Status{StatusQueued, StatusRunning} {
		if s.Terminal() {
			t.Errorf("%q must NOT be terminal, or we would stop polling a live execution", s)
		}
	}
}
