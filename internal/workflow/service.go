package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Service is what the platform actually calls. It is the same for every engine:
// validate the request, give it a correlation ID, time it, log it, and hand it to
// the engine.
//
// Those five things are here rather than in the engine on purpose. They are the
// parts you must not get wrong and must not vary: if each engine logged in its
// own shape, a dashboard could not span them; if each engine invented its own
// correlation, a GitHub delivery could not be followed across the platform. The
// engine's job is narrower and dirtier — speak HTTP, survive a flaky network —
// and it should be free to be replaced without taking the observability with it.
type Service struct {
	engine Engine
	log    *slog.Logger
	// now is injectable so a test can measure a duration without sleeping.
	now func() time.Time
}

// Option customises a Service.
type Option func(*Service)

// WithClock replaces the clock. Tests use it; nothing else should.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService wires a Service to an engine.
func NewService(engine Engine, log *slog.Logger, opts ...Option) *Service {
	s := &Service{engine: engine, log: log, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Workflows lists what can be run, for a caller that wants to check before it
// asks — or for a health endpoint that wants to show what is wired up.
func (s *Service) Workflows() []string { return s.engine.Workflows() }

// Run triggers a workflow and reports what happened.
//
// Every execution produces at least two log lines — requested, then completed or
// failed — sharing a correlation ID. That is not decoration: when a blog post
// fails to appear three hours after a merge, the only question that matters is
// "did the platform ask for it, and what did the engine say?", and the answer has
// to be findable from the GitHub delivery ID alone.
func (s *Service) Run(ctx context.Context, req Request) (Result, error) {
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(req.Event)
	}

	log := s.log.With(
		"correlationId", req.CorrelationID,
		"workflow", req.Workflow,
		"engine", s.engine.Name(),
		"eventType", req.Event.Type,
		"repository", req.Event.Repository,
	)

	if err := req.Validate(); err != nil {
		// A bad request never reaches the engine. It cannot succeed, and sending it
		// would only turn a local, obvious failure into a remote, confusing one.
		log.Error("workflow request rejected", "error", err)
		return Result{}, err
	}

	if !s.knows(req.Workflow) {
		err := fmt.Errorf("%w: %q (configured: %s)",
			ErrUnknownWorkflow, req.Workflow, strings.Join(s.engine.Workflows(), ", "))
		// Worth being loud about: this is almost always a typo in a caller or a
		// workflow that was never added to the configuration, and it will otherwise
		// present as a mysterious 404 from the engine.
		log.Error("workflow is not registered", "error", err)
		return Result{}, err
	}

	log.Info("workflow requested",
		"commitSha", req.Event.CommitSHA,
		"branch", req.Event.Branch,
		"payloadBytes", len(req.Event.Payload),
	)

	start := s.now()
	result, err := s.engine.Trigger(ctx, req)
	elapsed := s.now().Sub(start)

	// The engine reports attempts and the execution ID; everything else about the
	// outcome is measured here, so it is measured the same way for every engine.
	result.Workflow = req.Workflow
	result.CorrelationID = req.CorrelationID
	result.Duration = elapsed
	result.DurationMS = elapsed.Milliseconds()

	if err != nil {
		// Log the failure with the class of error, not just its text, so an alert
		// can fire on "unauthorized" without pattern-matching an error message that
		// someone will reword next week.
		//
		// "Gave up retrying" is logged separately from *what* we gave up on, because
		// they are different questions. ErrRetriesExhausted always wraps its cause,
		// so errorKind still reports the real problem (a timeout, say) while
		// retriesExhausted says whether we absorbed it or surrendered to it.
		log.Error("workflow failed",
			"error", err,
			"errorKind", kind(err),
			"retriesExhausted", errors.Is(err, ErrRetriesExhausted),
			"attempts", result.Attempts,
			"durationMs", result.DurationMS,
		)
		if result.Status == "" {
			result.Status = StatusFailed
		}
		return result, err
	}

	log.Info("workflow completed",
		"status", string(result.Status),
		"executionId", result.ExecutionID,
		"attempts", result.Attempts,
		"durationMs", result.DurationMS,
	)
	return result, nil
}

func (s *Service) knows(name string) bool {
	for _, w := range s.engine.Workflows() {
		if w == name {
			return true
		}
	}
	return false
}

// correlationID derives a stable ID from the event.
//
// Stable is the whole point: the same GitHub delivery, retried by GitHub or
// replayed by an operator, must produce the same correlation ID — otherwise the
// duplicate looks like a different event, and the engine's idempotency key
// (which is built from this) cannot deduplicate it.
//
// So it is derived, never generated. A UUID here would be worse than useless: it
// would be actively misleading.
func correlationID(e Event) string {
	if e.ID == "" {
		return ""
	}
	if e.Type == "" {
		return e.ID
	}
	return e.Type + ":" + e.ID
}

// kind classifies an error for logs and alerts. It reports the sentinel's name,
// which is stable, rather than the message, which is not — an alert built on a
// message string breaks the first time someone improves the wording.
//
// It deliberately reports the *cause*, not the wrapper: ErrRetriesExhausted is
// checked last, because "we gave up" is far less useful to an on-call engineer
// than "we gave up on a timeout" or "we gave up on a 503".
func kind(err error) string {
	switch {
	case errors.Is(err, ErrUnknownWorkflow):
		return "unknown_workflow"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, ErrInvalidResponse):
		return "invalid_response"
	case errors.Is(err, ErrWorkflowFailed):
		return "workflow_failed"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrRetriesExhausted):
		return "retries_exhausted"
	default:
		return "unknown"
	}
}
