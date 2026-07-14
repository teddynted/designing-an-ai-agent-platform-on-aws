// Package llm is the platform's boundary with whatever produces tokens: a self-hosted
// Ollama (M7), Amazon Bedrock (M8), and — through Bedrock — Claude, which can reason,
// return a schema, and call the platform's own tools (M9). Milestone 10 chooses between
// them per request.
//
// # Milestone 9 changed this package, and the previous two did not
//
// Milestone 8 was the good outcome: a second provider arrived, the interface did not
// change, and only the error vocabulary grew. Claude is not that. Claude is not really a
// new *provider* at all — it is reachable the moment BEDROCK_MODEL_ID names it — it is a
// new set of *demands*, and they land on the interface itself:
//
//   - Generate(prompt) → text cannot express "return exactly this schema".
//   - It cannot express "here are four tools; call them until you are done".
//   - It cannot express "think first, and let me pay for the thinking".
//
// So [Request], [Response], [Message] and [Capabilities] all grew. That is a more
// invasive change than Milestone 8's and it is the honest cost of the capability — but
// note what did NOT change: [Provider] still has the same five methods, and the Ollama
// implementation compiles untouched, because a provider that cannot do these things
// simply says so in [Capabilities] and the platform refuses on its behalf.
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
// # "A retry is safe here" — withdrawn in Milestone 9
//
// Milestones 7 and 8 both said this, in bold, and I believed it:
//
//	Inference is the first integration in this platform where a retry is safe.
//	Milestone 5: retrying an n8n trigger can run a workflow twice. Milestone 6:
//	retrying an agent submission can open a second pull request. Generation has no
//	side effects — it reads a prompt and produces tokens — so the worst case of a
//	retry is paying for the compute twice.
//
// Every sentence of that is true of a model that can only produce tokens. Milestone 9
// gave the model **tools**, and the claim did not survive contact with them.
//
// A model with a `run_workflow` tool can trigger an n8n run. A model with
// `submit_agent_task` can open a pull request and spend money. The inference now has
// side effects — the model chooses them — and "just retry it" has quietly become "run
// the workflow twice", which is the exact failure Milestone 5 spent a milestone
// learning to avoid.
//
// So the position now, precisely:
//
//   - **One inference call is still safe to retry.** It reads and produces tokens.
//     Nothing else. Retrying it inside the provider costs compute and nothing more.
//   - **A tool-using conversation is not**, once a [Write] tool has run. The world has
//     already moved, and it does not roll back because the third turn timed out. That
//     is [ErrEffectsCommitted], and it is terminal, in exactly the way that
//     [ErrStreamBroken] is terminal once a token has escaped to the caller.
//
// The shape of the mistake is worth keeping. The claim was not carelessly made; it was
// *true when it was made*, and it stopped being true because the system grew a
// capability that the claim had never been tested against. That is how load-bearing
// assumptions rot: not by being wrong, but by being outlived.
//
// # The other one that will bite you
//
// Once a stream has emitted a token, it can no longer be retried. The caller has
// already seen half an answer; retrying would send them a second beginning. See
// [Provider.Stream].
package llm

import (
	"context"
	"encoding/json"
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

	// --- added in Milestone 9, by asking the model to DO things ----------------
	//
	// Milestone 8 grew this vocabulary. Milestone 9 grows the interface itself, and
	// these five errors are the shape of that growth.

	// ErrUnsupported means the provider cannot do what was asked: tool use, a
	// structured schema, or reasoning.
	//
	// This exists because the alternative is so much worse. A 3B local model handed a
	// JSON schema does not refuse — it produces something that *looks* like JSON, with
	// the right keys and invented values, and it does it with total confidence. A model
	// handed tools it does not support ignores them and answers from memory.
	//
	// It is the silent-truncation trap (see ErrContextExceeded) wearing different
	// clothes: the failure mode of asking a model for a capability it does not have is
	// not an error, it is a plausible wrong answer. So the platform refuses to ask.
	ErrUnsupported = errors.New("the provider does not support this")

	// ErrSchemaViolation means the model's JSON did not satisfy the schema: a missing
	// required field, a string where an integer belongs, an invented argument.
	//
	// It is a normal event, not an exceptional one. It is what a language model
	// producing structured output does some of the time, and a system that treats it as
	// a crash is a system that falls over on a Tuesday.
	ErrSchemaViolation = errors.New("the model's output did not match the schema")

	// ErrToolLoop means the tool loop hit its iteration bound without the model
	// finishing.
	//
	// Usually the model is stuck: calling the same tool with the same arguments, or
	// ping-ponging between two. The bound exists because the failure mode of an
	// unbounded loop on a per-token API is not a hang — it is a bill.
	ErrToolLoop = errors.New("the tool loop did not converge")

	// ErrToolFailed means a TOOL failed — the platform's own code, not the model's.
	//
	// The distinction matters for who gets paged. The model did nothing wrong; our
	// workflow engine was down. A tool error is normally handed back to the model to
	// recover from (see ToolResult.IsError); this error is for when the runner itself
	// cannot even do that.
	ErrToolFailed = errors.New("a tool failed to run")

	// ErrEffectsCommitted means the inference failed AFTER a Write tool had already
	// changed something.
	//
	// It is the exact analogue of ErrStreamBroken, and it is the most important thing
	// this milestone learned.
	//
	// Milestones 7 and 8 both said, in bold, that a retry is safe here: generation has
	// no side effects, so the worst case is paying for the compute twice. That was true
	// of a model that only produces tokens. It is FALSE of a model that can call
	// run_workflow — because now an inference can trigger an n8n run and open a pull
	// request, and "just retry it" means running the workflow twice, which is precisely
	// the failure Milestone 5 spent a milestone learning to avoid.
	//
	// So a loop that fails after a Write tool has run is terminal, exactly as a stream
	// that fails after a token has escaped is terminal. The caller has to be told that
	// the world already moved.
	ErrEffectsCommitted = errors.New("the inference failed after a tool had already changed something")
)

// Role is who is speaking in a chat-shaped request.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn.
//
// Milestone 9 gave it three more fields, and each one is a thing a tool-using
// conversation cannot be correct without.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`

	// ToolCalls are what an assistant message ASKED for. They must be replayed on the
	// next turn: the model's own request is part of the conversation it is reasoning
	// about, and dropping it produces a model that cannot remember what it just did.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`

	// ToolResults are what the platform ran, and belong on the user turn that follows.
	ToolResults []ToolResult `json:"toolResults,omitempty"`

	// Reasoning is the model's thinking, when reasoning is enabled.
	//
	// It has to be carried back on the next turn, VERBATIM AND INCLUDING ITS SIGNATURE.
	// That is not an optimisation; Bedrock rejects the request without it. See
	// [ReasoningBlock] for why an opaque blob of provider state ended up in the
	// platform's own vocabulary, which is not a decision I enjoyed making.
	Reasoning *ReasoningBlock `json:"reasoning,omitempty"`
}

// ReasoningBlock is the model's thinking, and the platform's least favourite struct.
//
// Text is the reasoning. Signature is an opaque, provider-issued token that proves the
// reasoning has not been tampered with — and Bedrock will refuse the *next* turn of a
// tool-using conversation if it is missing or altered.
//
// So the platform is obliged to carry a piece of state it cannot read, cannot verify and
// cannot construct, purely to hand it back. It is a leak: nothing else in `llm` is opaque
// provider data. The alternatives were worse — drop it, and reasoning + tools cannot be
// combined at all; hide it inside the bedrock package, and the *conversation* (which
// lives here) would no longer be a complete description of itself.
//
// It is at least an honest leak: the field says exactly what it is.
type ReasoningBlock struct {
	Text string `json:"text"`

	// Signature is echoed back untouched. Never log it, never edit it, never generate one.
	Signature string `json:"-"`

	// Redacted carries thinking the provider encrypted rather than showed us. It is
	// still echoed back.
	Redacted []byte `json:"-"`
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

	// --- Milestone 9 ---------------------------------------------------------

	// Tools the model may call. Normally set by [Service.Converse] from the runner,
	// not by a caller.
	Tools []ToolSpec

	// ToolChoice forces a tool. Empty means the model decides, which is what you want
	// almost always; naming one is how [Structured] guarantees a schema-shaped answer.
	ToolChoice string

	// Reasoning asks the model to think before answering.
	//
	// It is not free and it is not always better. Reasoning tokens are billed as OUTPUT
	// tokens — the most expensive kind — and they count against MaxTokens, so a
	// reasoning model given a small budget will think carefully and then get cut off
	// mid-sentence. Turn it on for work that is actually hard, and leave it off for
	// "summarise this diff", where it buys nothing and costs several times the price.
	Reasoning *ReasoningConfig

	// PromptVersion identifies the prompt that produced this request, so a log line can
	// answer "which version of the prompt wrote this?". See internal/prompt.
	PromptVersion string
}

// ReasoningConfig turns on extended thinking.
type ReasoningConfig struct {
	// BudgetTokens is how much the model may spend thinking. It must be less than
	// Options.MaxTokens, because thinking comes out of the same budget as the answer —
	// which is the single easiest way to get a beautifully-reasoned empty response.
	BudgetTokens int
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
//
// It counts the tool schemas and the tool results, and it must. In a tool loop the
// conversation grows every turn — the model's requests, the results we handed back, all of
// it re-sent — and a context check that only measured the original prompt would pass
// happily on turn one and let the model be silently truncated on turn six, which is
// exactly the failure ErrContextExceeded exists to prevent.
func (r Request) Input() string {
	var b strings.Builder
	b.WriteString(r.System)

	for _, t := range r.Tools {
		b.WriteString("\n")
		b.WriteString(t.Name)
		b.WriteString(t.Description)
		if schema, err := json.Marshal(t.Schema); err == nil {
			b.Write(schema)
		}
	}

	if r.Prompt != "" {
		b.WriteString("\n")
		b.WriteString(r.Prompt)
		return b.String()
	}

	for _, m := range r.Messages {
		b.WriteString("\n")
		b.WriteString(m.Content)
		for _, c := range m.ToolCalls {
			b.WriteString(c.Name)
			b.Write(c.Arguments)
		}
		for _, res := range m.ToolResults {
			b.WriteString(res.Content)
		}
		if m.Reasoning != nil {
			b.WriteString(m.Reasoning.Text)
		}
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

	// FinishReason is why generation stopped: "stop", "length", "stop-sequence",
	// and — new in Milestone 9 — "tool_use".
	//
	// "length" is worth noticing — it means the answer was CUT OFF, and a truncated
	// blog post looks a lot like a finished one until someone reads the end.
	//
	// "tool_use" means the model did not finish at all: it stopped to ask for something.
	// A caller that treats it as an answer gets an empty string and no explanation.
	FinishReason string

	// ToolCalls are what the model wants run. Non-empty exactly when FinishReason is
	// "tool_use".
	ToolCalls []ToolCall

	// Reasoning is the model's thinking, if it was asked for. Carry it into the next
	// turn — see [ReasoningBlock].
	Reasoning *ReasoningBlock
}

// WantsTools reports whether the model stopped to ask for a tool rather than answering.
func (r Response) WantsTools() bool { return len(r.ToolCalls) > 0 }

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

	// --- Milestone 9: what the model can actually DO -------------------------
	//
	// These three are why Milestone 10's router is a router and not a load balancer.
	// Until now the providers differed in *where they ran* and *what they cost*; now
	// they differ in what they are capable of, and "send this to whichever is cheaper"
	// stops being a safe thing to say. A structured-output request routed to a model
	// that cannot produce structured output does not fail — it produces confident
	// nonsense, which is worse.
	//
	// # Why these are configured and not discovered
	//
	// You would expect to ask the provider. You cannot: Bedrock's model catalogue
	// (ListFoundationModels) does not report whether a model supports tool use, and
	// Ollama's does not either. The only way to *discover* it is to send a request and
	// see whether you get a ValidationException — which is a discovery mechanism that
	// costs money and fails in production.
	//
	// So capability is asserted by configuration, and the platform REFUSES rather than
	// discovers. That is unsatisfying, and it is the honest option.

	// Tools reports whether the model can call tools.
	Tools bool

	// StructuredOutput reports whether the model can be held to a JSON schema.
	//
	// On Bedrock this is implemented AS tool use — a single tool, forced — so in
	// practice it tracks Tools. It is a separate field because that is an
	// implementation detail of one provider and not a fact about the world.
	StructuredOutput bool

	// Reasoning reports whether the model supports extended thinking.
	Reasoning bool
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
