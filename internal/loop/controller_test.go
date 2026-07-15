package loop

import (
	"testing"
	"time"
)

// The reducer is two pure functions, so its test is a table: build a state, call Decide or
// Advance, assert the result. No engines, no runner, no I/O. This is the "each stage
// independently testable" requirement made literal — and the reason the reducer was written
// as a reducer in the first place.

var epoch = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

// testConfig is a small, fully-enabled config for the reducer tests.
func testConfig() Config {
	return Config{
		MaxIterations: 12,
		MaxReplans:    2,
		Timeout:       30 * time.Minute,
		MaxCostUSD:    5,
		Reflection:    true,
		MinConfidence: 0,
		Retry:         RetryPolicy{MaxRetries: 2, BaseDelay: time.Second, MaxDelay: time.Minute, Multiplier: 2},
	}
}

func goal() Goal {
	return Goal{
		Objective:     "Draft a post about the changes",
		Repository:    Repository{URL: "https://github.com/x/y"},
		CorrelationID: "corr-1",
	}
}

// onePlan is a two-task plan, the second depending on the first.
func onePlan() Plan {
	return Plan{
		Rationale: "analyse then write",
		Tasks: []Task{
			{ID: "analyse", Type: "repo-analysis", Instructions: "read the repo"},
			{ID: "write", Type: "blog-draft", Instructions: "draft the post", DependsOn: []string{"analyse"}},
		},
	}
}

// --- the happy path, one full turn of the loop -----------------------------------------

// The reducer walks planning → executing → evaluating → done, and each transition is a pure
// step. This is the lifecycle the milestone diagrams, asserted.
func TestTheHappyPathWalksTheLifecycle(t *testing.T) {
	cfg := testConfig()
	s := Initial(goal(), epoch)

	// 1. Planning.
	if a := Decide(s, cfg, epoch); a.Kind != ActionPlan {
		t.Fatalf("first action = %q, want plan", a.Kind)
	}
	p := onePlan()
	s = Advance(s, cfg, StepResult{Plan: &p}, epoch)
	if s.Phase != PhaseExecuting || s.CurrentTaskID != "analyse" {
		t.Fatalf("after planning: phase=%q task=%q, want executing/analyse", s.Phase, s.CurrentTaskID)
	}

	// 2. Execute the first task.
	a := Decide(s, cfg, epoch)
	if a.Kind != ActionExecute || a.Task.ID != "analyse" {
		t.Fatalf("action = %q task=%q, want execute/analyse", a.Kind, a.Task.ID)
	}
	s = Advance(s, cfg, StepResult{Outcome: &Outcome{Success: true, Output: "it is a Go platform", CostUSD: 0.1}}, epoch)
	if s.Phase != PhaseEvaluating {
		t.Fatalf("after execute: phase=%q, want evaluating", s.Phase)
	}
	if s.Iterations != 1 {
		t.Errorf("iterations = %d, want 1 (the execute counted)", s.Iterations)
	}
	if s.CostUSD != 0.1 {
		t.Errorf("cost = %v, want 0.1 (the execution's cost accumulated)", s.CostUSD)
	}

	// 3. Evaluate: task succeeded, goal not yet — move to the next task.
	if a := Decide(s, cfg, epoch); a.Kind != ActionEvaluate {
		t.Fatalf("action = %q, want evaluate", a.Kind)
	}
	s = Advance(s, cfg, StepResult{Evaluation: &Evaluation{TaskSucceeded: true, GoalAchieved: false, Confidence: 0.9}}, epoch)
	if s.Phase != PhaseExecuting || s.CurrentTaskID != "write" {
		t.Fatalf("after eval: phase=%q task=%q, want executing/write (deps satisfied)", s.Phase, s.CurrentTaskID)
	}
	if len(s.Completed) != 1 {
		t.Errorf("completed = %d, want 1", len(s.Completed))
	}

	// 4. Execute + evaluate the second task, this time achieving the goal.
	s = Advance(s, cfg, StepResult{Outcome: &Outcome{Success: true, Output: "# The Post"}}, epoch)
	s = Advance(s, cfg, StepResult{Evaluation: &Evaluation{TaskSucceeded: true, GoalAchieved: true, Confidence: 0.95}}, epoch)
	if s.Phase != PhaseSummarising || s.Stop != StopGoalAchieved {
		t.Fatalf("after goal met: phase=%q stop=%q, want summarising/goal-achieved", s.Phase, s.Stop)
	}

	// 5. Summarise → done.
	if a := Decide(s, cfg, epoch); a.Kind != ActionSummarise {
		t.Fatalf("action = %q, want summarise", a.Kind)
	}
	s = Advance(s, cfg, StepResult{Summary: &Summary{Outcome: "achieved"}}, epoch)
	if s.Phase != PhaseDone {
		t.Fatalf("final phase = %q, want done", s.Phase)
	}
}

// --- retry and reflection --------------------------------------------------------------

// A transient failure the evaluator wants retried goes through reflection (when enabled) and
// back to execution, with a backoff and an incremented retry count.
func TestATransientFailureIsReflectedThenRetried(t *testing.T) {
	cfg := testConfig()
	s := stateExecuting(cfg)

	// The task fails transiently.
	s = Advance(s, cfg, StepResult{Outcome: &Outcome{Success: false, Transient: true, Error: "runtime blip"}}, epoch)
	// The evaluator judges it failed and asks to retry.
	s = Advance(s, cfg, StepResult{Evaluation: &Evaluation{TaskSucceeded: false, Retry: true, Confidence: 0.8, Reason: "blip"}}, epoch)

	if s.Phase != PhaseReflecting {
		t.Fatalf("phase = %q, want reflecting (reflection is enabled)", s.Phase)
	}
	if s.RetryCount != 1 {
		t.Errorf("retryCount = %d, want 1", s.RetryCount)
	}
	if len(s.Failed) != 1 {
		t.Errorf("failed = %d, want 1 — the failure is recorded even though we will retry", len(s.Failed))
	}

	// Reflect, revising the instructions.
	a := Decide(s, cfg, epoch)
	if a.Kind != ActionReflect || a.Evaluation.Reason != "blip" {
		t.Fatalf("action = %q, want reflect carrying the evaluation", a.Kind)
	}
	s = Advance(s, cfg, StepResult{Reflection: &Reflection{Adjustment: "narrow it", RevisedInstructions: "read only the Go files"}}, epoch)
	if s.Phase != PhaseExecuting {
		t.Fatalf("phase = %q, want executing (retry)", s.Phase)
	}
	if len(s.ReflectionHistory) != 1 {
		t.Errorf("reflectionHistory = %d, want 1", len(s.ReflectionHistory))
	}

	// The revised instructions were applied to the task.
	task, _ := s.CurrentTask()
	if task.Instructions != "read only the Go files" {
		t.Errorf("task instructions = %q, want the revised ones — this is reflection changing behaviour", task.Instructions)
	}

	// And the retry carries a backoff delay.
	if a := Decide(s, cfg, epoch); a.Delay != cfg.Retry.Backoff(1) {
		t.Errorf("retry delay = %v, want the backoff for attempt 1", a.Delay)
	}
}

// A DETERMINISTIC failure is never retried, however much the evaluator wants it — asking a
// model the same impossible thing again gets the same answer and a second bill.
func TestADeterministicFailureIsNotRetried(t *testing.T) {
	cfg := testConfig()
	s := stateExecuting(cfg)

	s = Advance(s, cfg, StepResult{Outcome: &Outcome{Success: false, Transient: false, Error: "the objective is impossible"}}, epoch)
	s = Advance(s, cfg, StepResult{Evaluation: &Evaluation{TaskSucceeded: false, Retry: true, Confidence: 0.9}}, epoch)

	if s.Phase != PhaseSummarising {
		t.Fatalf("phase = %q, want summarising — a deterministic failure stops the loop", s.Phase)
	}
	if s.Stop != StopMaxRetries {
		// The evaluator asked to retry and was refused by the transient check, which the loop
		// reports as retry exhaustion rather than a bare critical failure.
		t.Errorf("stop = %q, want max-retries", s.Stop)
	}
}

// Reflection disabled: a retryable failure goes straight back to execution, skipping the
// reflect phase entirely.
func TestWithReflectionDisabledARetryGoesStraightBack(t *testing.T) {
	cfg := testConfig()
	cfg.Reflection = false
	s := stateExecuting(cfg)

	s = Advance(s, cfg, StepResult{Outcome: &Outcome{Success: false, Transient: true}}, epoch)
	s = Advance(s, cfg, StepResult{Evaluation: &Evaluation{TaskSucceeded: false, Retry: true, Confidence: 0.8}}, epoch)

	if s.Phase != PhaseExecuting {
		t.Fatalf("phase = %q, want executing (no reflect stage)", s.Phase)
	}
	if s.RetryCount != 1 {
		t.Errorf("retryCount = %d, want 1", s.RetryCount)
	}
}

// The retry budget is finite: once exhausted, the loop stops rather than retrying forever.
func TestTheRetryBudgetIsFinite(t *testing.T) {
	cfg := testConfig()
	cfg.Reflection = false
	cfg.Retry.MaxRetries = 1
	s := stateExecuting(cfg)

	fail := func(st State) State {
		st = Advance(st, cfg, StepResult{Outcome: &Outcome{Success: false, Transient: true}}, epoch)
		return Advance(st, cfg, StepResult{Evaluation: &Evaluation{TaskSucceeded: false, Retry: true, Confidence: 0.8}}, epoch)
	}

	s = fail(s) // attempt 1 fails → retry (budget 1)
	if s.Phase != PhaseExecuting || s.RetryCount != 1 {
		t.Fatalf("after first fail: phase=%q retries=%d, want executing/1", s.Phase, s.RetryCount)
	}
	s = fail(s) // attempt 2 fails → budget exhausted
	if s.Phase != PhaseSummarising || s.Stop != StopMaxRetries {
		t.Fatalf("after budget exhausted: phase=%q stop=%q, want summarising/max-retries", s.Phase, s.Stop)
	}
}

// --- replanning ------------------------------------------------------------------------

func TestAnEvaluatorCanAskToReplan(t *testing.T) {
	cfg := testConfig()
	s := stateExecuting(cfg)

	s = Advance(s, cfg, StepResult{Outcome: &Outcome{Success: false, Transient: false}}, epoch)
	s = Advance(s, cfg, StepResult{Evaluation: &Evaluation{TaskSucceeded: false, Replan: true, Confidence: 0.7}}, epoch)

	if s.Phase != PhasePlanning {
		t.Fatalf("phase = %q, want planning (replan requested)", s.Phase)
	}
	if s.Replans != 1 {
		t.Errorf("replans = %d, want 1", s.Replans)
	}
	if s.CurrentTaskID != "" {
		t.Errorf("current task = %q, want cleared for the new plan", s.CurrentTaskID)
	}
}

// --- the confidence gate ---------------------------------------------------------------

// A low-confidence "success" is downgraded to a retryable failure — the gate that stops a
// wrong-but-plausible result being waved through.
func TestALowConfidenceSuccessIsDowngraded(t *testing.T) {
	cfg := testConfig()
	cfg.MinConfidence = 0.8
	s := stateExecuting(cfg)

	s = Advance(s, cfg, StepResult{Outcome: &Outcome{Success: true, Transient: true}}, epoch)
	s = Advance(s, cfg, StepResult{Evaluation: &Evaluation{TaskSucceeded: true, GoalAchieved: false, Confidence: 0.5}}, epoch)

	// Confidence 0.5 < threshold 0.8, so the "success" becomes a retry.
	if s.Phase == PhaseExecuting && s.CurrentTaskID == "write" {
		t.Fatal("the low-confidence success was accepted and the loop advanced to the next task")
	}
	if len(s.Completed) != 0 || len(s.Failed) != 1 {
		t.Errorf("completed=%d failed=%d, want the downgraded success recorded as a failure",
			len(s.Completed), len(s.Failed))
	}
}

// --- stopping conditions: the guard that runs before every action -----------------------

// Decide checks the bounds BEFORE choosing an action, so a blown budget stops the loop no
// matter what phase it is in. This is the "always enforced" requirement.
func TestStoppingConditionsAreCheckedBeforeAnyAction(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(State, Config) (State, Config)
		now    time.Time
		want   StopReason
	}{
		{
			name:   "cost cap",
			mutate: func(s State, c Config) (State, Config) { s.CostUSD = 10; c.MaxCostUSD = 5; return s, c },
			now:    epoch,
			want:   StopCostExceeded,
		},
		{
			name:   "timeout",
			mutate: func(s State, c Config) (State, Config) { c.Timeout = time.Minute; return s, c },
			now:    epoch.Add(2 * time.Minute),
			want:   StopTimeout,
		},
		{
			name:   "max iterations",
			mutate: func(s State, c Config) (State, Config) { s.Iterations = 12; c.MaxIterations = 12; return s, c },
			now:    epoch,
			want:   StopMaxIterations,
		},
		{
			name:   "max replans",
			mutate: func(s State, c Config) (State, Config) { s.Replans = 2; c.MaxReplans = 2; return s, c },
			now:    epoch,
			want:   StopMaxReplans,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			s := stateExecuting(cfg) // mid-loop, about to execute an expensive task
			s, cfg = tc.mutate(s, cfg)

			a := Decide(s, cfg, tc.now)
			// It must NOT execute — it must head for the exit (summarise, then stop).
			if a.Kind == ActionExecute {
				t.Fatalf("the loop executed a task with %s tripped — the bound was not enforced before the action", tc.want)
			}
			if a.Stop != tc.want {
				t.Errorf("stop = %q, want %q", a.Stop, tc.want)
			}
		})
	}
}

// Even a stopped loop summarises first, so a caller always gets an account.
func TestAStoppedLoopSummarisesBeforeStopping(t *testing.T) {
	cfg := testConfig()
	cfg.MaxIterations = 1
	s := stateExecuting(cfg)
	s.Iterations = 1 // already at the cap

	a := Decide(s, cfg, epoch)
	if a.Kind != ActionSummarise {
		t.Fatalf("action = %q, want summarise — a stopped loop still owes a summary", a.Kind)
	}

	// After summarising, THEN it stops.
	s = Advance(s, cfg, StepResult{Summary: &Summary{Outcome: "stopped"}}, epoch)
	if s.Phase != PhaseStopped {
		t.Fatalf("phase = %q, want stopped", s.Phase)
	}
	if a := Decide(s, cfg, epoch); a.Kind != ActionStop {
		t.Errorf("action = %q, want stop", a.Kind)
	}
}

// A human hand-off stops the loop cleanly, whatever else the evaluation said.
func TestAHumanHandoffStops(t *testing.T) {
	cfg := testConfig()
	s := stateExecuting(cfg)

	s = Advance(s, cfg, StepResult{Outcome: &Outcome{Success: true}}, epoch)
	s = Advance(s, cfg, StepResult{Evaluation: &Evaluation{TaskSucceeded: true, HumanRequired: true, Confidence: 0.9}}, epoch)

	if s.Stop != StopHumanRequired {
		t.Errorf("stop = %q, want human-required", s.Stop)
	}
	if s.Phase != PhaseSummarising {
		t.Errorf("phase = %q, want summarising", s.Phase)
	}
}

// --- engine failure and bad plans ------------------------------------------------------

func TestAnEngineFailureIsTerminal(t *testing.T) {
	cfg := testConfig()
	s := Initial(goal(), epoch)
	s = Advance(s, cfg, StepResult{EngineErr: ErrEngine}, epoch)
	if s.Phase != PhaseFailed || s.Stop != StopCriticalFailure {
		t.Errorf("phase=%q stop=%q, want failed/critical-failure", s.Phase, s.Stop)
	}
}

func TestAnInvalidPlanIsRejected(t *testing.T) {
	cfg := testConfig()
	s := Initial(goal(), epoch)
	// A plan whose task depends on one that does not exist.
	bad := Plan{Tasks: []Task{{ID: "a", Type: "x", Instructions: "y", DependsOn: []string{"ghost"}}}}
	s = Advance(s, cfg, StepResult{Plan: &bad}, epoch)
	if s.Phase != PhaseFailed {
		t.Errorf("phase = %q, want failed — an unresolved dependency would deadlock the loop", s.Phase)
	}
}

// --- recovery: the state round-trips through JSON ---------------------------------------

// The whole recovery story: a mid-loop state serialises and reloads, and the reloaded state
// decides the same next action — including the pending outcome, which must survive so a
// reload does not re-run the expensive execution.
func TestAMidLoopStateSurvivesSerialisation(t *testing.T) {
	cfg := testConfig()
	s := stateExecuting(cfg)
	// Advance to evaluating, so there is a pending outcome to preserve.
	s = Advance(s, cfg, StepResult{Outcome: &Outcome{Success: true, Output: "expensive result", CostUSD: 0.5}}, epoch)
	if s.PendingOutcome == nil {
		t.Fatal("precondition: expected a pending outcome")
	}

	data, err := s.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	reloaded, err := Load(data)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if reloaded.PendingOutcome == nil || reloaded.PendingOutcome.Output != "expensive result" {
		t.Fatal("the pending outcome did not survive the round trip — a reload would re-run the " +
			"expensive execution, which is the one thing recovery must avoid")
	}
	// And the reloaded state decides the same next action.
	before := Decide(s, cfg, epoch)
	after := Decide(reloaded, cfg, epoch)
	if before.Kind != after.Kind {
		t.Errorf("reloaded state decided %q, original decided %q", after.Kind, before.Kind)
	}
}

// stateExecuting returns a state that has planned and is about to execute the first task.
func stateExecuting(cfg Config) State {
	s := Initial(goal(), epoch)
	p := onePlan()
	return Advance(s, cfg, StepResult{Plan: &p}, epoch)
}
