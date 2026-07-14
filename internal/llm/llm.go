// Package llm is the platform's boundary with whatever produces tokens. Today
// that is a self-hosted Ollama; tomorrow it is Bedrock, or Claude, or all three at
// once with something choosing between them.
//
// # A claim from Milestone 6, corrected
//
// Milestone 6 says, in several places and in bold: *the platform calls no model;
// the agent does*. That was true, and it is still true of the sentence it was
// making — but it is not the whole truth any more, and it would be dishonest to
// quietly widen it.
//
// The precise position now:
//
//   - **The agent's inference is still the agent's.** When OpenClaw reasons, it
//     calls its own model, behind its own boundary. This package is not in that
//     path, does not route it, and must not learn about it. Swapping the agent's
//     model remains a change in openclaw-on-aws that this repository does not
//     notice.
//   - **The platform now has inference of its own.** Not everything worth doing
//     with a model needs an agent. "Summarise this diff", "write release notes from
//     these commits", "classify this event" are single-shot: one prompt, one
//     completion, no shell, no tools, no loop. Routing those through an agent means
//     paying for a reasoning loop to do a job that needs none of it.
//
// So there are two consumers of inference and they are different: the *agent
// plane*, which owns its own model calls, and the *platform*, which now has an
// inference plane for work that is a function call rather than an errand. This
// package is the second one — and the repository's scope has said so from the
// beginning: it owns "the provider abstraction over model backends".
//
// # Why an interface, before there is a second provider
//
// The usual argument for an abstraction — "we might swap it one day" — is a promise
// nobody collects on. This one is different, because the swap is already on the
// roadmap and it is the *point*: Milestone 8 adds Bedrock, Milestone 9 adds Claude,
// and Milestone 10 routes between them per request by cost, latency, and
// capability. A provider abstraction added at Milestone 10, on top of three call
// sites that each learned Ollama's JSON shape, is a rewrite. Added now, it is an
// interface with one implementation.
//
// [Capabilities] is the forward-looking part, and it is deliberately small: the
// facts a router will need (is it local, what does it cost, how much context does
// it have) and nothing else. It is not a plugin system.
//
// # The one that will bite you
//
// Inference is the first integration in this platform where **a retry is safe**.
//
// Milestone 5: retrying an n8n trigger can run a workflow twice. Milestone 6:
// retrying an agent submission can open a second pull request and spend real money.
// Both needed idempotency keys and careful, narrow retry policies.
//
// Generation has no side effects. It reads a prompt and produces tokens. Retrying
// it costs compute and nothing else — no duplicate pull request, no second
// invoice, nothing to deduplicate. That is a genuinely easier world, and it is
// worth saying out loud, because the reflex built over the last two milestones was
// "be terrified of retrying".
//
// With one exception, and it is the sharp edge of this milestone: **once a stream
// has emitted a token, it can no longer be retried.** The caller has already seen
// half an answer. Retrying would send them a second beginning. See [Provider.Stream].
package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
)

// ErrRetriesExhausted is re-exported from the shared transport so a caller never
// has to import httpx to classify a failure.
var ErrRetriesExhausted = httpx.ErrRetriesExhausted

// Errors the platform can act on, whatever the provider. They exist so a caller can
// decide what to *do* without parsing an HTTP status or a vendor's error string.
var (
	// ErrUnavailable means the provider could not be reached at all.
	ErrUnavailable = errors.New("llm provider unavailable")

	// ErrTimeout means the provider did not answer within the budget. For inference
	// this is a blunter instrument than it looks — see [Config.IdleTimeout] in the
	// ollama package for why a *stall* timeout is the more useful one.
	ErrTimeout = errors.New("llm provider timed out")

	// ErrStalled means the model started producing tokens and then stopped, without
	// finishing. It is distinct from a timeout on purpose: a slow model on a CPU is
	// not a broken one, and a total timeout cannot tell the difference. A stall can.
	ErrStalled = errors.New("llm stopped producing tokens")

	// ErrModelNotFound means the model is not on the instance. It is not a transport
	// failure and retrying will not download it — the fix is a `ollama pull`, which
	// this milestone deliberately does not do for you.
	ErrModelNotFound = errors.New("model not found on the provider")

	// ErrInvalidRequest means the request cannot be sent as it stands: an empty
	// prompt, or one larger than the model's context window.
	ErrInvalidRequest = errors.New("invalid inference request")

	// ErrContextExceeded means the prompt is bigger than the context window it is
	// being sent to.
	//
	// This is its own error, rather than a flavour of ErrInvalidRequest, because it
	// is the failure that does the most damage while looking like success: a model
	// asked to read more than it can hold does not refuse. It silently drops the
	// beginning of the prompt and answers confidently from what is left.
	ErrContextExceeded = errors.New("prompt exceeds the model's context window")

	// ErrInvalidResponse means the provider answered with something we cannot trust.
	ErrInvalidResponse = errors.New("invalid response from llm provider")

	// ErrEmptyCompletion means the model produced nothing at all. A successful HTTP
	// call that yields zero tokens is not a success; it is a model that is loaded
	// wrong, or a prompt that was truncated to nothing.
	ErrEmptyCompletion = errors.New("model produced no output")

	// ErrStreamBroken means the stream failed AFTER tokens had already been sent to
	// the caller. It is deliberately not retryable: the caller has half an answer,
	// and a retry would hand them a second beginning.
	ErrStreamBroken = errors.New("stream broke after output had begun")

	// --- added in Milestone 8, by the second provider ------------------------
	//
	// These three did not exist until Bedrock arrived, and their absence is worth
	// dwelling on rather than quietly fixing.
	//
	// Milestone 7 designed this vocabulary against exactly one implementation —
	// Ollama — which has no authentication (it is a tool meant for a laptop) and
	// does not throttle (it is one process on one box; it just goes slower). So the
	// abstraction had no word for "your credentials are wrong" and no word for "you
	// are asking too often", because nothing had ever needed one.
	//
	// That is the ordinary way abstractions are wrong: not by being badly designed,
	// but by being designed from a sample of one. A second implementation is the
	// only thing that reveals it, which is a large part of why the second one is
	// worth doing before you have three call sites depending on the first.

	// ErrUnauthorized means the provider rejected our credentials or refused the
	// call. Retrying cannot help; a human must fix an IAM policy or a key.
	ErrUnauthorized = errors.New("llm provider rejected our credentials")

	// ErrModelAccessDenied means the model EXISTS and this account is not allowed to
	// use it.
	//
	// It is distinct from ErrUnauthorized on purpose, because the fix is different
	// and so is the person who applies it: ErrUnauthorized is an IAM problem for an
	// engineer, and this is an entitlement someone has to request — in Bedrock, a
	// per-model access grant in the console, which is the single most common reason
	// a correct-looking Bedrock call fails on a fresh account.
	ErrModelAccessDenied = errors.New("the account is not granted access to this model")

	// ErrThrottled means the provider is rate-limiting us.
	//
	// It is distinct from ErrUnavailable because it means something completely
	// different operationally. Unavailable is "the provider is broken"; throttled is
	// "the provider is fine, and you are over your quota" — which is a capacity and
	// cost signal, not an outage, and it is the failure a hosted provider actually
	// hands you under load. Collapsing the two would make an alert on "the model
	// provider is down" fire every time the platform got busy.
	ErrThrottled = errors.New("llm provider is throttling us")
)

// Role is who is speaking in a chat-shaped request.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// Options are the generation knobs every provider has some version of. They are
// deliberately few: the ones that change the *answer* rather than the plumbing.
type Options struct {
	// Temperature: 0 is deterministic-ish, higher is more creative. For summarising
	// a diff you want it low; for drafting prose, less so.
	Temperature float64

	// MaxTokens bounds the completion. Zero means the provider's default.
	//
	// Like the agent's step budget in Milestone 6, this is a spend guard: on a GPU
	// instance the currency is time, and an unbounded generation is a model free to
	// ramble for as long as it likes on hardware you are paying for by the hour.
	MaxTokens int

	// Stop sequences end generation early.
	Stop []string

	// Seed makes a generation reproducible where the provider supports it — which
	// matters more than it sounds. "The model wrote something odd" is not a bug
	// report you can act on unless you can make it happen again.
	Seed int
}

// Purpose says what this inference is *for* — "release-notes", "diff-summary".
//
// It is not sent to the model. It exists so logs and metrics can answer "what is
// this platform spending its tokens on?", which is the first question anyone asks
// when the GPU bill arrives, and the last one an unlabelled log can answer.
type Purpose string

// Request is one inference.
type Request struct {
	// Model is which model to use. Empty means the provider's configured default,
	// so a caller that does not care does not have to choose.
	Model string

	// System is the instruction the model is given about *how* to behave. It comes
	// from the platform, never from the content being processed.
	System string

	// Prompt is a single-shot instruction. Use this or Messages, not both.
	Prompt string

	// Messages is the chat form, for a conversation with history.
	Messages []Message

	Options Options
	Purpose Purpose

	// CorrelationID and WorkflowExecutionID continue the chain that began at the
	// GitHub delivery: webhook → n8n → (agent) → inference. Without them, a slow
	// generation is a mystery rather than a step in something.
	CorrelationID       string
	WorkflowExecutionID string
}

// Validate reports whether the request can be sent.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Prompt) == "" && len(r.Messages) == 0 {
		return fmt.Errorf("%w: no prompt and no messages", ErrInvalidRequest)
	}
	if r.Prompt != "" && len(r.Messages) > 0 {
		// Both would be ambiguous, and a provider is free to resolve that ambiguity
		// differently from the one you tested against.
		return fmt.Errorf("%w: set Prompt or Messages, not both", ErrInvalidRequest)
	}
	if r.Options.Temperature < 0 || r.Options.Temperature > 2 {
		return fmt.Errorf("%w: temperature %.2f is outside [0, 2]", ErrInvalidRequest, r.Options.Temperature)
	}
	return nil
}

// Input returns everything the model will read, for size checks. It is not for
// logging: see the note on [Usage].
func (r Request) Input() string {
	if r.Prompt != "" {
		return r.System + "\n" + r.Prompt
	}
	var b strings.Builder
	b.WriteString(r.System)
	for _, m := range r.Messages {
		b.WriteString("\n")
		b.WriteString(m.Content)
	}
	return b.String()
}

// Usage is what the generation cost and how fast it went.
//
// TokensPerSecond is the number to watch, and it is the reason this struct exists
// at all. It tells you, from a log line and without logging in to anything, whether
// the model is running on a GPU or quietly fell back to a CPU: single-digit
// tokens/sec is a CPU, and it means an inference the platform expects to take
// seconds is going to take minutes.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TokensPerSecond  float64

	// LoadDuration is how long the provider spent loading the model into memory
	// before generating anything. A large value on every request means the model is
	// being evicted between calls, which is a keep-alive setting, not a mystery.
	LoadDuration time.Duration
	EvalDuration time.Duration
}

// Response is a finished generation.
type Response struct {
	Model    string
	Content  string
	Usage    Usage
	Duration time.Duration
	Attempts int

	// FinishReason is why generation stopped: "stop", "length", "stop-sequence".
	// "length" is worth noticing — it means the answer was CUT OFF, and a truncated
	// blog post looks a lot like a finished one until someone reads the end.
	FinishReason string
}

// Chunk is a piece of a streamed response.
type Chunk struct {
	Content string
	Done    bool
}

// Sink receives streamed chunks. Returning an error stops the stream — which is how
// a caller cancels a generation that has already started saying something wrong.
type Sink func(Chunk) error

// Model is a model the provider has.
type Model struct {
	Name string
	// Family is the architecture: "llama", "qwen2", "gemma".
	Family string
	// ParameterSize is "7B", "70B" — the number that decides whether this needs a GPU.
	ParameterSize string
	// Quantization is "Q4_K_M" — the number that decides whether it fits in one.
	Quantization string
	SizeBytes    int64
	ModifiedAt   time.Time
}

// Capabilities are the facts a future router will need in order to choose between
// providers (Milestone 10). Small on purpose: enough to route by, not a plugin API.
type Capabilities struct {
	// Local reports whether inference happens on infrastructure we control.
	//
	// This is the single most important field here, and it is not about latency or
	// cost. It is about whether the prompt LEAVES. The platform's prompts contain
	// repository content — source code, commit messages, sometimes things nobody
	// meant to commit. With a local provider that content never leaves the VPC. With
	// a hosted one it crosses the internet to a third party, and no amount of TLS
	// changes the fact that you have sent them your source.
	//
	// For some repositories that is fine. For others it is the whole ball game, and a
	// router must be able to see the difference.
	Local bool

	Streaming bool

	// MaxContextTokens is how much the provider can read at once.
	MaxContextTokens int

	// CostPer1MInputTokensUSD and CostPer1MOutputTokensUSD are zero for a local
	// provider — the cost is the instance, and it is paid whether or not you use it.
	// A router weighs that against a hosted provider's per-token bill.
	CostPer1MInputTokensUSD  float64
	CostPer1MOutputTokensUSD float64
}

// Provider produces tokens. It is the seam: Ollama implements it today; Bedrock
// (M8), Claude (M9), and whatever routes between them (M10) implement it next.
//
// An implementation owns its transport, its authentication, its retries, and the
// translation of its failures into this package's errors. It does NOT own logging,
// validation, or correlation — the [Service] does those once, so every provider gets
// them identically and none can forget.
type Provider interface {
	// Name identifies the provider in logs: "ollama".
	Name() string

	// Capabilities describes it, for a router that must choose.
	Capabilities() Capabilities

	// Models lists what is available. It is how the platform checks that the model
	// it is configured to use actually exists, before it needs it.
	Models(ctx context.Context) ([]Model, error)

	// Generate runs an inference and returns the whole completion.
	Generate(ctx context.Context, req Request) (Response, error)

	// Stream runs an inference, handing each chunk to the sink as it arrives, and
	// returns the assembled response at the end.
	//
	// # Retries and streams do not mix
	//
	// An implementation may retry a stream that fails BEFORE the first token — the
	// caller has seen nothing, and nothing is lost. It must NOT retry one that fails
	// after: the caller already has the beginning of an answer, and a retry would
	// hand them a second beginning, silently concatenated onto the first.
	//
	// A stream that breaks mid-flight is [ErrStreamBroken], and it is terminal.
	Stream(ctx context.Context, req Request, sink Sink) (Response, error)
}
