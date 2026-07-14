package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Service is what the platform calls. It is the same for every runtime: validate
// the request, carry the correlation, time it, log it, hand it to the runtime.
//
// Those things are here and not in the runtime on purpose. They must not vary: if
// each runtime logged in its own shape, no dashboard could span them; if each
// invented its own correlation, an agent execution could not be traced back to the
// GitHub delivery that caused it. The runtime's job is narrower and dirtier — speak
// HTTP, survive a flaky network — and it should be replaceable without taking the
// observability with it.
type Service struct {
	runtime Runtime
	log     *slog.Logger
	now     func() time.Time
}

// Option customises a Service.
type Option func(*Service)

// WithClock replaces the clock. Tests use it; nothing else should.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService wires a Service to a runtime.
func NewService(runtime Runtime, log *slog.Logger, opts ...Option) *Service {
	s := &Service{runtime: runtime, log: log, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Tasks lists what can be executed.
func (s *Service) Tasks() []TaskType { return s.runtime.Tasks() }

// Submit starts an execution and returns immediately.
//
// This is the call the platform should almost always use. The agent runs for
// minutes to hours; nothing that is holding an HTTP connection, a Lambda
// invocation, or a webhook response open should be waiting for it.
func (s *Service) Submit(ctx context.Context, req Request) (Execution, error) {
	log := s.logger(req)

	if err := req.Validate(); err != nil {
		// A request that cannot succeed never reaches the runtime. Sending it turns a
		// local, obvious failure into a remote, confusing, billable one.
		log.Error("agent request rejected", "error", err)
		return Execution{}, err
	}

	if !s.knows(req.Task.Type) {
		err := fmt.Errorf("%w: %q (registered: %s)", ErrUnknownTask, req.Task.Type, taskList(s.runtime.Tasks()))
		log.Error("no agent for this task", "error", err)
		return Execution{}, err
	}

	log.Info("agent execution requested",
		"repository", req.Task.Repository.Name,
		"commitSha", req.Task.Repository.CommitSHA,
		"maxSteps", req.Task.Limits.MaxSteps,
		"maxDuration", req.Task.Limits.MaxDuration.String(),
		"idempotencyKey", req.IdempotencyKey(),
	)

	start := s.now()
	exec, err := s.runtime.Submit(ctx, req)
	elapsed := s.now().Sub(start)

	// Stamp the chain on the way out. The runtime echoes it back on Status and
	// Result — that is in the contract — but on Submit we are the ones who know it,
	// and a runtime that forgot to echo must not be able to erase it here.
	exec.TaskType = req.Task.Type
	exec.CorrelationID = req.CorrelationID
	exec.WorkflowExecutionID = req.WorkflowExecutionID

	if err != nil {
		log.Error("agent execution failed to start",
			"error", err,
			"errorKind", Kind(err),
			"retriesExhausted", errors.Is(err, ErrRetriesExhausted),
			"attempts", exec.Attempts,
			"submitMs", elapsed.Milliseconds(),
		)
		return exec, err
	}

	// "started" is a distinct event from "requested": between them sits the
	// network, and an execution ID we did not have before. It is the first moment
	// the run exists somewhere other than in our intention.
	log.Info("agent execution started",
		"executionId", exec.ID,
		"agent", exec.Agent,
		"status", string(exec.Status),
		"attempts", exec.Attempts,
		"submitMs", elapsed.Milliseconds(),
	)
	return exec, nil
}

// Status reports where an execution is.
func (s *Service) Status(ctx context.Context, executionID string) (Execution, error) {
	exec, err := s.runtime.Status(ctx, executionID)
	if err != nil {
		s.log.Error("could not read execution status",
			"executionId", executionID, "error", err, "errorKind", Kind(err))
		return Execution{}, err
	}
	return exec, nil
}

// Result fetches what a finished execution produced.
func (s *Service) Result(ctx context.Context, executionID string) (Result, error) {
	res, err := s.runtime.Result(ctx, executionID)
	if err != nil {
		s.log.Error("could not read execution result",
			"executionId", executionID, "error", err, "errorKind", Kind(err))
		return Result{}, err
	}
	return res, nil
}

// Cancel stops an execution that has gone wrong — before it finishes spending.
func (s *Service) Cancel(ctx context.Context, executionID string) error {
	s.log.Warn("cancelling agent execution", "executionId", executionID)
	if err := s.runtime.Cancel(ctx, executionID); err != nil {
		s.log.Error("could not cancel execution",
			"executionId", executionID, "error", err, "errorKind", Kind(err))
		return err
	}
	s.log.Info("agent execution cancelled", "executionId", executionID)
	return nil
}

// WaitPolicy governs polling.
type WaitPolicy struct {
	// Interval is how often to ask. Too often and you are DDoSing your own runtime
	// for no information; agent runs move on the scale of seconds at best.
	Interval time.Duration
	// Timeout bounds the whole wait. It is NOT the agent's limit — the agent has
	// its own, in Limits. This one bounds *our patience*, and when it expires the
	// agent is still running.
	Timeout time.Duration
	// sleep is injectable so tests do not wait.
	sleep func(context.Context, time.Duration) error
}

// DefaultWaitPolicy polls every 5 seconds for up to 30 minutes.
func DefaultWaitPolicy() WaitPolicy {
	return WaitPolicy{Interval: 5 * time.Second, Timeout: 30 * time.Minute}
}

// Wait polls until the execution reaches a terminal status, then fetches its
// result.
//
// # Where NOT to call this
//
// **Not in a Lambda. Not in an HTTP handler. Not in the webhook path.**
//
// An agent run takes minutes to hours. Blocking a request-scoped process on it
// means paying for a process to sleep, and losing the run entirely when that
// process is killed — which, on this platform, is a Spot instance that can be
// reclaimed with two minutes' notice.
//
// **Waiting is n8n's job.** It has wait nodes, it is durable, and it already
// survives restarts. That is a large part of why the platform has an orchestrator
// at all. This method exists for the CLI, for tests, and for a caller that has
// genuinely thought about it.
func (s *Service) Wait(ctx context.Context, executionID string, policy WaitPolicy) (Result, error) {
	if policy.Interval <= 0 {
		policy.Interval = 5 * time.Second
	}
	if policy.Timeout <= 0 {
		policy.Timeout = 30 * time.Minute
	}
	sleep := policy.sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	ctx, cancel := context.WithTimeout(ctx, policy.Timeout)
	defer cancel()

	log := s.log.With("executionId", executionID)
	start := s.now()
	polls := 0

	for {
		exec, err := s.runtime.Status(ctx, executionID)
		if err != nil {
			// A status check failing does not mean the execution failed — we simply
			// cannot see it. Do not conclude anything about the agent from it.
			log.Error("could not read execution status while waiting",
				"error", err, "errorKind", Kind(err), "polls", polls)
			return Result{}, err
		}
		polls++

		if exec.Status.Terminal() {
			return s.finish(ctx, exec, s.now().Sub(start), polls)
		}

		log.Debug("agent still running",
			"status", string(exec.Status), "steps", exec.Steps, "polls", polls)

		if err := sleep(ctx, policy.Interval); err != nil {
			// Our patience ran out. Note precisely what this does and does not mean:
			// the agent is STILL RUNNING and still spending. The caller decides
			// whether to keep waiting or to cancel it.
			log.Warn("gave up waiting; the execution is still running",
				"waitedMs", s.now().Sub(start).Milliseconds(), "polls", polls)
			return Result{}, fmt.Errorf("%w: waited %s for execution %s, which is still running",
				ErrTimeout, policy.Timeout, executionID)
		}
	}
}

// finish turns a terminal execution into a result, and logs the outcome.
func (s *Service) finish(ctx context.Context, exec Execution, waited time.Duration, polls int) (Result, error) {
	// The whole chain, on the line that says how it ended. This is the line someone
	// reads when a pull request appears and nobody knows why, and it has to answer
	// that question on its own.
	log := s.log.With(
		"executionId", exec.ID,
		"correlationId", exec.CorrelationID,
		"workflowExecutionId", exec.WorkflowExecutionID,
		"taskType", string(exec.TaskType),
		"agent", exec.Agent,
	)

	if exec.Status != StatusSucceeded {
		// It ran and did not succeed. This is a *result*, not a transport failure,
		// and it is never retried by us: the agent had its chance, and it has already
		// spent the money.
		err := terminalError(exec)
		log.Error("agent execution failed",
			"error", err,
			"errorKind", Kind(err),
			"status", string(exec.Status),
			"steps", exec.Steps,
			"costUsd", exec.Cost,
			"durationMs", exec.Duration().Milliseconds(),
			"waitedMs", waited.Milliseconds(),
			"polls", polls,
		)
		return Result{Execution: exec}, err
	}

	result, err := s.runtime.Result(ctx, exec.ID)
	if err != nil {
		// Two very different things land here, and conflating them would be a
		// disservice to whoever reads the log at 3am.
		if errors.Is(err, ErrOutputRejected) {
			// The agent succeeded, and we REFUSED what it produced — a credential in
			// the draft, or something too large or malformed to publish. This is a
			// security event, not a transport failure, and it needs a human today.
			log.Error("agent output REJECTED — not published",
				"error", err,
				"errorKind", Kind(err),
				"steps", exec.Steps,
				"costUsd", exec.Cost,
			)
			return Result{Execution: exec}, err
		}
		// The agent succeeded and we could not read what it produced. The work is done
		// and paid for, and we are about to throw it away. The execution ID in this
		// line is how someone recovers it by hand.
		log.Error("agent succeeded but its result could not be read",
			"error", err, "errorKind", Kind(err))
		return Result{Execution: exec}, err
	}
	result.Execution = exec

	log.Info("agent execution completed",
		"status", string(exec.Status),
		"steps", exec.Steps,
		"costUsd", exec.Cost,
		"durationMs", exec.Duration().Milliseconds(),
		"waitedMs", waited.Milliseconds(),
		"polls", polls,
		"outputBytes", len(result.Output.Content),
		"artifacts", len(result.Output.Artifacts),
	)
	return result, nil
}

// Execute submits and waits. Convenience for a caller that can genuinely afford to
// block — the CLI, a test, an n8n node that is holding the run open on purpose.
//
// Read [Service.Wait] before using it.
func (s *Service) Execute(ctx context.Context, req Request, policy WaitPolicy) (Result, error) {
	exec, err := s.Submit(ctx, req)
	if err != nil {
		return Result{}, err
	}
	return s.Wait(ctx, exec.ID, policy)
}

// terminalError turns a non-successful terminal status into the right error, so a
// caller can distinguish "the agent was wrong" from "the agent ran out of road".
func terminalError(exec Execution) error {
	detail := exec.Error
	if detail == "" {
		detail = "no reason given"
	}
	switch exec.Status {
	case StatusTimedOut:
		return fmt.Errorf("%w: stopped after %d steps: %s", ErrExecutionTimeout, exec.Steps, detail)
	case StatusCancelled:
		return fmt.Errorf("%w: %s", ErrCancelled, detail)
	default:
		return fmt.Errorf("%w: %s", ErrAgentFailed, detail)
	}
}

func (s *Service) knows(t TaskType) bool {
	for _, known := range s.runtime.Tasks() {
		if known == t {
			return true
		}
	}
	return false
}

func (s *Service) logger(req Request) *slog.Logger {
	return s.log.With(
		"correlationId", req.CorrelationID,
		"workflowExecutionId", req.WorkflowExecutionID,
		"taskType", string(req.Task.Type),
		"runtime", s.runtime.Name(),
	)
}

// Kind classifies an error for logs and alerts: the sentinel's name, which is
// stable, rather than the message, which is not. An alert built on a message
// breaks the first time someone improves the wording.
//
// It reports the *cause*, so ErrRetriesExhausted is checked last: "we gave up" is
// far less useful on call than "we gave up on a timeout".
func Kind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnknownTask):
		return "unknown_task"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrInvalidResponse):
		return "invalid_response"
	case errors.Is(err, ErrOutputRejected):
		return "output_rejected"
	case errors.Is(err, ErrExecutionTimeout):
		return "execution_timeout"
	case errors.Is(err, ErrCancelled):
		return "cancelled"
	case errors.Is(err, ErrAgentFailed):
		return "agent_failed"
	case errors.Is(err, ErrStillRunning):
		return "still_running"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrRetriesExhausted):
		return "retries_exhausted"
	default:
		return "unknown"
	}
}

func taskList(tasks []TaskType) string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = string(t)
	}
	return strings.Join(out, ", ")
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
