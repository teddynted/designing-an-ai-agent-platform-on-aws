package loop

import "time"

// StopReason is why a loop stopped short of achieving its goal. It is an enumerated value,
// not a message, for the reason every sentinel on this platform is: an alert or a branch
// built on a message breaks the first time someone improves the wording, and these are
// exactly the things a caller needs to branch on — "a human is needed" and "the cost cap
// tripped" lead to completely different next actions.
type StopReason string

const (
	// StopNone is the zero value: not stopped.
	StopNone StopReason = ""

	// StopGoalAchieved is the happy one, and the only reason that is not a shortfall. It is
	// here so that "why did the loop stop?" always has an answer from the same field, rather
	// than the success case being an absence.
	StopGoalAchieved StopReason = "goal-achieved"

	// StopMaxIterations: the loop ran its budget of execution attempts without finishing. The
	// commonest non-trivial stop, and usually a sign the objective was too big for one loop or
	// the plan was going in circles.
	StopMaxIterations StopReason = "max-iterations"

	// StopMaxRetries: the current task failed more times than its retry budget allows, and no
	// replan was called for. The task is stuck, not merely unlucky.
	StopMaxRetries StopReason = "max-retries"

	// StopMaxReplans: the plan has been rebuilt its budgeted number of times without success.
	// This is the outer-loop analogue of StopMaxRetries — it stops a loop that "adapts" its
	// way around in circles, producing a new plan each time and never finishing one.
	StopMaxReplans StopReason = "max-replans"

	// StopTimeout: the wall-clock budget expired. Distinct from max-iterations because a loop
	// can burn its clock on one slow agent execution without taking many iterations, and
	// distinct from a task timeout (which the agent runtime enforces on its own run) because
	// this one bounds the WHOLE loop.
	StopTimeout StopReason = "timeout"

	// StopCostExceeded: the running cost passed the cap. The most important stop on a
	// per-token, per-agent-run platform, and the reason the cost total is accumulated into the
	// state rather than merely logged.
	StopCostExceeded StopReason = "cost-exceeded"

	// StopHumanRequired: the evaluator judged that a person must decide. Not a failure — a
	// deliberate hand-off, which Milestone 14 will make an n8n approval gate.
	StopHumanRequired StopReason = "human-required"

	// StopCriticalFailure: a stage engine failed unrecoverably, or a task failed in a way that
	// is neither retryable nor replannable. The loop cannot honestly continue.
	StopCriticalFailure StopReason = "critical-failure"
)

// Terminal reports whether a reason ends the loop. Every reason except [StopNone] does; the
// method exists so the check reads as intent rather than as a comparison to the empty string.
func (r StopReason) Terminal() bool { return r != StopNone }

// checkStops is the enforcement point for every configurable stopping condition. It is called
// at the very top of [Decide], before any action is chosen, on every cycle — which is what
// makes "stopping conditions must always be enforced" true by construction rather than by the
// diligence of whoever wrote each phase.
//
// The order is by severity of the thing being protected. Cost and time come first because
// they protect the bill and the caller's patience and do not care WHY the loop is busy;
// iteration and retry caps come next because they protect against a loop that is making no
// progress. Goal-achieved is checked by the reducer's phase logic, not here, because it is a
// success and not a guard — it is reached by evaluation, not by a budget running out.
func checkStops(s State, cfg Config, now time.Time) StopReason {
	// Cost first. It is the one that arrives as real money, and it must be able to stop a loop
	// even if every other counter looks healthy.
	if cfg.MaxCostUSD > 0 && s.CostUSD >= cfg.MaxCostUSD {
		return StopCostExceeded
	}

	// Then the clock. A single slow agent run can exhaust the wall-clock budget in one
	// iteration, so time is checked independently of iteration count.
	if cfg.Timeout > 0 && !s.StartedAt.IsZero() && now.Sub(s.StartedAt) >= cfg.Timeout {
		return StopTimeout
	}

	// Then the hard iteration cap: total execution attempts across the whole loop. This is the
	// backstop that guarantees termination even if every other condition is misconfigured to
	// zero (disabled), because a loop that cannot execute forever cannot run forever.
	if cfg.MaxIterations > 0 && s.Iterations >= cfg.MaxIterations {
		return StopMaxIterations
	}

	// Then replans: an outer loop that keeps producing new plans and finishing none.
	if cfg.MaxReplans > 0 && s.Replans >= cfg.MaxReplans {
		return StopMaxReplans
	}

	return StopNone
}
