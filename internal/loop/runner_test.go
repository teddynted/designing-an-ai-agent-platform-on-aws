package loop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// The Runner is tested by driving it against FAKE engines — functions that return whatever
// the test wants, with no model and no agent runtime. That the whole loop can be exercised
// this way is the point of the engine interfaces: a test of the loop needs no Claude and no
// OpenClaw, only a struct that says "the plan is this, the outcome is that".

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fakeEngines lets each stage be scripted. A nil function means the stage is never expected to
// be called, and calling it fails the test loudly rather than returning a zero value that
// would quietly send the loop somewhere wrong.
type fakeEngines struct {
	t         *testing.T
	plan      func(int) (Plan, error)
	execute   func(Task) (Outcome, error)
	evaluate  func(Task, Outcome) (Evaluation, error)
	reflect   func(Task) (Reflection, error)
	summarise func(State) (Summary, error)

	plans, executes, evaluates, reflects, summarises int
}

func (f *fakeEngines) Plan(_ context.Context, _ Goal) (Plan, error) {
	if f.plan == nil {
		f.t.Fatal("Plan called unexpectedly")
	}
	p, err := f.plan(f.plans)
	f.plans++
	return p, err
}
func (f *fakeEngines) Execute(_ context.Context, _ Goal, task Task, _ int) (Outcome, error) {
	f.executes++
	return f.execute(task)
}
func (f *fakeEngines) Evaluate(_ context.Context, _ Goal, task Task, o Outcome) (Evaluation, error) {
	f.evaluates++
	return f.evaluate(task, o)
}
func (f *fakeEngines) Reflect(_ context.Context, _ Goal, task Task, _ Outcome, _ Evaluation) (Reflection, error) {
	f.reflects++
	if f.reflect == nil {
		return Reflection{}, nil
	}
	return f.reflect(task)
}
func (f *fakeEngines) Summarise(_ context.Context, _ Goal, s State) (Summary, error) {
	f.summarises++
	if f.summarise == nil {
		return Summary{Outcome: string(s.Stop)}, nil
	}
	return f.summarise(s)
}

func (f *fakeEngines) engines() Engines {
	return Engines{Planner: f, Executor: f, Evaluator: f, Reflector: f, Summariser: f}
}

// noSleep makes backoff instantaneous, so a test that exercises retries does not actually wait.
func noSleep(context.Context, time.Duration) error { return nil }

func newTestRunner(t *testing.T, f *fakeEngines, cfg Config) *Runner {
	t.Helper()
	r, err := NewRunner(f.engines(), cfg, discard(), WithSleep(noSleep), WithClock(func() time.Time { return epoch }))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// The whole loop, end to end, through the Runner: a one-task plan that succeeds and achieves
// the goal. This is the milestone's lifecycle driven for real, against fakes.
func TestRunnerDrivesTheLoopToCompletion(t *testing.T) {
	f := &fakeEngines{
		t: t,
		plan: func(int) (Plan, error) {
			return Plan{Tasks: []Task{{ID: "a", Type: "repo-analysis", Instructions: "go"}}}, nil
		},
		execute: func(Task) (Outcome, error) {
			return Outcome{Success: true, Output: "done", CostUSD: 0.2}, nil
		},
		evaluate: func(Task, Outcome) (Evaluation, error) {
			return Evaluation{TaskSucceeded: true, GoalAchieved: true, Confidence: 0.9}, nil
		},
		summarise: func(State) (Summary, error) { return Summary{Outcome: "achieved", Result: "done"}, nil },
	}
	r := newTestRunner(t, f, testConfig())

	s, err := r.Run(context.Background(), goal())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.Phase != PhaseDone || s.Stop != StopGoalAchieved {
		t.Fatalf("final: phase=%q stop=%q, want done/goal-achieved", s.Phase, s.Stop)
	}
	if f.plans != 1 || f.executes != 1 || f.evaluates != 1 || f.summarises != 1 {
		t.Errorf("stage calls: plan=%d exec=%d eval=%d summ=%d, want 1 each",
			f.plans, f.executes, f.evaluates, f.summarises)
	}
	if s.Summary == nil || s.Summary.Result != "done" {
		t.Errorf("summary = %+v, want the deliverable", s.Summary)
	}
}

// A task that fails transiently, is reflected on, and succeeds on retry. The reflection's
// revised instructions reach the executor on the second attempt.
func TestRunnerRetriesWithReflection(t *testing.T) {
	var sawInstructions []string
	f := &fakeEngines{
		t: t,
		plan: func(int) (Plan, error) {
			return Plan{Tasks: []Task{{ID: "a", Type: "t", Instructions: "first try"}}}, nil
		},
		execute: func(task Task) (Outcome, error) {
			sawInstructions = append(sawInstructions, task.Instructions)
			if len(sawInstructions) == 1 {
				return Outcome{Success: false, Transient: true, Error: "blip"}, nil
			}
			return Outcome{Success: true, Output: "recovered"}, nil
		},
		evaluate: func(_ Task, o Outcome) (Evaluation, error) {
			if o.Success {
				return Evaluation{TaskSucceeded: true, GoalAchieved: true, Confidence: 0.9}, nil
			}
			return Evaluation{TaskSucceeded: false, Retry: true, Confidence: 0.8, Reason: "blip"}, nil
		},
		reflect: func(Task) (Reflection, error) {
			return Reflection{Adjustment: "try harder", RevisedInstructions: "second try"}, nil
		},
	}
	r := newTestRunner(t, f, testConfig())

	s, err := r.Run(context.Background(), goal())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.Phase != PhaseDone {
		t.Fatalf("phase = %q, want done", s.Phase)
	}
	if f.reflects != 1 {
		t.Errorf("reflects = %d, want 1", f.reflects)
	}
	if len(sawInstructions) != 2 || sawInstructions[1] != "second try" {
		t.Errorf("executor saw %v, want the revised instructions on the retry", sawInstructions)
	}
}

// The Runner cannot loop forever: a task that always fails transiently is retried its budget
// and then the loop stops. This is the milestone's "prevent infinite retry loops", exercised
// end to end rather than argued.
func TestRunnerCannotLoopForever(t *testing.T) {
	cfg := testConfig()
	cfg.Reflection = false
	cfg.Retry.MaxRetries = 3
	cfg.MaxIterations = 100 // deliberately high, so it is the RETRY budget that stops it

	f := &fakeEngines{
		t:       t,
		plan:    func(int) (Plan, error) { return Plan{Tasks: []Task{{ID: "a", Type: "t", Instructions: "go"}}}, nil },
		execute: func(Task) (Outcome, error) { return Outcome{Success: false, Transient: true}, nil },
		evaluate: func(Task, Outcome) (Evaluation, error) {
			return Evaluation{TaskSucceeded: false, Retry: true, Confidence: 0.8}, nil
		},
	}
	r := newTestRunner(t, f, cfg)

	s, err := r.Run(context.Background(), goal())
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
	if s.Stop != StopMaxRetries {
		t.Errorf("stop = %q, want max-retries", s.Stop)
	}
	// 1 initial attempt + 3 retries = 4 executions. Not unbounded.
	if f.executes != 4 {
		t.Errorf("executes = %d, want 4 (1 + 3 retries)", f.executes)
	}
}

// The hard iteration cap stops a loop even if nothing else would — the backstop that
// guarantees termination.
func TestRunnerRespectsTheIterationCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxIterations = 3
	cfg.Reflection = false
	cfg.Retry.MaxRetries = 100 // so retries would otherwise run forever

	f := &fakeEngines{
		t:       t,
		plan:    func(int) (Plan, error) { return Plan{Tasks: []Task{{ID: "a", Type: "t", Instructions: "go"}}}, nil },
		execute: func(Task) (Outcome, error) { return Outcome{Success: false, Transient: true}, nil },
		evaluate: func(Task, Outcome) (Evaluation, error) {
			return Evaluation{TaskSucceeded: false, Retry: true, Confidence: 0.8}, nil
		},
	}
	r := newTestRunner(t, f, cfg)

	s, _ := r.Run(context.Background(), goal())
	if s.Stop != StopMaxIterations {
		t.Errorf("stop = %q, want max-iterations", s.Stop)
	}
	if f.executes != 3 {
		t.Errorf("executes = %d, want exactly the iteration cap of 3", f.executes)
	}
	// Even a capped-out loop produced a summary.
	if s.Summary == nil {
		t.Error("a stopped loop still owes a summary, and did not produce one")
	}
}

// An executor ERROR (not a failed outcome) is turned into a transient failed outcome, so the
// loop's retry machinery gets a chance rather than the loop dying on the spot.
func TestAnExecutorErrorBecomesATransientFailure(t *testing.T) {
	cfg := testConfig()
	cfg.Reflection = false
	cfg.Retry.MaxRetries = 1

	calls := 0
	f := &fakeEngines{
		t:    t,
		plan: func(int) (Plan, error) { return Plan{Tasks: []Task{{ID: "a", Type: "t", Instructions: "go"}}}, nil },
		execute: func(Task) (Outcome, error) {
			calls++
			if calls == 1 {
				return Outcome{}, errors.New("runtime unreachable")
			}
			return Outcome{Success: true, Output: "ok"}, nil
		},
		evaluate: func(_ Task, o Outcome) (Evaluation, error) {
			if o.Success {
				return Evaluation{TaskSucceeded: true, GoalAchieved: true, Confidence: 0.9}, nil
			}
			return Evaluation{TaskSucceeded: false, Retry: true, Confidence: 0.8}, nil
		},
	}
	r := newTestRunner(t, f, cfg)

	s, err := r.Run(context.Background(), goal())
	if err != nil {
		t.Fatalf("Run: %v — an executor error should have been retried, not killed the loop", err)
	}
	if s.Phase != PhaseDone {
		t.Errorf("phase = %q, want done (recovered on retry)", s.Phase)
	}
}

// A summariser failure does not turn an achieved goal into a failure — the loop synthesises a
// plain summary and still reports success.
func TestASummariserFailureDoesNotFailTheLoop(t *testing.T) {
	f := &fakeEngines{
		t:       t,
		plan:    func(int) (Plan, error) { return Plan{Tasks: []Task{{ID: "a", Type: "t", Instructions: "go"}}}, nil },
		execute: func(Task) (Outcome, error) { return Outcome{Success: true, Output: "the deliverable"}, nil },
		evaluate: func(Task, Outcome) (Evaluation, error) {
			return Evaluation{TaskSucceeded: true, GoalAchieved: true, Confidence: 0.9}, nil
		},
		summarise: func(State) (Summary, error) {
			return Summary{}, errors.New("summariser model down")
		},
	}
	r := newTestRunner(t, f, testConfig())

	s, err := r.Run(context.Background(), goal())
	if err != nil {
		t.Fatalf("Run: %v — a summariser failure must not fail an achieved goal", err)
	}
	if s.Phase != PhaseDone {
		t.Errorf("phase = %q, want done", s.Phase)
	}
	if s.Summary == nil || s.Summary.Result != "the deliverable" {
		t.Errorf("summary = %+v, want a synthesised one carrying the deliverable", s.Summary)
	}
}

// A planning engine failure is terminal — a loop with no plan cannot proceed.
func TestAPlanningFailureFailsTheLoop(t *testing.T) {
	f := &fakeEngines{
		t:    t,
		plan: func(int) (Plan, error) { return Plan{}, errors.New("planner unreachable") },
	}
	r := newTestRunner(t, f, testConfig())

	s, err := r.Run(context.Background(), goal())
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
	if s.Phase != PhaseFailed {
		t.Errorf("phase = %q, want failed", s.Phase)
	}
}

// The Runner refuses an incoherent configuration at construction, not at the first failure.
func TestNewRunnerRefusesReflectionWithoutAReflector(t *testing.T) {
	engines := Engines{
		Planner:    &fakeEngines{},
		Executor:   &fakeEngines{},
		Evaluator:  &fakeEngines{},
		Summariser: &fakeEngines{},
		Reflector:  nil, // missing
	}
	cfg := testConfig()
	cfg.Reflection = true

	if _, err := NewRunner(engines, cfg, discard()); !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig — reflection enabled with no reflector is incoherent", err)
	}
}

// A goal that does not validate never starts.
func TestRunRejectsAnInvalidGoal(t *testing.T) {
	f := &fakeEngines{t: t}
	r := newTestRunner(t, f, testConfig())

	if _, err := r.Run(context.Background(), Goal{Objective: ""}); !errors.Is(err, ErrInvalidGoal) {
		t.Fatalf("err = %v, want ErrInvalidGoal", err)
	}
	if f.plans != 0 {
		t.Error("an invalid goal reached the planner")
	}
}
