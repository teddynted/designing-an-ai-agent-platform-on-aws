package loop

import (
	"encoding/json"
	"time"
)

// Phase is where the loop is in its lifecycle. It is the reducer's single most important
// field: [Decide] reads it to choose the next [Action], and [Advance] writes it to move the
// loop forward. Everything else in [State] is history or accounting; the Phase is the program
// counter.
type Phase string

const (
	// PhasePlanning: the loop needs a plan. The first phase of every run, and the phase a
	// replan returns to.
	PhasePlanning Phase = "planning"

	// PhaseExecuting: a task is selected and waiting to be run.
	PhaseExecuting Phase = "executing"

	// PhaseEvaluating: a task has run and its outcome is waiting to be judged.
	PhaseEvaluating Phase = "evaluating"

	// PhaseReflecting: a task failed and the loop is working out what to change before it
	// tries again. Skipped entirely when reflection is disabled.
	PhaseReflecting Phase = "reflecting"

	// PhaseSummarising: the loop is finished with the work and needs to write its account.
	PhaseSummarising Phase = "summarising"

	// PhaseDone: the goal was achieved and summarised. Terminal.
	PhaseDone Phase = "done"

	// PhaseStopped: a stopping condition ended the loop before the goal. Terminal. The
	// difference from PhaseFailed is that a stop is a bound doing its job — max iterations, a
	// cost cap, a human needed — not a malfunction.
	PhaseStopped Phase = "stopped"

	// PhaseFailed: the loop could not continue — planning produced nothing, a stage engine
	// broke, or a task failed unrecoverably with no replan available. Terminal.
	PhaseFailed Phase = "failed"
)

// Terminal reports whether the loop has finished, one way or another. A driver stops calling
// [Decide] here.
func (p Phase) Terminal() bool {
	switch p {
	case PhaseDone, PhaseStopped, PhaseFailed:
		return true
	default:
		return false
	}
}

// State is the entire loop, as a value.
//
// It is serialisable on purpose, and that purpose is the milestone's "recovery from
// interruptions where practical". Everything the loop needs to resume is here and nothing is
// on a goroutine's stack or in a closure — so the state can be written to S3 after each step,
// and a fresh process (a new Spot instance after a reclaim, an n8n run picking up where a
// previous node left off) can load it and call [Decide] to continue as if nothing happened.
//
// The fields are the ones the milestone enumerates, and each earns its place by being
// something a decision or a recovery genuinely needs — not a log line dressed up as state.
type State struct {
	// --- identity: the chain, so any log line about this loop can be traced back ----------
	WorkflowExecutionID string `json:"workflowExecutionId,omitempty"`
	CorrelationID       string `json:"correlationId"`
	AgentID             string `json:"agentId,omitempty"`

	Goal  Goal  `json:"goal"`
	Phase Phase `json:"phase"`

	// --- the plan and where we are in it --------------------------------------------------

	// Plan is the CURRENT plan. It is replaced wholesale by a replan, not edited, so that the
	// history of what was attempted is the sequence of Completed/Failed outcomes rather than
	// a mutated plan nobody can reconstruct.
	Plan Plan `json:"plan"`

	// CurrentTaskID is the task being worked on. Empty between plans.
	CurrentTaskID string `json:"currentTaskId,omitempty"`

	// PendingOutcome and PendingEvaluation hold an action's result between [Advance]-ing the
	// phase and the next stage consuming it: the outcome an evaluator will judge, the
	// evaluation a reflector will read.
	//
	// They ARE serialised, and that is the whole recovery story. A loop persisted in
	// PhaseEvaluating has already paid for the execution whose outcome is pending — the
	// expensive, side-effecting step — and losing that outcome on a reload would force a
	// re-execution, which is the one thing recovery must avoid. So the pending result rides
	// along in the state, and a reloaded loop evaluates the outcome it already has rather than
	// re-running the agent to get it again.
	PendingOutcome    *Outcome    `json:"pendingOutcome,omitempty"`
	PendingEvaluation *Evaluation `json:"pendingEvaluation,omitempty"`

	// --- progress -------------------------------------------------------------------------

	// Completed and Failed are every outcome the loop has seen, in order. They are the
	// execution history the milestone asks for, and the raw material the summariser turns into
	// its account. A task that failed and then succeeded on retry appears in both, which is
	// correct: the failure happened and cost money, and hiding it would make the cost
	// inexplicable.
	Completed []Outcome `json:"completed,omitempty"`
	Failed    []Outcome `json:"failed,omitempty"`

	// ReflectionHistory is what the loop learned, in order. It is kept because reflection that
	// vanishes after it is applied leaves a loop that "mysteriously" changed its approach, and
	// the trail is the answer to "why did attempt three look different from attempt two?".
	ReflectionHistory []Reflection `json:"reflectionHistory,omitempty"`

	// --- accounting: the numbers the stopping conditions are enforced against --------------

	// Iterations counts execution ATTEMPTS across the whole loop — every time a task is
	// dispatched to the executor, including retries and post-replan. It is the hard spend
	// guard: max-iterations bounds it, exactly as the tool loop's DefaultMaxIterations bounds
	// the turns of a single conversation, and for the same reason — the failure mode of an
	// unbounded loop on a per-token/per-agent system is not a hang, it is a bill.
	Iterations int `json:"iterations"`

	// RetryCount is consecutive retries of the CURRENT task. It resets when the loop moves to
	// a different task or replans, because "this task has failed 3 times" and "the loop has
	// retried 3 times across 3 different tasks" are different situations, and only the first
	// should exhaust a per-task retry budget.
	RetryCount int `json:"retryCount"`

	// Replans counts how many times the plan has been rebuilt. Bounded separately, so a loop
	// cannot escape the iteration cap by replanning its way to a fresh task cursor forever.
	Replans int `json:"replans"`

	// CostUSD is the running total across every stage — executions and the reasoning steps
	// alike. The cost cap is enforced against it in [Decide].
	CostUSD float64 `json:"costUsd"`

	// StartedAt anchors the wall-clock timeout. now - StartedAt is checked in [Decide], so the
	// clock is passed in and the check stays deterministic in tests.
	StartedAt time.Time `json:"startedAt"`

	// --- the end -------------------------------------------------------------------------

	// Stop is why the loop stopped, set when Phase becomes PhaseStopped or PhaseFailed. It is
	// what [ErrStopped] wraps, and what a human reads first.
	Stop StopReason `json:"stop,omitempty"`

	// Summary is the final account, set when Phase becomes PhaseDone (or PhaseStopped, if the
	// loop got far enough to summarise). Zero until then.
	Summary *Summary `json:"summary,omitempty"`
}

// Initial builds the starting state for a goal. The loop always begins by planning.
func Initial(goal Goal, now time.Time) State {
	return State{
		WorkflowExecutionID: goal.WorkflowExecutionID,
		CorrelationID:       goal.CorrelationID,
		AgentID:             goal.AgentID,
		Goal:                goal,
		Phase:               PhasePlanning,
		StartedAt:           now,
	}
}

// CurrentTask returns the task the loop is working on, and whether there is one.
func (s State) CurrentTask() (Task, bool) {
	return s.taskByID(s.CurrentTaskID)
}

func (s State) taskByID(id string) (Task, bool) {
	for _, t := range s.Plan.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

// done reports whether a task has already completed successfully — so the loop does not
// re-run work when it resumes, and so dependency checks can tell what is finished.
func (s State) done(taskID string) bool {
	for _, o := range s.Completed {
		if o.TaskID == taskID && o.Success {
			return true
		}
	}
	return false
}

// nextTask returns the next task whose dependencies are all satisfied and which has not yet
// succeeded — the sequential scheduler. A parallel driver would take ALL such tasks at once;
// that this returns one is the only thing making the Runner sequential, and it is a
// deliberately small thing to change.
func (s State) nextTask() (Task, bool) {
	for _, t := range s.Plan.Tasks {
		if s.done(t.ID) {
			continue
		}
		ready := true
		for _, dep := range t.DependsOn {
			if !s.done(dep) {
				ready = false
				break
			}
		}
		if ready {
			return t, true
		}
	}
	return Task{}, false
}

// Marshal serialises the state for persistence between steps. It is a thin wrapper so the one
// place that writes loop state does so consistently, and so a caller does not have to know
// the state is "just JSON" — which lets that stop being true later without a caller changing.
func (s State) Marshal() ([]byte, error) {
	return json.Marshal(s)
}

// Load reconstructs a state persisted by [State.Marshal]. Resuming is: Load, then [Decide].
func Load(data []byte) (State, error) {
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, err
	}
	return s, nil
}
