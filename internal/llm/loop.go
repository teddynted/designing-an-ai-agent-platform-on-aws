package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultMaxIterations bounds the tool loop.
//
// Eight is a judgement, not a discovery. Real work — "list the workflows, then run the
// right one" — converges in two or three. A model that has taken eight turns is not
// making progress; it is calling the same tool with slightly different arguments, and it
// will keep doing so, cheerfully, until something stops it.
//
// On a per-token API, the failure mode of an unbounded loop is not a hang that someone
// notices. It is a bill that arrives at the end of the month. THAT is why there is a
// bound, and why it is small.
const DefaultMaxIterations = 8

// LoopPolicy bounds a tool-using conversation.
type LoopPolicy struct {
	// MaxIterations is how many times the model may go round: think, call tools, read
	// the results, think again. Zero means DefaultMaxIterations.
	MaxIterations int

	// MaxCostUSD stops the loop if the accumulated cost passes it. Zero means no bound.
	//
	// It is the same idea as Milestone 6's mandatory agent budget, arrived at from the
	// other direction: an agent's budget is enforced by the agent, and a tool loop is
	// enforced by us, because we are the ones going round it.
	MaxCostUSD float64
}

func (p LoopPolicy) iterations() int {
	if p.MaxIterations <= 0 {
		return DefaultMaxIterations
	}
	return p.MaxIterations
}

// Turn is one round of the loop: what the model said, what it asked for, what it got.
type Turn struct {
	Index     int
	Response  Response
	Results   []ToolResult
	Effectful bool
}

// Conversation is a finished tool-using exchange.
type Conversation struct {
	// Content is the model's final answer, after all the tools have run.
	Content string
	Model   string

	Turns []Turn

	// Usage is the SUM across every turn, and it is usually a larger number than people
	// expect — see the note on cost in [Service.Converse].
	Usage            Usage
	EstimatedCostUSD float64
	Duration         time.Duration

	// EffectsCommitted reports that at least one Write tool ran. If this is true and an
	// error came back with it, the conversation MUST NOT be retried wholesale: something
	// in the world has already changed.
	EffectsCommitted bool

	// Messages is the full conversation, replayable. It is what you would send to
	// continue it, and what you would read to explain it.
	Messages []Message
}

// Converse runs a tool-using conversation to completion.
//
// # The loop
//
// Ask the model. If it answers, we are done. If instead it asks for tools, run them, hand
// back the results, and ask again — until it answers, or the bound is reached.
//
// # The cost, which surprises everyone
//
// A tool loop resends **the entire conversation on every turn**. The system prompt, the
// tool schemas, the original question, every previous answer, and every tool result — all
// of it, again, as input tokens. A five-turn loop is not five times the cost of one call;
// it is closer to the sum of 1..5, because turn five re-reads everything that came before
// it.
//
// This is the single most expensive thing in the platform, and it is why the loop tracks
// cost per turn, why [LoopPolicy.MaxCostUSD] exists, and why prompt caching (which lets
// the unchanging prefix — system prompt and tool schemas — be billed at a fraction) is
// worth the configuration it costs.
//
// # The retry rule, which is the point of the milestone
//
// Each individual inference is still retried by the provider: it has no side effects.
// But the moment a [Write] tool runs, the CONVERSATION is no longer replayable, and any
// error from here on is wrapped in [ErrEffectsCommitted] so that a caller — n8n, a
// workflow, a human — knows that the world already moved and a clean retry is not
// available to them.
func (s *Service) Converse(ctx context.Context, req Request, runner ToolRunner, policy LoopPolicy) (Conversation, error) {
	start := s.now()
	convo := Conversation{Model: s.modelName(req)}

	caps := s.provider.Capabilities()
	if !caps.Tools {
		// Refuse, rather than send the tools and hope. A model that does not support tool
		// use does not say so — it ignores them and answers from memory, fluently, and the
		// answer is wrong in a way that no log line will ever mention.
		return convo, fmt.Errorf("%w: %s cannot use tools, so the platform will not pretend "+
			"it can — a model given tools it does not understand does not refuse, it invents "+
			"an answer instead. Use a provider with Tools capability (LLM_PROVIDER=bedrock "+
			"with a Claude model)", ErrUnsupported, s.provider.Name())
	}

	specs := runner.Specs()
	if len(specs) == 0 {
		return convo, fmt.Errorf("%w: the tool runner offers no tools", ErrInvalidRequest)
	}

	// The conversation starts as whatever the caller gave us, and grows.
	messages := req.Messages
	if len(messages) == 0 {
		messages = []Message{{Role: RoleUser, Content: req.Prompt}}
	}

	log := s.log.With(
		"provider", s.provider.Name(),
		"correlationId", req.CorrelationID,
		"purpose", string(req.Purpose),
		"tools", len(specs),
		"maxIterations", policy.iterations(),
	)
	log.Info("tool conversation started")

	for i := 0; i < policy.iterations(); i++ {
		turnReq := req
		turnReq.Prompt = ""
		turnReq.Messages = messages
		turnReq.Tools = specs

		res, err := s.Generate(ctx, turnReq)
		convo.Usage.PromptTokens += res.Usage.PromptTokens
		convo.Usage.CompletionTokens += res.Usage.CompletionTokens
		convo.EstimatedCostUSD = s.cost(convo.Usage)

		if err != nil {
			return s.abort(convo, start, messages, err)
		}

		turn := Turn{Index: i, Response: res}

		// The model answered. This is the exit.
		if !res.WantsTools() {
			convo.Content = res.Content
			convo.Turns = append(convo.Turns, turn)
			convo.Messages = append(messages, Message{
				Role: RoleAssistant, Content: res.Content, Reasoning: res.Reasoning,
			})
			convo.Duration = s.now().Sub(start)

			log.Info("tool conversation completed",
				"turns", len(convo.Turns),
				"promptTokens", convo.Usage.PromptTokens,
				"completionTokens", convo.Usage.CompletionTokens,
				"estimatedCostUsd", convo.EstimatedCostUSD,
				"effectsCommitted", convo.EffectsCommitted,
				"durationMs", convo.Duration.Milliseconds(),
			)
			return convo, nil
		}

		// The model wants tools. Replay its request — including the reasoning, which
		// Bedrock will demand back verbatim on the next turn.
		messages = append(messages, Message{
			Role:      RoleAssistant,
			Content:   res.Content,
			ToolCalls: res.ToolCalls,
			Reasoning: res.Reasoning,
		})

		results := make([]ToolResult, 0, len(res.ToolCalls))
		for _, call := range res.ToolCalls {
			result, effectful, err := s.runTool(ctx, log, req, runner, specs, call)
			if effectful {
				// Set BEFORE checking the error: a tool that failed halfway through may
				// still have done something, and the honest assumption when we cannot know
				// is that it did.
				convo.EffectsCommitted = true
			}
			if err != nil {
				// The runner itself broke — not the tool reporting a failure, which is a
				// result and goes back to the model, but the runner unable to run at all.
				return s.abort(convo, start, messages, err)
			}
			results = append(results, result)
		}

		turn.Results = results
		turn.Effectful = convo.EffectsCommitted
		convo.Turns = append(convo.Turns, turn)

		messages = append(messages, Message{Role: RoleUser, ToolResults: results})

		if policy.MaxCostUSD > 0 && convo.EstimatedCostUSD > policy.MaxCostUSD {
			err := fmt.Errorf("%w: the conversation has cost ~$%.4f, over the $%.4f budget, "+
				"after %d turns", ErrToolLoop, convo.EstimatedCostUSD, policy.MaxCostUSD, i+1)
			return s.abort(convo, start, messages, err)
		}
	}

	// Out of iterations. The model is not converging: almost always it is calling the same
	// tool over and over, and the bound is the only thing that was ever going to stop it.
	err := fmt.Errorf("%w: the model was still calling tools after %d turns and never "+
		"produced an answer. It is usually stuck repeating one call — check the tool "+
		"descriptions, which are the only thing it reads when choosing",
		ErrToolLoop, policy.iterations())
	return s.abort(convo, start, messages, err)
}

// runTool validates, runs, logs, and times one tool call.
func (s *Service) runTool(
	ctx context.Context,
	log *slog.Logger,
	req Request,
	runner ToolRunner,
	specs []ToolSpec,
	call ToolCall,
) (ToolResult, bool, error) {
	spec, known := Lookup(specs, call.Name)
	if !known {
		// The model invented a tool. It happens, and it is not fatal: hand the failure
		// back and let it choose again from the list it was given.
		log.Warn("the model called a tool that does not exist", "tool", call.Name)
		return ToolResult{
			ID: call.ID, Name: call.Name, IsError: true,
			Content: fmt.Sprintf("There is no tool called %q. The tools you may call are listed "+
				"in this conversation; use one of those.", call.Name),
		}, false, nil
	}

	effectful := spec.Effect == Write

	// Validate BEFORE running. The schema is advice to the model and a contract for us,
	// and the tool implementation must never be the first thing to notice that the model
	// sent a string where an integer belongs.
	if err := ValidateArguments(spec, call.Arguments); err != nil {
		log.Warn("the model called a tool with arguments that do not fit its schema",
			"tool", call.Name, "error", err, "errorKind", Kind(err))
		// Also not fatal, and this is the whole reason ToolResult has IsError. Handing the
		// validation message back is what lets the model *fix it* — and it usually does,
		// on the next turn, which is a far better outcome than failing the request.
		return ToolResult{
			ID: call.ID, Name: call.Name, IsError: true,
			Content: "The arguments were rejected: " + err.Error() + ". Call it again with arguments that match the schema.",
		}, false, nil
	}

	// Derived, never random. The same call, retried, must produce the same key — see
	// [DeriveIdempotencyKey].
	call.IdempotencyKey = DeriveIdempotencyKey(req.CorrelationID, call.Name, call.Arguments)

	log = log.With(
		"tool", call.Name,
		"effect", string(spec.Effect),
		"toolCallId", call.ID,
		"idempotencyKey", call.IdempotencyKey,
		// The ARGUMENTS are not logged. They are derived from the prompt, and the prompt is
		// repository content — the same reason the prompt itself is only ever hashed.
		"argumentsHash", hash(string(call.Arguments)),
	)

	if effectful {
		// A Write tool crossing the boundary is the single most consequential thing that
		// happens in this package, and it gets its own log line at INFO — because when
		// somebody asks "why did this pull request appear?", this is the line that answers.
		log.Info("the model is calling a tool that CHANGES SOMETHING")
	} else {
		log.Debug("the model is calling a read-only tool")
	}

	start := s.now()
	result, err := runner.Run(ctx, call)
	elapsed := s.now().Sub(start)

	if err != nil {
		log.Error("the tool runner failed", "error", err, "errorKind", Kind(err),
			"durationMs", elapsed.Milliseconds())
		return ToolResult{}, effectful, fmt.Errorf("%w: %s: %v", ErrToolFailed, call.Name, err)
	}

	result.ID, result.Name = call.ID, call.Name
	log.Info("tool completed",
		"durationMs", elapsed.Milliseconds(),
		"toolError", result.IsError,
		"resultChars", len(result.Content),
	)
	return result, effectful, nil
}

// abort ends a conversation with an error, and — this is the important part — marks it
// terminal if a Write tool has already run.
func (s *Service) abort(convo Conversation, start time.Time, messages []Message, err error) (Conversation, error) {
	convo.Duration = s.now().Sub(start)
	convo.Messages = messages

	if convo.EffectsCommitted {
		// Wrap BOTH: the cause, so a log can say what went wrong, and ErrEffectsCommitted,
		// so a caller's retry policy can see that it must not run this again. Exactly the
		// pattern ErrStreamBroken uses, for exactly the same reason.
		err = fmt.Errorf("%w: %w", ErrEffectsCommitted, err)
	}

	s.log.Error("tool conversation failed",
		"error", err,
		"errorKind", Kind(err),
		"turns", len(convo.Turns),
		"effectsCommitted", convo.EffectsCommitted,
		"estimatedCostUsd", convo.EstimatedCostUSD,
		"durationMs", convo.Duration.Milliseconds(),
		// The loudest field in the file. If this is true, a retry is not available: a
		// workflow has run, or a pull request exists, and doing it again would do it twice.
		"safeToRetry", !convo.EffectsCommitted,
	)
	return convo, err
}

// cost estimates what the conversation has spent so far, from the provider's own prices.
func (s *Service) cost(u Usage) float64 {
	caps := s.provider.Capabilities()
	in := float64(u.PromptTokens) / 1e6 * caps.CostPer1MInputTokensUSD
	out := float64(u.CompletionTokens) / 1e6 * caps.CostPer1MOutputTokensUSD
	return in + out
}

// ToolsJSON renders the tool results for a model to read.
func ToolsJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Retryable reports whether a failed inference may be tried again from the top.
//
// It is the one-line summary of everything Milestone 9 learned, and it is the function a
// caller should reach for rather than matching on errors themselves.
func Retryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrEffectsCommitted):
		// A tool has already changed something. There is no clean retry available.
		return false
	case errors.Is(err, ErrStreamBroken):
		// The caller has half an answer. A retry gives them a second beginning.
		return false
	case errors.Is(err, ErrThrottled), errors.Is(err, ErrUnavailable),
		errors.Is(err, ErrTimeout), errors.Is(err, ErrStalled):
		return true
	default:
		// Everything else — unauthorised, access denied, unsupported, schema violation,
		// context exceeded — is deterministic. Asking again produces the same answer.
		return false
	}
}
