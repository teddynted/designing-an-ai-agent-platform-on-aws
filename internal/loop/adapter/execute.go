package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/agent"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/loop"
)

// Executor implements [loop.Executor] against the agent runtime (OpenClaw, via
// [agent.Service]). It is the one place the loop's abstract "execute a task" becomes a
// concrete, expensive, side-effecting agent run.
//
// # It submits-and-waits, deliberately
//
// This executor uses agent.Service.Execute, which submits and then BLOCKS until the agent
// finishes — the exact thing agent.Service.Wait warns against for a Lambda or an HTTP
// handler. That is correct here, and only here, because a [loop.Runner] is the synchronous
// driver: the CLI, or a caller that has genuinely thought about it. The production driver is
// n8n, and the n8n version of this executor does not block — it submits, and an n8n wait node
// does the polling durably. That executor is an n8n workflow, not code in this repository, in
// the same way the waiting for a single agent execution is (see AGENTS.md). The loop's
// reducer is identical under both.
type Executor struct {
	svc    *agent.Service
	limits agent.Limits
	wait   agent.WaitPolicy
}

// ExecutorConfig bounds each agent execution the loop starts. These are the AGENT's limits —
// its step budget, its wall clock, its output size — not the loop's; the loop's own bounds
// (iterations, cost, timeout) live in loop.Config. Both matter: the loop bounds the whole
// run, and these bound each task within it, so a single runaway agent cannot exhaust the
// loop's entire budget by itself.
type ExecutorConfig struct {
	MaxSteps       int
	MaxDuration    time.Duration
	MaxOutputBytes int64
	Wait           agent.WaitPolicy
}

// DefaultExecutorConfig is a conservative per-task budget.
func DefaultExecutorConfig() ExecutorConfig {
	return ExecutorConfig{
		MaxSteps:       40,
		MaxDuration:    20 * time.Minute,
		MaxOutputBytes: 1 << 20, // 1 MiB
		Wait:           agent.DefaultWaitPolicy(),
	}
}

// NewExecutor builds an executor over an agent service.
func NewExecutor(svc *agent.Service, cfg ExecutorConfig) *Executor {
	if cfg.MaxSteps <= 0 {
		cfg = DefaultExecutorConfig()
	}
	return &Executor{
		svc: svc,
		limits: agent.Limits{
			MaxSteps:           cfg.MaxSteps,
			MaxDuration:        cfg.MaxDuration,
			MaxDurationSeconds: int(cfg.MaxDuration.Seconds()),
			MaxOutputBytes:     cfg.MaxOutputBytes,
		},
		wait: cfg.Wait,
	}
}

// TaskTypes reports the task types this executor's runtime can perform, for the planner to
// choose from. It is how the plan is constrained to work the runtime can actually do — a
// planner told these cannot invent a task type no agent exists for.
func (e *Executor) TaskTypes() []string {
	types := e.svc.Tasks()
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	return out
}

// Execute runs one task as an agent execution and maps the result to a [loop.Outcome].
//
// It never returns an error for a task that RAN and failed — that is an Outcome with
// Success=false, which the loop reasons about. It returns an error only for the executor
// being unable to run at all, and even then the Runner turns that into a transient outcome so
// the retry machinery gets a chance. The one thing this method must get right is
// [loop.Outcome.Transient], because the loop's whole retry decision rests on it.
func (e *Executor) Execute(ctx context.Context, goal loop.Goal, task loop.Task, attempt int) (loop.Outcome, error) {
	taskType, ok := e.resolve(task.Type)
	if !ok {
		// The plan named a task type no agent can perform. This is deterministic — retrying
		// cannot conjure an agent — so it is a non-transient failed outcome, which the loop
		// will not retry. It is not an executor error, because the executor is fine; the plan
		// was wrong, and the loop's replan is the right recovery.
		return loop.Outcome{
			TaskID:    task.ID,
			Success:   false,
			Transient: false,
			Error:     fmt.Sprintf("no agent can perform task type %q (available: %s)", task.Type, strings.Join(e.TaskTypes(), ", ")),
		}, nil
	}

	req := agent.Request{
		Task: agent.Task{
			Type:         taskType,
			Instructions: task.Instructions,
			Repository: agent.Repository{
				Name:      goal.Repository.Name,
				URL:       goal.Repository.URL,
				Branch:    goal.Repository.Branch,
				CommitSHA: goal.Repository.CommitSHA,
			},
			Limits: e.limits,
		},
		// The correlation folds in the task AND the attempt, so a genuine loop retry is a fresh
		// agent execution rather than an idempotent replay of the failed one — while a transport
		// retry WITHIN a single submit still shares one key and stays idempotent. See the note
		// on attempt in loop.Executor.
		CorrelationID:       fmt.Sprintf("%s:%s:%d", goal.CorrelationID, task.ID, attempt),
		WorkflowExecutionID: goal.WorkflowExecutionID,
	}

	result, err := e.svc.Execute(ctx, req, e.wait)

	outcome := loop.Outcome{
		TaskID:          task.ID,
		ExecutionID:     result.ID,
		CostUSD:         result.Cost,
		DurationSeconds: result.Duration().Seconds(),
	}

	if err != nil {
		outcome.Success = false
		outcome.Error = err.Error()
		outcome.Transient = transient(err)
		return outcome, nil
	}

	outcome.Success = true
	outcome.Output = result.Output.Content
	return outcome, nil
}

// resolve maps a plan's task-type string to a runtime task type, checking it is one the
// runtime actually has.
func (e *Executor) resolve(t string) (agent.TaskType, bool) {
	for _, known := range e.svc.Tasks() {
		if string(known) == t {
			return known, true
		}
	}
	return "", false
}

// transient decides whether an agent failure is worth retrying, mapping the agent runtime's
// errors the way the router maps a provider's. The rule is the same one the whole platform
// uses: a failure that is the RUNTIME's (unreachable, timed out) might go differently next
// time; a failure that is the WORK's (the agent ran and failed, produced something we
// rejected, was misconfigured) will not.
func transient(err error) bool {
	switch {
	case errors.Is(err, agent.ErrUnavailable),
		errors.Is(err, agent.ErrTimeout),
		errors.Is(err, agent.ErrRetriesExhausted):
		// The runtime could not be reached or did not answer in time. A later attempt might.
		return true
	default:
		// ErrAgentFailed, ErrExecutionTimeout, ErrOutputRejected, ErrUnauthorized,
		// ErrUnknownTask, ErrInvalidRequest — the agent ran and failed, or the request is
		// wrong. Retrying re-runs a doomed task and spends again. The loop's REFLECTION, not a
		// blind retry, is the recovery for a task that failed on its merits — and reflection
		// only helps if the loop does not burn the attempt on an identical retry first.
		return false
	}
}
