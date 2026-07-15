package adapter

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/agent"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/loop"
)

// fakeRuntime is an agent.Runtime that returns what the test scripts, with no HTTP. It lets
// the executor adapter be tested for the one thing it must get right — mapping a runtime's
// failures to loop.Outcome.Transient — without an OpenClaw anywhere.
type fakeRuntime struct {
	result agent.Result
	err    error
}

func (f *fakeRuntime) Name() string { return "fake" }
func (f *fakeRuntime) Tasks() []agent.TaskType {
	return []agent.TaskType{agent.TaskRepoAnalysis, agent.TaskBlogDraft}
}
func (f *fakeRuntime) Submit(context.Context, agent.Request) (agent.Execution, error) {
	if f.err != nil {
		return agent.Execution{}, f.err
	}
	return agent.Execution{ID: "exec-1", Status: agent.StatusQueued}, nil
}
func (f *fakeRuntime) Status(context.Context, string) (agent.Execution, error) {
	return f.result.Execution, f.err
}
func (f *fakeRuntime) Result(context.Context, string) (agent.Result, error) { return f.result, f.err }
func (f *fakeRuntime) Cancel(context.Context, string) error                 { return nil }

func newExecutor(t *testing.T, rt agent.Runtime) *Executor {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc := agent.NewService(rt, log)
	cfg := DefaultExecutorConfig()
	// Poll instantly and give up fast, so a test never actually waits on the fake.
	cfg.Wait = agent.WaitPolicy{Interval: time.Millisecond, Timeout: time.Second}
	return NewExecutor(svc, cfg)
}

func testGoal() loop.Goal {
	return loop.Goal{
		Objective:     "analyse",
		Repository:    loop.Repository{Name: "x/y", URL: "https://github.com/x/y"},
		CorrelationID: "corr-1",
	}
}

// A successful agent run becomes a successful outcome carrying the output and the cost.
func TestExecuteMapsSuccess(t *testing.T) {
	rt := &fakeRuntime{result: agent.Result{
		Execution: agent.Execution{ID: "exec-1", Status: agent.StatusSucceeded, Cost: 0.42},
		Output:    agent.Output{Content: "it is a Go platform"},
	}}
	e := newExecutor(t, rt)

	out, err := e.Execute(context.Background(), testGoal(), loop.Task{ID: "a", Type: "repo-analysis", Instructions: "go"}, 0)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Success || out.Output != "it is a Go platform" {
		t.Errorf("outcome = %+v, want a success carrying the output", out)
	}
	if out.CostUSD != 0.42 || out.ExecutionID != "exec-1" {
		t.Errorf("outcome = %+v, want the cost and execution id carried through", out)
	}
}

// THE test of the executor adapter: the runtime's errors are mapped to Transient correctly,
// because the loop's whole retry decision rests on that one boolean.
func TestExecuteMapsFailuresToTransience(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantTransient bool
	}{
		{"runtime unreachable is transient", agent.ErrUnavailable, true},
		{"a timeout is transient", agent.ErrTimeout, true},
		{"retries exhausted is transient", agent.ErrRetriesExhausted, true},
		{"the agent ran and failed is NOT transient", agent.ErrAgentFailed, false},
		{"output rejected is NOT transient", agent.ErrOutputRejected, false},
		{"unauthorized is NOT transient", agent.ErrUnauthorized, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRuntime{
				result: agent.Result{Execution: agent.Execution{ID: "exec-1", Status: agent.StatusFailed}},
				err:    tc.err,
			}
			e := newExecutor(t, rt)

			out, err := e.Execute(context.Background(), testGoal(), loop.Task{ID: "a", Type: "repo-analysis", Instructions: "go"}, 0)
			if err != nil {
				t.Fatalf("Execute returned an error; a failed run should be an Outcome: %v", err)
			}
			if out.Success {
				t.Fatal("a failed run must not be a successful outcome")
			}
			if out.Transient != tc.wantTransient {
				t.Errorf("transient = %v, want %v — the retry decision depends on this", out.Transient, tc.wantTransient)
			}
		})
	}
}

// A task type no runtime can perform is a DETERMINISTIC failure — retrying cannot conjure an
// agent — so it is a non-transient outcome the loop will not retry.
func TestExecuteRejectsAnUnknownTaskType(t *testing.T) {
	rt := &fakeRuntime{}
	e := newExecutor(t, rt)

	out, err := e.Execute(context.Background(), testGoal(), loop.Task{ID: "a", Type: "quantum-compute", Instructions: "go"}, 0)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Success || out.Transient {
		t.Errorf("outcome = %+v, want a non-transient failure for an impossible task type", out)
	}
}

// The correlation folds in the task and attempt, so two attempts at the same task are two
// distinct agent executions rather than one idempotent replay.
func TestExecuteMakesEachAttemptDistinct(t *testing.T) {
	var seen []string
	rt := &recordingRuntime{onSubmit: func(req agent.Request) { seen = append(seen, req.IdempotencyKey()) }}
	e := newExecutor(t, rt)

	task := loop.Task{ID: "a", Type: "repo-analysis", Instructions: "go"}
	_, _ = e.Execute(context.Background(), testGoal(), task, 0)
	_, _ = e.Execute(context.Background(), testGoal(), task, 1) // a retry: attempt 1

	if len(seen) != 2 || seen[0] == seen[1] {
		t.Errorf("idempotency keys = %v, want two DISTINCT keys so the retry is a fresh execution", seen)
	}
}

type recordingRuntime struct {
	fakeRuntime
	onSubmit func(agent.Request)
}

func (r *recordingRuntime) Submit(ctx context.Context, req agent.Request) (agent.Execution, error) {
	r.onSubmit(req)
	return agent.Execution{ID: "exec", Status: agent.StatusSucceeded}, nil
}
func (r *recordingRuntime) Status(context.Context, string) (agent.Execution, error) {
	return agent.Execution{ID: "exec", Status: agent.StatusSucceeded}, nil
}
func (r *recordingRuntime) Result(context.Context, string) (agent.Result, error) {
	return agent.Result{Execution: agent.Execution{ID: "exec", Status: agent.StatusSucceeded}}, nil
}
