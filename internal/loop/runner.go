package loop

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Runner drives the reducer to completion by performing each [Action] the reducer asks for,
// synchronously, in this process.
//
// # Where NOT to run this
//
// The same warning agent.Service.Wait carries, for the same reason. A loop performs agent
// executions, and an agent execution takes minutes to hours; a Runner blocks on each one. So
// **do not run a Runner in a Lambda, an HTTP handler, or the webhook path** — it would hold a
// request-scoped process open for the length of an agentic loop, and lose the whole run when
// that process is reclaimed (a Spot instance, with two minutes' notice).
//
// The Runner is the SYNCHRONOUS driver, for the CLI and for tests — the caller that has, in
// agent.Service.Wait's phrase, "genuinely thought about it". The production driver is n8n:
// it calls [Decide], submits the agent execution the action asks for, lets a durable wait
// node poll it, and calls [Advance] with the outcome — across a run that may last hours, on
// infrastructure built to survive restarts. That driver is not code in this repository; it is
// an n8n workflow, exactly as the waiting for a single agent execution is (see AGENTS.md).
// The reducer is written so that both drivers call the same two functions.
//
// # Why the logging lives here and not in the reducer
//
// The reducer is pure and must stay pure — it cannot log, because logging is I/O and a pure
// function does no I/O. So structured, CloudWatch-shaped logging lives in the one place that
// performs the loop's I/O anyway: here. Every stage gets logged identically, with the same
// correlation fields, because they all pass through this one method — the same reason
// llm.Service and agent.Service centralise their logging rather than letting each provider or
// runtime invent its own shape.
type Runner struct {
	engines Engines
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
}

// Option customises a Runner.
type Option func(*Runner)

// WithClock replaces the clock. Tests use it; nothing else should.
func WithClock(now func() time.Time) Option {
	return func(r *Runner) { r.now = now }
}

// WithSleep replaces the backoff sleep. Tests use it to make backoff instantaneous.
func WithSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(r *Runner) { r.sleep = sleep }
}

// NewRunner wires a Runner to its engines and configuration.
//
// It refuses the one incoherent combination at construction, not at the first failure:
// reflection enabled with no reflector is a misconfiguration that would otherwise surface
// only when a task first failed and the loop reached for a reflector that was not there.
func NewRunner(engines Engines, cfg Config, log *slog.Logger, opts ...Option) (*Runner, error) {
	if engines.Planner == nil || engines.Executor == nil || engines.Evaluator == nil || engines.Summariser == nil {
		return nil, fmt.Errorf("%w: a runner needs a planner, an executor, an evaluator and a summariser", ErrConfig)
	}
	if cfg.Reflection && engines.Reflector == nil {
		return nil, fmt.Errorf("%w: reflection is enabled but no reflector was provided", ErrConfig)
	}
	r := &Runner{
		engines: engines,
		cfg:     cfg,
		log:     log,
		now:     time.Now,
		sleep:   sleepCtx,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Run drives a goal from planning to a terminal state and returns the final [State]. The
// returned error is non-nil only when the loop ended somewhere other than the goal being
// achieved — [ErrStopped] wrapping the [StopReason] — so a caller can branch on the outcome
// without inspecting the state, exactly as agent.Service.Wait returns an error for a terminal
// non-success.
//
// The State is ALWAYS returned, error or not, because a loop that stopped short still did
// work worth reporting: it has a plan, a partial history, and — because Decide summarises
// before it stops — usually a summary.
func (r *Runner) Run(ctx context.Context, goal Goal) (State, error) {
	if err := goal.Validate(); err != nil {
		return State{}, err
	}

	log := r.log.With(
		"correlationId", goal.CorrelationID,
		"workflowExecutionId", goal.WorkflowExecutionID,
		"agentId", goal.AgentID,
		"goal", goal.Objective,
	)
	log.Info("loop started", "config", r.cfg.Redacted())

	s := Initial(goal, r.now())

	for {
		action := Decide(s, r.cfg, r.now())

		if action.Kind == ActionStop {
			return r.finish(log, s, action)
		}

		// The backoff for a retry is a delay the reducer hands us; WE do the waiting. In this
		// synchronous driver that is a sleep; under n8n it is a durable wait node. Either way
		// the reducer decided the delay and stayed pure.
		if action.Delay > 0 {
			log.Info("waiting before the next attempt", "delay", action.Delay.String(), "reason", action.Reason)
			if err := r.sleep(ctx, action.Delay); err != nil {
				// The caller cancelled, or the context expired, DURING a backoff. That is not a
				// loop failure — it is the outside world reclaiming its process — so surface it
				// as-is and let the caller resume from the persisted state if it wants to.
				return s, err
			}
		}

		result := r.perform(ctx, log, s, action)
		s = Advance(s, r.cfg, result, r.now())
	}
}

// perform executes one action by calling the right engine, times it, logs it, and packages
// the result for [Advance]. It is the boundary between the pure reducer and the impure world,
// and it is the only method in the package that calls an engine.
func (r *Runner) perform(ctx context.Context, log *slog.Logger, s State, a Action) StepResult {
	start := r.now()

	stageLog := log.With(
		"iteration", s.Iterations,
		"phase", string(s.Phase),
		"action", string(a.Kind),
		"reason", a.Reason,
	)

	switch a.Kind {
	case ActionPlan:
		plan, err := r.engines.Planner.Plan(ctx, s.Goal)
		if err != nil {
			return r.engineFailed(stageLog, "planning", start, err)
		}
		stageLog.Info("planned", "tasks", len(plan.Tasks), "elapsedMs", r.since(start))
		return StepResult{Plan: &plan, Cost: pricedCost(r.engines.Planner)}

	case ActionExecute:
		stageLog = stageLog.With("taskId", a.Task.ID, "taskType", a.Task.Type)
		stageLog.Info("executing task", "retryCount", s.RetryCount, "attempt", s.Iterations)
		// s.Iterations is the count BEFORE this execution — a distinct value for every
		// dispatch, including retries — so it is exactly the attempt number the executor needs
		// to make each retry a fresh execution rather than an idempotent replay of the last.
		outcome, err := r.engines.Executor.Execute(ctx, s.Goal, a.Task, s.Iterations)
		if err != nil {
			// An executor ERROR (as opposed to a failed outcome) means it could not run the
			// task at all — the runtime was unreachable in a way it could not turn into an
			// outcome. Treat it as a transient failed outcome rather than killing the loop, so
			// the retry machinery gets a chance; a truly dead runtime will exhaust the retries
			// and stop the loop cleanly.
			stageLog.Error("executor could not run the task",
				"error", err, "elapsedMs", r.since(start))
			return StepResult{Outcome: &Outcome{
				TaskID:    a.Task.ID,
				Success:   false,
				Transient: true,
				Error:     err.Error(),
			}}
		}
		stageLog.Info("task executed",
			"success", outcome.Success,
			"transient", outcome.Transient,
			"executionId", outcome.ExecutionID,
			"costUsd", outcome.CostUSD,
			"elapsedMs", r.since(start),
		)
		return StepResult{Outcome: &outcome}

	case ActionEvaluate:
		eval, err := r.engines.Evaluator.Evaluate(ctx, s.Goal, a.Task, a.Outcome)
		if err != nil {
			return r.engineFailed(stageLog, "evaluation", start, err)
		}
		stageLog.Info("evaluated",
			"taskSucceeded", eval.TaskSucceeded,
			"goalAchieved", eval.GoalAchieved,
			"retry", eval.Retry,
			"replan", eval.Replan,
			"humanRequired", eval.HumanRequired,
			"confidence", eval.Confidence,
			"decision", eval.Reason,
			"elapsedMs", r.since(start),
		)
		return StepResult{Evaluation: &eval, Cost: pricedCost(r.engines.Evaluator)}

	case ActionReflect:
		refl, err := r.engines.Reflector.Reflect(ctx, s.Goal, a.Task, a.Outcome, a.Evaluation)
		if err != nil {
			return r.engineFailed(stageLog, "reflection", start, err)
		}
		stageLog.Info("reflected",
			"adjustment", refl.Adjustment,
			"revisedInstructions", refl.RevisedInstructions != "",
			"elapsedMs", r.since(start),
		)
		return StepResult{Reflection: &refl, Cost: pricedCost(r.engines.Reflector)}

	case ActionSummarise:
		summary, err := r.engines.Summariser.Summarise(ctx, s.Goal, s)
		if err != nil {
			// A summariser failing is not worth failing the whole loop over — the work is
			// already done, and a missing narrative should not turn an achieved goal into a
			// failure. Synthesise a plain summary from the state so the loop still ends with an
			// account, and note that the prose was unavailable.
			stageLog.Warn("summariser failed; using a plain summary from the state",
				"error", err, "elapsedMs", r.since(start))
			fallback := fallbackSummary(s)
			return StepResult{Summary: &fallback}
		}
		stageLog.Info("summarised", "outcome", summary.Outcome, "elapsedMs", r.since(start))
		return StepResult{Summary: &summary, Cost: pricedCost(r.engines.Summariser)}

	default:
		return StepResult{EngineErr: fmt.Errorf("%w: unknown action %q", ErrEngine, a.Kind)}
	}
}

func (r *Runner) engineFailed(log *slog.Logger, stage string, start time.Time, err error) StepResult {
	log.Error("a loop stage failed", "stage", stage, "error", err, "elapsedMs", r.since(start))
	return StepResult{EngineErr: fmt.Errorf("%w: %s: %v", ErrEngine, stage, err)}
}

// finish logs the terminal outcome and turns it into the return value.
func (r *Runner) finish(log *slog.Logger, s State, a Action) (State, error) {
	// Perform the terminal stop's bookkeeping: the reducer set the phase, we record why.
	s.Stop = a.Stop
	if !s.Phase.Terminal() {
		// Decide asked to stop but the phase is not terminal (a mid-run stop before summarising
		// could not complete). Mark it stopped so the returned state is honest.
		s.Phase = PhaseStopped
	}

	fields := []any{
		"phase", string(s.Phase),
		"stop", string(s.Stop),
		"iterations", s.Iterations,
		"replans", s.Replans,
		"completed", len(s.Completed),
		"failed", len(s.Failed),
		"reflections", len(s.ReflectionHistory),
		"costUsd", s.CostUSD,
		"durationMs", r.now().Sub(s.StartedAt).Milliseconds(),
	}

	switch s.Phase {
	case PhaseDone:
		log.Info("loop completed — goal achieved", fields...)
		return s, nil
	case PhaseStopped:
		// A stop is not a crash. It is a bound doing its job, and the log says so at WARN, not
		// ERROR — an alert on "the loop failed" must not fire every time a cost cap works.
		log.Warn("loop stopped before achieving the goal", fields...)
		return s, fmt.Errorf("%w: %s", ErrStopped, s.Stop)
	default:
		log.Error("loop failed", fields...)
		return s, fmt.Errorf("%w: %s", ErrStopped, s.Stop)
	}
}

func (r *Runner) since(start time.Time) int64 { return r.now().Sub(start).Milliseconds() }

// fallbackSummary builds a plain-language summary from the state, for when the summariser
// itself is unavailable. It states facts, invents nothing.
func fallbackSummary(s State) Summary {
	outcome := "stopped: " + string(s.Stop)
	if s.Stop == StopGoalAchieved {
		outcome = "achieved"
	}
	result := ""
	if len(s.Completed) > 0 {
		result = s.Completed[len(s.Completed)-1].Output
	}
	return Summary{
		Outcome: outcome,
		Narrative: fmt.Sprintf("The loop ran %d iteration(s), completed %d task(s) and failed %d, "+
			"across %d reflection(s), and stopped with reason %q. (This summary was generated from "+
			"the loop state because the summariser was unavailable.)",
			s.Iterations, len(s.Completed), len(s.Failed), len(s.ReflectionHistory), s.Stop),
		Result: result,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
