// Package loop is the platform's loop CONTROLLER: an explicit, external, serialisable
// state machine that drives a goal to completion through plan → execute → evaluate →
// reflect → decide, until it is done or a stopping condition stops it.
//
// # Three loops, and this is the third
//
// By Milestone 11 the platform already has two loops, and the entire point of this one is
// that it is NOT either of them. Blurring the three is the mistake this package exists to
// avoid, so it is worth being exact about which is which:
//
//   - **The agent's loop (Milestone 6) runs INSIDE OpenClaw.** The platform submits one
//     open-ended task, and the agent reasons, uses its shell, and produces output entirely
//     behind an HTTP boundary. The platform never sees the turns; agent.Limits.MaxSteps
//     bounds them. That loop is the agent's business and this package does not reach into it.
//
//   - **The tool loop (Milestone 9) runs INSIDE one inference.** llm.Service.Converse lets a
//     single model call tools and read results within one conversation — think, act, think —
//     bounded by turns and cost. The MODEL drives it. It is one reasoning task that happens
//     to take several round trips.
//
//   - **This loop (Milestone 11) runs ABOVE both.** A goal is decomposed into tasks; each
//     task is executed, its result evaluated, reflected on, and the plan adapted; and the
//     whole thing repeats until the goal is met or a bound is hit. The PLATFORM drives it,
//     explicitly, and can inspect it, persist it, retry it, and stop it.
//
// The word that matters is *engineering*. Milestone 9 let a loop happen implicitly, inside a
// model, and trusted the model to converge. This milestone makes the loop explicit, external
// and inspectable — because a loop you can see is a loop you can bound, resume, and be billed
// predictably for, and a loop inside a model is none of those things.
//
// # Where each stage's work actually happens
//
// This package orchestrates; it does not do the work itself. It holds no HTTP client, calls
// no model, and runs no shell. Each stage is delegated across a boundary the platform
// already had, and the mapping is the whole reason this respects the architecture rather
// than reinventing the agent:
//
//	PLAN · EVALUATE · REFLECT · SUMMARISE   → the platform's own inference (Milestone 7-10)
//	                                          Single-shot, structured, no side effects, safe
//	                                          to retry. This is what the inference plane is
//	                                          FOR ("not everything worth doing with a model
//	                                          needs an agent").
//
//	EXECUTE a task                          → OpenClaw, via the agent runtime (Milestone 6)
//	                                          The one step with side effects, real cost, and
//	                                          "NOT safe to retry". OpenClaw executes tasks;
//	                                          this loop decides WHICH, and what to do next.
//
//	WAITING across long executions          → n8n (Milestone 5)
//	                                          An agent run takes minutes to hours, and this
//	                                          package must never hold a process open for one.
//	                                          See below.
//
// So this loop is orchestration, not execution — the distinction Milestone 5 drew and
// Milestone 6 sharpened. It sits where an orchestrator sits, and every boundary beneath it is
// untouched.
//
// # Why the controller is a pure reducer, and does no I/O
//
// The controller is two pure functions — [Decide] turns a [State] into the next [Action],
// and [Advance] folds an action's result back into the state — and neither one calls
// anything. A [Runner] performs the actions and feeds the results back. That separation is
// not tidiness; it is what buys every hard requirement of this milestone at once:
//
//   - **Stopping conditions are ALWAYS enforced.** They are checked at the top of [Decide],
//     which runs before every single action, so there is no code path that can start work
//     with a budget already blown. It is the same discipline as llm.Service checking the
//     context window in one place so no provider can forget — a guard that lives on the one
//     road everything travels.
//
//   - **State survives interruption.** The [State] is a plain value with JSON tags. A Spot
//     instance reclaimed mid-loop (which, on this platform, happens with two minutes'
//     notice — see infra/SPOT.md) loses only the in-flight action, and the idempotency the
//     agent runtime already enforces makes re-running that action safe. Persist the state,
//     reload it, call [Decide] again: the loop resumes exactly where it stopped.
//
//   - **n8n can drive it durably.** Because the controller never waits, the production driver
//     is n8n: call [Decide], perform the action (submitting an agent execution and letting an
//     n8n wait node poll it), then call [Advance] with the outcome — durably, across a run
//     that may last hours, on infrastructure built to survive restarts. The [Runner] in this
//     package is the *synchronous* driver, for the CLI and for tests: the one that is allowed
//     to block because it has, in agent.Service.Wait's words, "genuinely thought about it".
//
//   - **Every stage is a table test.** "Why did the loop replan here?" is answered by calling
//     a pure function with a struct literal — no model, no network, no agent.
//
// # It knows nothing about a provider or a runtime
//
// This package imports the standard library and nothing else of ours. It does not import
// internal/llm, internal/agent, internal/openclaw, or any vendor — internal/architecture_test.go
// fails the build if that changes. The stages are declared as interfaces here ([Planner],
// [Executor], [Evaluator], [Reflector], [Summariser]) and implemented at the edge, in
// internal/loop/adapter, which is the one place that knows a Plan is produced by Claude and a
// task is executed by OpenClaw. So "the loop is independent of the provider and the runtime"
// is a fact the compiler checks, not a claim in a comment — and adding a provider, or
// swapping OpenClaw for another runtime, changes an adapter and never touches the loop.
package loop

import (
	"errors"
	"fmt"
	"strings"
)

// Errors the loop can produce. They are the platform's vocabulary for "the loop itself went
// wrong", as distinct from a task failing (which is a normal [Outcome], not an error) — the
// same distinction agent.ErrAgentFailed draws between "the agent ran and failed" and "the
// plumbing broke".
var (
	// ErrNoPlan means planning produced nothing usable — an empty task list, or a plan whose
	// dependencies do not resolve. It is terminal: a loop with no plan has nothing to execute
	// and re-running the same planner on the same goal will produce the same nothing.
	ErrNoPlan = errors.New("planning produced no usable plan")

	// ErrInvalidGoal means the goal cannot be worked on as stated — no objective, or no
	// repository to point an executor at. It fails before any model or agent is called.
	ErrInvalidGoal = errors.New("invalid goal")

	// ErrStopped means the loop hit a stopping condition before achieving the goal. It wraps
	// the specific [StopReason] so a caller can tell "we ran out of iterations" from "a human
	// is needed" from "the cost cap tripped" — because the next action a human takes is
	// different for each, and an error that collapsed them would be one nobody could act on.
	ErrStopped = errors.New("the loop stopped before achieving the goal")

	// ErrEngine means a stage engine (planner, executor, evaluator, reflector, summariser)
	// failed in a way that is not a task outcome — the model was unreachable, or returned
	// something that did not fit its schema. The loop cannot continue without that stage.
	ErrEngine = errors.New("a loop stage failed")
)

// Repository is what a goal is about. It is a small, deliberate duplication of
// agent.Repository: this package must not import internal/agent, so it owns its own copy of
// the handful of fields an executor needs, and the adapter translates. Every seam in this
// platform owns its own vocabulary for exactly this reason.
type Repository struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Branch    string `json:"branch,omitempty"`
	CommitSHA string `json:"commitSha,omitempty"`
}

// Goal is what the loop is working toward. It is set once, at the start, and never changes —
// the PLAN changes as the loop learns, but the goal is the fixed point the whole thing is
// measured against.
type Goal struct {
	// Objective is the thing to achieve, in plain language: "Draft a blog post explaining the
	// changes in this release." It comes from the platform — a workflow, an operator — never
	// from the repository being worked on, exactly as agent.Task.Instructions does, and for
	// the same security reason: repository content is attacker-influenced and must be data,
	// never instruction.
	Objective string `json:"objective"`

	// Repository is what the objective is about, for the executor to point an agent at.
	Repository Repository `json:"repository"`

	// CorrelationID and WorkflowExecutionID continue the chain that began at the GitHub
	// delivery: webhook → n8n → loop → (plan/execute/evaluate). Without them a loop's cost and
	// duration are a mystery rather than a step in something.
	CorrelationID       string `json:"correlationId"`
	WorkflowExecutionID string `json:"workflowExecutionId,omitempty"`

	// AgentID names the logical agent this loop is acting as, for the logs. It is not an
	// OpenClaw agent name (that is the runtime's business); it is who, at the platform level,
	// this loop is on behalf of.
	AgentID string `json:"agentId,omitempty"`

	// Params are objective-specific knobs passed through to the stages: a tone, a target
	// file. Sent as-is, so no secrets.
	Params map[string]string `json:"params,omitempty"`
}

// Validate reports whether the goal can be worked on at all.
func (g Goal) Validate() error {
	if strings.TrimSpace(g.Objective) == "" {
		return fmt.Errorf("%w: no objective", ErrInvalidGoal)
	}
	if strings.TrimSpace(g.Repository.URL) == "" {
		// An executor has to point an agent at something. A goal with no repository is a goal
		// no task can be run against.
		return fmt.Errorf("%w: no repository URL", ErrInvalidGoal)
	}
	if strings.TrimSpace(g.CorrelationID) == "" {
		// The whole chain hangs off this, and the executor's idempotency key is derived from
		// it — without it, a re-run of a task is indistinguishable from a new one.
		return fmt.Errorf("%w: no correlation ID", ErrInvalidGoal)
	}
	return nil
}

// Task is one unit of work in a plan.
//
// A task is deliberately coarse: it is a whole piece of work an executor can do end to end
// ("analyse the repository", "draft the post"), not a single tool call or a single model
// turn. Those finer grains belong to the loops beneath this one — the tool loop inside an
// inference, the reasoning loop inside the agent. A task here is the unit this loop plans,
// executes, evaluates, and may retry as a whole.
type Task struct {
	// ID is unique within a plan. It is how outcomes, retries, and dependencies refer to a
	// task without depending on its position, which reflection may change.
	ID string `json:"id"`

	// Type is the KIND of work, which the executor maps to a concrete capability (an OpenClaw
	// agent task type). It is a string, not an agent.TaskType, because this package does not
	// know that OpenClaw exists — the adapter does the mapping, and refuses a type no runtime
	// can perform.
	Type string `json:"type"`

	// Description is what this task is for, in plain language — for the log, for a human, and
	// for the evaluator deciding whether it succeeded.
	Description string `json:"description"`

	// Instructions are what the executor should actually do. Like [Goal.Objective], they come
	// from the platform's own planning, never from repository content. Reflection may rewrite
	// them between attempts — which is the whole point of reflection — but it rewrites them
	// from the platform's side of the boundary.
	Instructions string `json:"instructions"`

	// DependsOn lists task IDs that must complete first. It is what lets a plan be a small
	// graph rather than a flat list, and it is what a parallel driver would read to run
	// independent tasks at once. This milestone's Runner executes sequentially in dependency
	// order; the field exists so that parallelism is a driver change, not a plan change.
	DependsOn []string `json:"dependsOn,omitempty"`
}

// Plan is an ordered set of tasks and the reasoning behind it.
type Plan struct {
	// Rationale is why the plan looks the way it does. It is logged, not executed — but it is
	// the first thing a human reads when a plan looks wrong, and a planner made to justify
	// itself tends to produce a better plan.
	Rationale string `json:"rationale"`

	// Tasks are the work, in an order that respects DependsOn.
	Tasks []Task `json:"tasks"`
}

// Validate reports whether the plan can be executed.
//
// It is the loop's own check, separate from any schema the adapter enforced on the model's
// output — because the schema can say "tasks is a non-empty array of objects with an id",
// and it cannot say "every DependsOn refers to a task that exists" or "no task depends on
// itself". Those are the checks that catch a plausibly-shaped, unexecutable plan, which is
// exactly the kind a language model produces some of the time.
func (p Plan) Validate() error {
	if len(p.Tasks) == 0 {
		return fmt.Errorf("%w: the plan has no tasks", ErrNoPlan)
	}

	ids := make(map[string]bool, len(p.Tasks))
	for _, t := range p.Tasks {
		if strings.TrimSpace(t.ID) == "" {
			return fmt.Errorf("%w: a task has no id", ErrNoPlan)
		}
		if ids[t.ID] {
			return fmt.Errorf("%w: duplicate task id %q", ErrNoPlan, t.ID)
		}
		if strings.TrimSpace(t.Type) == "" {
			return fmt.Errorf("%w: task %q has no type", ErrNoPlan, t.ID)
		}
		if strings.TrimSpace(t.Instructions) == "" {
			return fmt.Errorf("%w: task %q has no instructions", ErrNoPlan, t.ID)
		}
		ids[t.ID] = true
	}

	// Dependencies must resolve, and a task may not depend on itself. An unresolved
	// dependency is not a slow plan; it is one the loop would deadlock on, waiting for a task
	// that will never run.
	for _, t := range p.Tasks {
		for _, dep := range t.DependsOn {
			if dep == t.ID {
				return fmt.Errorf("%w: task %q depends on itself", ErrNoPlan, t.ID)
			}
			if !ids[dep] {
				return fmt.Errorf("%w: task %q depends on %q, which is not in the plan", ErrNoPlan, t.ID, dep)
			}
		}
	}
	return nil
}

// Outcome is the result of executing one task. It is a VALUE, not an error: a task that
// failed is a normal thing for the loop to reason about — to retry, to reflect on, to replan
// around — and modelling it as an error would force the reducer to unpack errors to make
// routing decisions, which is exactly the coupling the [Evaluation] exists to avoid.
type Outcome struct {
	TaskID string `json:"taskId"`

	// Success is whether the executor believes the task did what it was asked. It is the
	// executor's first-pass verdict; the [Evaluator] gets a second, more considered say,
	// because "the agent exited 0" and "the agent did the right thing" are different claims.
	Success bool `json:"success"`

	// Output is what the task produced — a draft, a summary, an analysis — for the evaluator
	// to judge and the next task to build on.
	Output string `json:"output,omitempty"`

	// Error is why it failed, when it did. A string, not an error, because an Outcome is
	// serialised into the [State] and carried across process boundaries, and an error does
	// not survive that.
	Error string `json:"error,omitempty"`

	// Transient reports whether the failure looks retryable — the runtime was unreachable,
	// the request timed out — as opposed to deterministic — the agent ran and produced
	// something we rejected. Only transient failures are retried, and getting this line right
	// is the difference between recovering from a blip and re-running a doomed task until the
	// retry budget is gone. It is the executor's job to set it, mapping the runtime's errors
	// the same way the router maps a provider's (see internal/router canFailOver).
	Transient bool `json:"transient,omitempty"`

	// CostUSD and DurationSeconds are what this attempt spent. They accumulate into the
	// [State], because the cost cap is enforced against the running total, and "it failed
	// after $1.80" is a different problem from "it failed immediately".
	CostUSD         float64 `json:"costUsd,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`

	// ExecutionID is the runtime's own handle on the run, for the log — the thing you paste
	// into OpenClaw to see what the agent actually did.
	ExecutionID string `json:"executionId,omitempty"`
}

// Evaluation is the considered verdict on an outcome, and the loop's routing decision. It is
// produced by the [Evaluator] — a reasoning step — and it is what turns a raw result into a
// decision about what the loop does next. It is deliberately a set of BOOLEANS plus a reason,
// not free prose, because the reducer branches on it and a reducer cannot branch on a
// paragraph.
type Evaluation struct {
	// TaskSucceeded is the considered verdict on THIS task — which can differ from the
	// outcome's own Success flag. An agent can exit cleanly having produced a draft that does
	// not actually address the objective; the evaluator is where that is caught.
	TaskSucceeded bool `json:"taskSucceeded"`

	// GoalAchieved is whether the OBJECTIVE is now met — the only condition that lets the loop
	// finish successfully. It is separate from TaskSucceeded because a task can succeed
	// without completing the goal (there is more to do) and, rarely, the goal can be met
	// before every planned task has run.
	GoalAchieved bool `json:"goalAchieved"`

	// Retry asks for another attempt at this task. The reducer honours it only if the failure
	// was transient and the retry budget allows — a request to retry is advice, not a
	// command, because an evaluator that could force unbounded retries would be an evaluator
	// that could spend without limit.
	Retry bool `json:"retry"`

	// Replan asks for the plan to be rebuilt from here — the current approach is not working
	// and a different decomposition is needed. It is the loop's main form of adaptation, and
	// it is bounded by the iteration cap so that "replan" cannot become an infinite outer
	// loop the way "retry" cannot become an infinite inner one.
	Replan bool `json:"replan"`

	// HumanRequired stops the loop and asks for a person. It is one of the always-enforced
	// stopping conditions, and it exists because the honest answer to some situations is "an
	// autonomous system should not decide this" — a destructive action, an ambiguous
	// objective, a repeated failure it cannot diagnose. Milestone 14 will turn this into an
	// n8n approval gate; here it is a clean, loud stop.
	HumanRequired bool `json:"humanRequired"`

	// Confidence is how sure the evaluator is, in [0, 1]. It feeds the configurable evaluation
	// threshold: a "success" the evaluator is not confident about can be treated as a failure
	// worth another look, rather than waved through — because a low-confidence pass is where a
	// wrong-but-plausible result slips into a pull request.
	Confidence float64 `json:"confidence"`

	// Reason is the one-sentence why, for the log and for the reflector to build on. Not
	// executed; but it is what makes a routing decision explicable after the fact.
	Reason string `json:"reason"`
}

// Validate is the evaluator's semantic self-check, run after the schema decode. The schema
// guarantees the fields are present and typed; this guarantees they are coherent.
func (e Evaluation) Validate() error {
	if e.Confidence < 0 || e.Confidence > 1 {
		return fmt.Errorf("confidence %.2f is outside [0, 1]", e.Confidence)
	}
	// GoalAchieved implies the task did not fail: a goal met by a failed task is a
	// contradiction the reducer would have to guess its way out of, so refuse it here where
	// the model can still be asked to reconsider.
	if e.GoalAchieved && !e.TaskSucceeded {
		return fmt.Errorf("goalAchieved is true but taskSucceeded is false — a goal cannot be met by a task that failed")
	}
	return nil
}

// Reflection is what the loop learned from a failure, and how it will change its next
// attempt. It is produced by the [Reflector] — a reasoning step — and it is the mechanism by
// which the loop's BEHAVIOUR improves without any of the loop's CODE changing, which is the
// requirement the milestone states most plainly.
type Reflection struct {
	// Analysis is why the task failed, in the reflector's words. Logged and carried in the
	// [State.ReflectionHistory], so a human reading the trail can see the loop's reasoning
	// and not just its actions.
	Analysis string `json:"analysis"`

	// RevisedInstructions, when non-empty, REPLACE the current task's instructions for the
	// next attempt. This is the concrete output of reflection: a sharper, corrected prompt,
	// authored on the platform's side of the boundary. An empty value means "retry as-is",
	// which is the right answer for a purely transient failure that reflection cannot improve.
	RevisedInstructions string `json:"revisedInstructions,omitempty"`

	// Adjustment is a short note on the strategy change, for the log — "narrow the scope",
	// "ask for the summary first". It is the human-readable companion to RevisedInstructions.
	Adjustment string `json:"adjustment,omitempty"`
}

// Summary is the loop's final account of itself, produced once at the end by the
// [Summariser]. It is what a workflow receives and what a human reads: what was attempted,
// what happened, and what it cost.
type Summary struct {
	// Outcome is the headline: "achieved", "stopped: max-iterations", "failed". It is derived
	// from the [State], not invented by the model — the model writes the prose, the loop
	// states the fact.
	Outcome string `json:"outcome"`

	// Narrative is the readable account of the run, for a person.
	Narrative string `json:"narrative"`

	// Result is the objective's actual deliverable, when there is one — the finished draft,
	// the analysis — pulled from the outcome that achieved the goal.
	Result string `json:"result,omitempty"`
}
