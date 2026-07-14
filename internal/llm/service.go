package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// CharsPerToken is the rough conversion used to check a prompt against a context
// window before sending it.
//
// It is an approximation and it is deliberately a PESSIMISTIC one. Real tokenisers
// average nearer 4 characters per token for English prose, but code — which is most
// of what this platform sends — tokenises much worse: punctuation, identifiers, and
// indentation all cost tokens. Guessing 3 means we refuse some prompts that would
// have fitted.
//
// That is the right way to be wrong. The alternative failure is silent truncation,
// and see [ErrContextExceeded] for why that is so much worse than a rejection.
const CharsPerToken = 3

// Service is what the platform calls. It is the same for every provider: validate
// the request, check it fits, carry the correlation, time it, log it.
//
// Those live here rather than in the provider because they must not vary. A router
// (Milestone 10) will sit in front of several providers, and if each logged in its
// own shape no dashboard could span them, and if each checked context windows
// differently the same prompt would be accepted by one and silently truncated by
// another.
type Service struct {
	provider Provider
	log      *slog.Logger
	now      func() time.Time
}

// Option customises a Service.
type Option func(*Service)

// WithClock replaces the clock. Tests use it; nothing else should.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService wires a Service to a provider.
func NewService(provider Provider, log *slog.Logger, opts ...Option) *Service {
	s := &Service{provider: provider, log: log, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Provider returns the underlying provider, for a caller that needs to ask it about
// itself (its models, its capabilities).
func (s *Service) Provider() Provider { return s.provider }

// Models lists what the provider has.
func (s *Service) Models(ctx context.Context) ([]Model, error) {
	models, err := s.provider.Models(ctx)
	if err != nil {
		s.log.Error("could not list models",
			"provider", s.provider.Name(), "error", err, "errorKind", Kind(err))
		return nil, err
	}
	return models, nil
}

// EnsureModel checks that a model exists on the provider, and says what IS there if
// it does not.
//
// This is meant to be called at start-up, not on the first webhook of the day. A
// model that is not pulled is a configuration error, and finding out about it while
// a user waits is strictly worse than finding out while nobody does.
func (s *Service) EnsureModel(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: no model name", ErrInvalidRequest)
	}

	models, err := s.provider.Models(ctx)
	if err != nil {
		return err
	}

	available := make([]string, 0, len(models))
	for _, m := range models {
		// Ollama's names carry a tag: "llama3.2" and "llama3.2:latest" are the same
		// model, and a config that omits the tag should not be an error.
		if m.Name == name || strings.TrimSuffix(m.Name, ":latest") == name {
			s.log.Info("model available",
				"provider", s.provider.Name(), "model", m.Name,
				"parameterSize", m.ParameterSize, "quantization", m.Quantization)
			return nil
		}
		available = append(available, m.Name)
	}

	// The error lists what IS there. Otherwise the next thing that happens is someone
	// SSHing into the box to run `ollama list`, and they should not have to.
	return fmt.Errorf("%w: %q is not on %s (available: %s). Pull it with: ollama pull %s",
		ErrModelNotFound, name, s.provider.Name(), strings.Join(available, ", "), name)
}

// Generate runs one inference and returns the whole completion.
func (s *Service) Generate(ctx context.Context, req Request) (Response, error) {
	log, err := s.begin(req)
	if err != nil {
		return Response{}, err
	}

	start := s.now()
	res, err := s.provider.Generate(ctx, req)
	res.Duration = s.now().Sub(start)

	return s.finish(log, req, res, err, false)
}

// Stream runs one inference, handing chunks to the sink as they arrive.
//
// It is the shape to prefer. A generation that takes ninety seconds looks exactly
// like a hang if you cannot see it happening — and, more importantly, streaming is
// what makes a *stall* detectable: a model that has gone quiet for thirty seconds is
// distinguishable from a model that is merely slow, and only one of those is broken.
func (s *Service) Stream(ctx context.Context, req Request, sink Sink) (Response, error) {
	log, err := s.begin(req)
	if err != nil {
		return Response{}, err
	}

	start := s.now()
	first := time.Duration(0)
	tokens := 0

	// Time to first token is the number a human actually experiences. It is also the
	// one that separates "the model is slow" from "the model is not loaded": a
	// ten-second first token and then a brisk stream means you paid a load cost.
	wrapped := func(c Chunk) error {
		if c.Content != "" {
			tokens++
			if first == 0 {
				first = s.now().Sub(start)
			}
		}
		return sink(c)
	}

	res, err := s.provider.Stream(ctx, req, wrapped)
	res.Duration = s.now().Sub(start)

	if first > 0 {
		log = log.With("firstTokenMs", first.Milliseconds())
	}
	return s.finish(log, req, res, err, true)
}

// begin validates the request and prepares the logger.
func (s *Service) begin(req Request) (*slog.Logger, error) {
	log := s.log.With(
		"provider", s.provider.Name(),
		"correlationId", req.CorrelationID,
		"workflowExecutionId", req.WorkflowExecutionID,
		"purpose", string(req.Purpose),
		"model", s.modelName(req),
	)

	if err := req.Validate(); err != nil {
		log.Error("inference request rejected", "error", err, "errorKind", Kind(err))
		return nil, err
	}

	// Check the prompt against the context window BEFORE sending it. A model asked to
	// read more than it can hold does not refuse — it silently drops the start of the
	// prompt and answers confidently from what is left. That failure produces a
	// plausible, wrong blog post, and nothing anywhere reports an error.
	input := req.Input()
	estimated := len(input) / CharsPerToken
	if window := s.provider.Capabilities().MaxContextTokens; window > 0 && estimated > window {
		err := fmt.Errorf("%w: ~%d tokens of prompt into a %d-token window. "+
			"The model would silently drop the beginning and answer from the rest — "+
			"summarise or chunk the input instead",
			ErrContextExceeded, estimated, window)
		log.Error("prompt does not fit", "error", err, "errorKind", Kind(err),
			"promptChars", len(input), "estimatedTokens", estimated, "contextWindow", window)
		return nil, err
	}

	// The prompt is NOT logged. It contains repository content: source code, commit
	// messages, and — on a bad day — something nobody meant to commit. A hash lets two
	// log lines be recognised as the same prompt without the prompt being in either.
	log.Info("inference requested",
		"promptChars", len(input),
		"estimatedTokens", estimated,
		"promptHash", hash(input),
		"maxTokens", req.Options.MaxTokens,
		"temperature", req.Options.Temperature,
	)
	return log, nil
}

// finish logs the outcome and returns it.
func (s *Service) finish(log *slog.Logger, req Request, res Response, err error, streamed bool) (Response, error) {
	if err != nil {
		log.Error("inference failed",
			"error", err,
			"errorKind", Kind(err),
			"retriesExhausted", errors.Is(err, ErrRetriesExhausted),
			"attempts", res.Attempts,
			"durationMs", res.Duration.Milliseconds(),
			"streamed", streamed,
		)
		return res, err
	}

	if strings.TrimSpace(res.Content) == "" {
		// A 200 with no tokens is not a success. It is a model that is loaded wrong, or
		// a prompt that was truncated to nothing — and passing an empty string back as
		// if it were an answer is how a platform publishes an empty blog post.
		err := fmt.Errorf("%w: the provider returned success and zero tokens", ErrEmptyCompletion)
		log.Error("inference produced nothing", "error", err, "errorKind", Kind(err),
			"durationMs", res.Duration.Milliseconds())
		return res, err
	}

	log = log.With(
		"durationMs", res.Duration.Milliseconds(),
		"attempts", res.Attempts,
		"promptTokens", res.Usage.PromptTokens,
		"completionTokens", res.Usage.CompletionTokens,
		"tokensPerSecond", round(res.Usage.TokensPerSecond),
		"loadMs", res.Usage.LoadDuration.Milliseconds(),
		"finishReason", res.FinishReason,
		"outputChars", len(res.Content),
		"streamed", streamed,
	)

	// "length" means the model was CUT OFF by the token budget. A truncated blog post
	// looks a great deal like a finished one until somebody reads the end of it, so
	// this is a warning and not an aside.
	if res.FinishReason == "length" {
		log.Warn("inference hit the token limit and was cut off — the output is INCOMPLETE",
			"maxTokens", req.Options.MaxTokens)
		return res, nil
	}

	log.Info("inference completed")
	return res, nil
}

func (s *Service) modelName(req Request) string {
	if req.Model != "" {
		return req.Model
	}
	return "(provider default)"
}

// hash identifies a prompt without revealing it. Short, because it is an identifier
// and not a checksum: it exists so two log lines can be recognised as the same
// prompt, not to prove integrity.
func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func round(f float64) float64 {
	return float64(int(f*10)) / 10
}

// Kind classifies an error for logs and alerts: the sentinel's name, which is stable,
// rather than the message, which is not. It reports the cause, so ErrRetriesExhausted
// is checked last.
func Kind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrModelNotFound):
		return "model_not_found"
	case errors.Is(err, ErrContextExceeded):
		return "context_exceeded"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, ErrModelAccessDenied):
		// Before ErrUnauthorized: "you may not use this model" is a far more specific
		// and actionable fact than "your credentials were rejected", and an alert that
		// collapsed them would send someone to look at IAM when the fix is an
		// entitlement.
		return "model_access_denied"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrThrottled):
		// Not "unavailable". The provider is fine; we are over quota. An alert on
		// "the model provider is down" must not fire every time the platform is busy.
		return "throttled"
	case errors.Is(err, ErrStalled):
		return "stalled"
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, ErrStreamBroken):
		return "stream_broken"
	case errors.Is(err, ErrEmptyCompletion):
		return "empty_completion"
	case errors.Is(err, ErrInvalidResponse):
		return "invalid_response"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrRetriesExhausted):
		return "retries_exhausted"
	default:
		return "unknown"
	}
}
