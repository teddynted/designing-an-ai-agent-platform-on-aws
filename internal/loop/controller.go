package loop

import (
	"fmt"
	"time"
)

// ActionKind is what the driver should do next. It is the reducer's output alphabet — the
// complete set of things the loop can ask the outside world to do — and it is small on
// purpose, because every kind here is a place where the loop touches something expensive or
// slow, and a small alphabet is an auditable one.
type ActionKind string

const (
	// ActionPlan: produce a plan for the goal. The driver calls the [Planner].
	ActionPlan ActionKind = "plan"

	// ActionExecute: run [Action.Task]. The driver calls the [Executor] — this is the one
	// action that changes the world.
	ActionExecute ActionKind = "execute"

	// ActionEvaluate: judge [Action.Outcome]. The driver calls the [Evaluator].
	ActionEvaluate ActionKind = "evaluate"

	// ActionReflect: analyse the last failure. The driver calls the [Reflector].
	ActionReflect ActionKind = "reflect"

	// ActionSummarise: write the final account. The driver calls the [Summariser].
	ActionSummarise ActionKind = "summarise"

	// ActionStop: the loop is finished. The driver stops. [Action.Stop] says why, and it is
	// StopGoalAchieved on success — a stop is not the same as a failure.
	ActionStop ActionKind = "stop"
)

// Action is the reducer's instruction to the driver: do this one thing, then hand the result
// back through [Advance]. It carries exactly the data the driver needs to perform the action
// and nothing else — the reducer keeps the [State].
type Action struct {
	Kind ActionKind

	// Task is set for ActionExecute: which task to run.
	Task Task

	// Outcome is set for ActionEvaluate and ActionReflect: the outcome to judge, or the
	// failure to reflect on. It is carried on the action rather than read from the state so
	// that the driver is a pure function of the action — it never has to reach into the state
	// to know what to work on.
	Outcome Outcome

	// Evaluation is set for ActionReflect: the verdict the reflector is reacting to, so it
	// knows WHY the task was judged a failure and not merely that it was.
	Evaluation Evaluation

	// Delay is how long the driver should wait before performing the action. It is non-zero
	// only for a retry, where it is the backoff. The driver does the waiting — a sleep in the
	// Runner, a durable wait node in n8n — which is why it is a value the reducer emits rather
	// than a pause the reducer takes.
	Delay time.Duration

	// Stop is the reason, set for ActionStop.
	Stop StopReason

	// Reason is a human-readable note for the log, explaining why the reducer chose this
	// action. It is the loop's equivalent of the router's routing "reason": the difference
	// between "the loop executed a task" and "the loop retried task analyse-repo after a
	// transient failure, attempt 2 of 2".
	Reason string
}

// Decide chooses the next action from the current state. It is a pure function of the state,
// the config, and the clock — no I/O, no engines, no randomness — which is what makes "why
// did the loop do that?" a question answerable by a test with a struct literal.
//
// # Stopping conditions come first, always
//
// Before it looks at the phase, Decide checks every configurable stopping condition. This is
// the one place they are enforced, and putting them ahead of the phase switch is what makes
// "stopping conditions must always be enforced" structural rather than a promise each phase
// has to keep. A loop cannot begin an expensive action with its budget already blown, because
// the budget is checked on the road to every action.
func Decide(s State, cfg Config, now time.Time) Action {
	// A terminal state asks for nothing. This makes Decide safe to call on a reloaded,
	// already-finished state — a driver that resumes does not have to check first.
	if s.Phase.Terminal() {
		return Action{Kind: ActionStop, Stop: s.Stop, Reason: "the loop has already finished"}
	}

	// THE GUARD. Every configurable bound, checked before any action is chosen. If one has
	// tripped, the loop stops — but it stops by SUMMARISING first if it has not yet, so that
	// even an aborted loop produces the account a caller is owed. The one exception is a
	// summarise phase that itself tripped a bound, which stops cleanly to avoid a loop trying
	// to summarise its way out of a cost cap.
	if reason := checkStops(s, cfg, now); reason.Terminal() {
		if s.Phase == PhaseSummarising || s.Summary != nil {
			return Action{Kind: ActionStop, Stop: reason, Reason: "a stopping condition tripped: " + string(reason)}
		}
		return Action{
			Kind:   ActionSummarise,
			Stop:   reason,
			Reason: "stopping (" + string(reason) + "); summarising before we go",
		}
	}

	switch s.Phase {
	case PhasePlanning:
		return Action{Kind: ActionPlan, Reason: "no plan yet; planning the goal"}

	case PhaseExecuting:
		task, ok := s.CurrentTask()
		if !ok {
			// The phase says execute but there is no current task — this is reached only by a
			// corrupted or hand-built state, and stopping with a clear reason beats dispatching
			// a zero-value task to an executor.
			return Action{Kind: ActionStop, Stop: StopCriticalFailure, Reason: "executing phase with no current task"}
		}
		act := Action{Kind: ActionExecute, Task: task, Reason: fmt.Sprintf("executing task %q", task.ID)}
		// A retry carries its backoff. RetryCount is the number of retries already spent on
		// this task, so the delay for the attempt we are about to make is Backoff(RetryCount).
		if s.RetryCount > 0 {
			act.Delay = cfg.Retry.Backoff(s.RetryCount)
			act.Reason = fmt.Sprintf("retrying task %q (attempt %d, after %s)", task.ID, s.RetryCount+1, act.Delay)
		}
		return act

	case PhaseEvaluating:
		if s.PendingOutcome == nil {
			return Action{Kind: ActionStop, Stop: StopCriticalFailure, Reason: "evaluating phase with no outcome to judge"}
		}
		return Action{Kind: ActionEvaluate, Outcome: *s.PendingOutcome, Task: mustTask(s), Reason: "judging the last outcome"}

	case PhaseReflecting:
		if s.PendingOutcome == nil || s.PendingEvaluation == nil {
			return Action{Kind: ActionStop, Stop: StopCriticalFailure, Reason: "reflecting phase with nothing to reflect on"}
		}
		return Action{
			Kind:       ActionReflect,
			Outcome:    *s.PendingOutcome,
			Evaluation: *s.PendingEvaluation,
			Task:       mustTask(s),
			Reason:     "reflecting on the failure before retrying",
		}

	case PhaseSummarising:
		return Action{Kind: ActionSummarise, Stop: s.Stop, Reason: "work finished; writing the summary"}

	default:
		return Action{Kind: ActionStop, Stop: StopCriticalFailure, Reason: "unknown phase " + string(s.Phase)}
	}
}

// StepResult carries what an action produced back into [Advance]. Exactly one field is set,
// matching the [ActionKind] that produced it. It is the reducer's input alphabet, the mirror
// of [Action].
type StepResult struct {
	Plan       *Plan
	Outcome    *Outcome
	Evaluation *Evaluation
	Reflection *Reflection
	Summary    *Summary

	// Cost is what a REASONING step spent — a plan, an evaluation, a reflection, a summary are
	// all inference, and inference is billed. Execution cost travels on [Outcome.CostUSD]
	// instead, so the two never overlap: a reasoning result sets Cost and no Outcome, an
	// execution result sets an Outcome and no Cost. Both feed the same running total, because
	// a cost cap blind to the reasoning half of the bill is a cost cap that lies.
	Cost float64

	// EngineErr is set when the stage engine itself failed — not a task failing (that is an
	// Outcome), but the planner unreachable, the evaluator's model returning junk. It is how a
	// driver reports "I could not perform the action at all", and the reducer turns it into a
	// terminal failure rather than pretending the stage produced something.
	EngineErr error
}

// Advance folds an action's result into the next state. Pure: it takes a state and a result
// and returns a new state, touching nothing outside them. Together with [Decide] it is the
// whole controller — everything else in this package is the vocabulary these two functions
// speak or the driver that turns their conversation into real work.
func Advance(s State, cfg Config, r StepResult, now time.Time) State {
	// An engine failure is terminal for the phase that suffered it. A loop cannot plan without
	// a planner or judge without an evaluator, and a stage engine breaking is not a task
	// outcome to reason about — it is the reasoning itself being unavailable.
	if r.EngineErr != nil {
		s.Phase = PhaseFailed
		s.Stop = StopCriticalFailure
		return s
	}

	// Reasoning cost accumulates here, once, for every stage. Execution cost is added inside
	// advanceExecute from the outcome, and a reasoning result carries no outcome, so there is
	// no double count.
	s.CostUSD += r.Cost

	// Advance dispatches on the RESULT, not the phase. Exactly one result field is set, and it
	// says which action the driver performed — which is more reliable than the phase, because
	// [Decide] can legitimately ask for an action that does not match the current phase (a
	// stopping condition that trips mid-execute asks for a summary before the phase has moved
	// to summarising). Folding in "whatever came back" makes that case correct without a
	// special path, and it means a driver can never desync the reducer by performing an action
	// the phase did not strictly expect.
	switch {
	case r.Plan != nil:
		return s.advancePlan(r)
	case r.Outcome != nil:
		return s.advanceExecute(r)
	case r.Evaluation != nil:
		return s.advanceEvaluate(cfg, r)
	case r.Reflection != nil:
		return s.advanceReflect(r)
	case r.Summary != nil:
		return s.advanceSummarise(cfg, r, now)
	default:
		// An empty result with no engine error. The driver performed something and reported
		// nothing, which is a driver bug — fail rather than spin.
		s.Phase = PhaseFailed
		s.Stop = StopCriticalFailure
		return s
	}
}

func (s State) advancePlan(r StepResult) State {
	if r.Plan == nil {
		s.Phase = PhaseFailed
		s.Stop = StopCriticalFailure
		return s
	}
	if err := r.Plan.Validate(); err != nil {
		// A plan that does not validate is not a plan. Fail rather than execute a task list
		// with an unresolved dependency the loop would deadlock on.
		s.Phase = PhaseFailed
		s.Stop = StopCriticalFailure
		return s
	}

	s.Plan = *r.Plan
	// A fresh plan means a fresh task cursor: forget which task was "current" under the old
	// plan, and reset the per-task retry count. The iteration total is NOT reset — it counts
	// across the whole loop, replans included, which is what stops replanning from being an
	// escape hatch out of the iteration cap.
	s.CurrentTaskID = ""
	s.RetryCount = 0

	task, ok := s.nextTask()
	if !ok {
		// A valid plan whose every task is somehow already done. Nothing to execute; go
		// straight to summarising and let the evaluator's earlier GoalAchieved (or its
		// absence) speak through the summary.
		s.Phase = PhaseSummarising
		return s
	}
	s.CurrentTaskID = task.ID
	s.Phase = PhaseExecuting
	return s
}

func (s State) advanceExecute(r StepResult) State {
	if r.Outcome == nil {
		s.Phase = PhaseFailed
		s.Stop = StopCriticalFailure
		return s
	}
	out := *r.Outcome
	out.TaskID = s.CurrentTaskID

	// Executing is the one action that spends real, external money and time, so this is where
	// the accounting lands. Iterations counts this attempt — the hard cap is enforced against
	// it on the next Decide.
	s.Iterations++
	s.CostUSD += out.CostUSD

	// Hold the outcome for the evaluator. It is not filed into Completed/Failed yet, because
	// whether it counts as a success is the evaluator's call, not the executor's.
	s.PendingOutcome = &out
	s.Phase = PhaseEvaluating
	return s
}

func (s State) advanceEvaluate(cfg Config, r StepResult) State {
	if r.Evaluation == nil || s.PendingOutcome == nil {
		s.Phase = PhaseFailed
		s.Stop = StopCriticalFailure
		return s
	}
	eval := *r.Evaluation
	outcome := *s.PendingOutcome

	// The confidence gate. A claimed success the evaluator is not sure about is downgraded to
	// a failure worth another look — because a low-confidence pass is precisely where a
	// wrong-but-plausible result slips through into something that gets published.
	succeeded := eval.TaskSucceeded
	if succeeded && cfg.MinConfidence > 0 && eval.Confidence < cfg.MinConfidence {
		succeeded = false
		eval.TaskSucceeded = false
		eval.Retry = true
		eval.Reason = fmt.Sprintf("downgraded: confidence %.2f is below the %.2f threshold — %s",
			eval.Confidence, cfg.MinConfidence, eval.Reason)
	}

	// A human hand-off stops the loop cleanly, whatever else the evaluation says. It is one of
	// the always-enforced stops, and it is deliberate, not a failure.
	if eval.HumanRequired {
		s.file(outcome, succeeded)
		s.PendingOutcome = nil
		s.Phase = PhaseSummarising
		s.Stop = StopHumanRequired
		return s
	}

	if succeeded {
		s.file(outcome, true)
		s.PendingOutcome = nil
		s.RetryCount = 0

		if eval.GoalAchieved {
			s.Phase = PhaseSummarising
			s.Stop = StopGoalAchieved
			return s
		}
		// Task done, goal not yet. Move to the next ready task; if there is none, the plan is
		// complete but the goal was not declared achieved — summarise and let the account say
		// so, rather than looping on an empty plan.
		if _, ok := s.nextTask(); !ok {
			s.Phase = PhaseSummarising
			return s
		}
		next, _ := s.nextTask()
		s.CurrentTaskID = next.ID
		s.Phase = PhaseExecuting
		return s
	}

	// The task did not succeed. File it as a failure — it happened and it cost money — and
	// decide among retry, replan, and stop.
	s.file(outcome, false)

	if shouldRetry(outcome, eval, s.RetryCount, cfg.Retry) {
		s.RetryCount++
		if cfg.Reflection {
			// Hold both the outcome and the verdict for the reflector, which needs to know not
			// just what happened but why it was judged a failure.
			s.PendingOutcome = &outcome
			s.PendingEvaluation = &eval
			s.Phase = PhaseReflecting
		} else {
			s.PendingOutcome = nil
			s.PendingEvaluation = nil
			s.Phase = PhaseExecuting
		}
		return s
	}
	s.PendingOutcome = nil
	s.PendingEvaluation = nil

	if eval.Replan {
		// A different decomposition might work where this one did not. Bounded by MaxReplans,
		// enforced on the next Decide.
		s.Replans++
		s.RetryCount = 0
		s.CurrentTaskID = "" // between plans: there is no current task until the new plan lands
		s.Phase = PhasePlanning
		return s
	}

	// Not retryable and not replannable: the task is stuck. Whether this is "max retries" or a
	// plain critical failure depends on why we are not retrying — if the evaluator asked to
	// retry but the budget or the transient check refused, it is retry exhaustion; otherwise
	// it is a deterministic dead end.
	if eval.Retry {
		s.Stop = StopMaxRetries
	} else {
		s.Stop = StopCriticalFailure
	}
	s.Phase = PhaseSummarising
	return s
}

func (s State) advanceReflect(r StepResult) State {
	if r.Reflection == nil {
		s.Phase = PhaseFailed
		s.Stop = StopCriticalFailure
		return s
	}
	refl := *r.Reflection
	s.ReflectionHistory = append(s.ReflectionHistory, refl)

	// Reflection's concrete output: if it rewrote the instructions, apply them to the current
	// task for the next attempt. This is the loop changing its BEHAVIOUR — a sharper prompt —
	// without any code changing, and it is authored on the platform's side of the boundary,
	// never from repository content.
	if refl.RevisedInstructions != "" {
		s.Plan.Tasks = withRevisedInstructions(s.Plan.Tasks, s.CurrentTaskID, refl.RevisedInstructions)
	}
	s.PendingOutcome = nil
	s.PendingEvaluation = nil
	s.Phase = PhaseExecuting
	return s
}

func (s State) advanceSummarise(cfg Config, r StepResult, now time.Time) State {
	if r.Summary != nil {
		s.Summary = r.Summary
	}

	// Decide why the loop is ending. Some reasons are already on the state, set by the phase
	// that chose to summarise — goal achieved, a human needed, retries exhausted. A BUDGET
	// stop, though, was found by Decide's guard, which is pure and could not write it to the
	// state — so recompute it here. Between them these cover every way a loop ends.
	reason := s.Stop
	if reason == StopNone {
		reason = checkStops(s, cfg, now)
	}

	switch {
	case reason == StopGoalAchieved:
		s.Phase = PhaseDone
	case reason.Terminal():
		// A bound did its job: max iterations, a cost cap, a human hand-off. Stopped, not
		// failed — the difference an alert depends on.
		s.Phase = PhaseStopped
		s.Stop = reason
	default:
		// No bound tripped and no explicit stop: the loop finished the plan it was given
		// without anything going wrong. That is a normal, successful end.
		s.Phase = PhaseDone
	}
	return s
}

// file records an outcome as a success or a failure. A task that failed then succeeded on
// retry ends up in both lists, in order — which is the honest record: the failure happened
// and cost money.
func (s *State) file(o Outcome, success bool) {
	o.Success = success
	if success {
		s.Completed = append(s.Completed, o)
	} else {
		s.Failed = append(s.Failed, o)
	}
}

func withRevisedInstructions(tasks []Task, id, instructions string) []Task {
	out := make([]Task, len(tasks))
	copy(out, tasks)
	for i := range out {
		if out[i].ID == id {
			out[i].Instructions = instructions
		}
	}
	return out
}

// mustTask returns the current task, or a zero task if somehow absent. Decide has already
// guarded the phases that require a task, so the zero case is unreachable in practice; it
// exists so the reducer never panics on a hand-built state.
func mustTask(s State) Task {
	t, _ := s.CurrentTask()
	return t
}
