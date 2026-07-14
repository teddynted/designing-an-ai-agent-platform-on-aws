package bedrock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// Environment variables. As everywhere else in this platform, everything that differs
// between a laptop, dev and prod is here — and, notably, **no credentials are**. See
// [Config.Credentials].
const (
	// EnvRegion is the AWS region Bedrock is called in. It matters more than it looks:
	// model availability, per-model pricing, and quotas are all regional, and a model
	// that exists in us-east-1 may simply not be offered in eu-west-2.
	EnvRegion = "BEDROCK_REGION"

	// EnvModelID is the foundation model, e.g.
	//
	//	anthropic.claude-3-5-haiku-20241022-v1:0        (a direct model ID)
	//	us.anthropic.claude-3-5-sonnet-20241022-v2:0    (a cross-region inference profile)
	//
	// The "us." prefix is not decoration. Newer models are only available on-demand
	// through a cross-region *inference profile*, which routes the request to whichever
	// region in the group has capacity. Passing the bare model ID for one of those gets
	// you a ValidationException that says, unhelpfully, that the model is not supported —
	// when it is, just not the way you asked for it.
	EnvModelID = "BEDROCK_MODEL_ID"

	// EnvContextTokens is the model's context window.
	//
	// Bedrock will not tell you this — there is no API that reports it — so the platform
	// is told, and refuses to send a prompt that would not fit. Unlike Ollama, Bedrock
	// does at least *reject* an oversized prompt rather than silently truncating it, so
	// the consequence of getting this wrong is a wasted round trip rather than a
	// confidently wrong answer. That is a mercy, and it is not a reason to be careless.
	EnvContextTokens = "BEDROCK_CONTEXT_TOKENS"

	// EnvMaxTokens is the completion budget. Here it is not a guard against a rambling
	// model wasting your GPU hours (Milestone 7) — it is a guard against a rambling model
	// spending your money, because output tokens are the expensive half of a Bedrock bill.
	EnvMaxTokens = "BEDROCK_MAX_TOKENS"

	EnvTemperature = "BEDROCK_TEMPERATURE"

	// EnvTimeout bounds a single non-streaming call.
	EnvTimeout = "BEDROCK_TIMEOUT"

	// EnvIdleTimeout bounds the silence within a stream — the same stall detection as
	// Milestone 7, for the same reason: a total timeout cannot tell a slow model from a
	// hung one.
	EnvIdleTimeout = "BEDROCK_IDLE_TIMEOUT"

	// EnvRetryAttempts is the TOTAL attempts we will make.
	//
	// Note carefully: this is *our* retry count, and the AWS SDK's own retryer is turned
	// OFF. Two retry layers multiply — three of ours over three of the SDK's is nine
	// requests to a throttled endpoint — and they hide each other, so the attempt count
	// in our logs would be a lie. See [Config.awsRetryDisabled].
	EnvRetryAttempts = "BEDROCK_RETRY_ATTEMPTS"
	EnvRetryDelay    = "BEDROCK_RETRY_DELAY"

	// EnvTopP is nucleus sampling. Unset by default — see Config.TopP.
	EnvTopP = "BEDROCK_TOP_P"

	// EnvStream selects streaming by default.
	EnvStream = "BEDROCK_STREAM"

	// EnvPromptCache turns on Bedrock prompt caching (Milestone 9).
	//
	// It matters most in a tool loop, where the system prompt and every tool schema are
	// re-sent on every turn. Caching that stable prefix bills it at a fraction after the
	// first read — and on a six-turn conversation that is most of the invoice.
	EnvPromptCache = "BEDROCK_PROMPT_CACHE"

	// EnvTools and EnvReasoning override the capabilities inferred from the model ID.
	//
	// They exist because Bedrock will not tell us. ListFoundationModels does not report
	// whether a model supports tool use, so the platform infers it from the model name
	// and lets an operator correct it — which is unsatisfying, and better than finding
	// out through a ValidationException in production.
	EnvTools     = "BEDROCK_TOOLS"
	EnvReasoning = "BEDROCK_REASONING"

	// EnvEndpoint overrides the Bedrock endpoint. Its real purpose is a **VPC endpoint**
	// (AWS PrivateLink) for bedrock-runtime, which keeps the prompt off the public
	// internet — see the note on Local in Capabilities. It is also what a test or a local
	// stub points at.
	EnvEndpoint = "BEDROCK_ENDPOINT"

	// EnvInputCostPer1M and EnvOutputCostPer1M are the model's price, in USD per million
	// tokens.
	//
	// They are configuration rather than a lookup table baked into this repository,
	// because a price table in source code is a price table that is wrong. AWS changes
	// prices, and a platform that believes a stale constant will make routing decisions
	// (Milestone 10) on a number that has not been true for a year.
	EnvInputCostPer1M  = "BEDROCK_INPUT_COST_PER_1M_USD"
	EnvOutputCostPer1M = "BEDROCK_OUTPUT_COST_PER_1M_USD"
)

// Defaults.
const (
	DefaultRegion        = "us-east-1"
	DefaultContextTokens = 200_000 // Claude 3.5's window; override for a smaller model
	DefaultMaxTokens     = 2048
	DefaultTemperature   = 0.2
	DefaultTimeout       = 2 * time.Minute
	DefaultIdleTimeout   = 60 * time.Second
	DefaultRetries       = 3
	DefaultRetryDelay    = time.Second
)

// ErrConfig means the integration is misconfigured. Always fatal at start-up.
var ErrConfig = errors.New("bedrock configuration")

// Config is everything the provider needs.
//
// # Where the credentials are
//
// There are none here, and that is the point.
//
// Bedrock is authenticated with AWS IAM, and the SDK resolves credentials through its
// default chain: an EC2 instance profile, an ECS task role, a Lambda execution role, an
// OIDC web identity, or a developer's local profile. Every one of those is a *temporary*
// credential that AWS rotates.
//
// So there is no BEDROCK_ACCESS_KEY and there never will be. A static key in an
// environment variable is a credential that cannot be rotated, will be copied into a CI
// secret, and will eventually be printed by something. The platform already has an
// instance role ([infra/cloudformation/02-iam.yaml]); Bedrock access is a policy on it,
// not a secret in a file.
//
// This is the one genuinely nice thing about a hosted provider that runs in your own
// cloud: the authentication problem is already solved, and solved better than you would
// have solved it.
type Config struct {
	Region   string
	ModelID  string
	Endpoint string

	ContextTokens int
	MaxTokens     int
	Temperature   float64
	Stream        bool
	PromptCache   bool

	// TopP is nucleus sampling, and it is ZERO (unset) by default on purpose.
	//
	// Temperature and TopP are two knobs on the same distribution. Anthropic's own guidance
	// is to tune one of them and leave the other at its default — so a platform that shipped
	// a non-zero default for both would be pulling the model in two directions on every
	// request, and nobody would ever be able to say which knob was doing what.
	TopP float64

	// Tools and Reasoning are what this model can DO. Inferred from the model ID
	// (Claude can; most others cannot do all of it) and overridable.
	Tools     bool
	Reasoning bool

	Timeout       time.Duration
	IdleTimeout   time.Duration
	RetryAttempts int
	RetryDelay    time.Duration

	InputCostPer1M  float64
	OutputCostPer1M float64
}

// ConfigFromEnv reads the configuration and refuses to return anything half-built.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Region:        envOr(EnvRegion, DefaultRegion),
		ModelID:       strings.TrimSpace(os.Getenv(EnvModelID)),
		Endpoint:      strings.TrimSpace(os.Getenv(EnvEndpoint)),
		ContextTokens: DefaultContextTokens,
		MaxTokens:     DefaultMaxTokens,
		Temperature:   DefaultTemperature,
		Stream:        true,
		Timeout:       DefaultTimeout,
		IdleTimeout:   DefaultIdleTimeout,
		RetryAttempts: DefaultRetries,
		RetryDelay:    DefaultRetryDelay,
	}

	if cfg.ModelID == "" {
		return Config{}, fmt.Errorf("%w: %s is not set (there is no sensible default foundation model)",
			ErrConfig, EnvModelID)
	}

	var err error
	if cfg.ContextTokens, err = envInt(EnvContextTokens, DefaultContextTokens); err != nil {
		return Config{}, err
	}
	if cfg.ContextTokens < 512 {
		return Config{}, fmt.Errorf("%w: %s = %d is too small to be real", ErrConfig, EnvContextTokens, cfg.ContextTokens)
	}
	if cfg.MaxTokens, err = envInt(EnvMaxTokens, DefaultMaxTokens); err != nil {
		return Config{}, err
	}
	if cfg.MaxTokens < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1", ErrConfig, EnvMaxTokens)
	}
	if cfg.Temperature, err = envFloat(EnvTemperature, DefaultTemperature); err != nil {
		return Config{}, err
	}
	if cfg.Temperature < 0 || cfg.Temperature > 1 {
		// Bedrock's Converse API takes temperature in [0, 1] — narrower than Ollama's
		// [0, 2]. A value that is merely "high" for one provider is a validation error
		// for the other, which is exactly the kind of difference an abstraction has to
		// absorb rather than pass on.
		return Config{}, fmt.Errorf("%w: %s must be in [0, 1] for Bedrock (Ollama allows up to 2)",
			ErrConfig, EnvTemperature)
	}
	if cfg.Timeout, err = envDuration(EnvTimeout, DefaultTimeout); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = envDuration(EnvIdleTimeout, DefaultIdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RetryDelay, err = envDuration(EnvRetryDelay, DefaultRetryDelay); err != nil {
		return Config{}, err
	}
	if cfg.RetryAttempts, err = envInt(EnvRetryAttempts, DefaultRetries); err != nil {
		return Config{}, err
	}
	if cfg.RetryAttempts < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1 (it counts total attempts, not retries)",
			ErrConfig, EnvRetryAttempts)
	}
	// Claude is the reason this milestone exists: it reasons, it returns schemas, and it
	// calls tools. Other Bedrock models do some of that and not the rest, so the default
	// is inferred from the model ID and an operator can override it.
	claude := isClaude(cfg.ModelID)
	if cfg.Tools, err = envBool(EnvTools, claude); err != nil {
		return Config{}, err
	}
	if cfg.Reasoning, err = envBool(EnvReasoning, claude); err != nil {
		return Config{}, err
	}
	if cfg.PromptCache, err = envBool(EnvPromptCache, false); err != nil {
		return Config{}, err
	}
	if cfg.TopP, err = envFloat(EnvTopP, 0); err != nil {
		return Config{}, err
	}
	if cfg.TopP < 0 || cfg.TopP > 1 {
		return Config{}, fmt.Errorf("%w: %s must be in [0, 1], got %.2f", ErrConfig, EnvTopP, cfg.TopP)
	}
	if cfg.Stream, err = envBool(EnvStream, true); err != nil {
		return Config{}, err
	}
	if cfg.InputCostPer1M, err = envFloat(EnvInputCostPer1M, 0); err != nil {
		return Config{}, err
	}
	if cfg.OutputCostPer1M, err = envFloat(EnvOutputCostPer1M, 0); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Capabilities describes this provider to anything that must choose between providers —
// which, from Milestone 10, is a router.
//
// The two fields that matter are the two that differ most sharply from Ollama.
func (c Config) Capabilities() llm.Capabilities {
	return llm.Capabilities{
		// FALSE. The prompt leaves.
		//
		// This is the single most important fact about Bedrock, and it is the one a
		// comparison table usually buries under "latency" and "cost". The platform's
		// prompts are full of somebody's source code, and calling Bedrock sends that
		// source out of the VPC and into a managed AWS service.
		//
		// It is a *good* version of leaving: it stays inside AWS, in your chosen region,
		// and AWS states it is not used to train models. With a bedrock-runtime VPC
		// endpoint it does not even traverse the public internet. But "it leaves and is
		// handled well" is a different claim from "it does not leave", and a router
		// deciding what to do with a private repository needs to be able to tell the
		// difference. So: false.
		Local:            false,
		Streaming:        true,
		MaxContextTokens: c.ContextTokens,

		// Non-zero, for the first time. Ollama's cost is the instance, paid whether or
		// not a token is generated; Bedrock's is per token, paid only when one is. That
		// inversion is the whole basis of cost-aware routing, and it is why these are
		// configuration rather than a constant: a price table in source code is a price
		// table that is quietly wrong.
		CostPer1MInputTokensUSD:  c.InputCostPer1M,
		CostPer1MOutputTokensUSD: c.OutputCostPer1M,

		// --- Milestone 9 -----------------------------------------------------
		//
		// The first time a Capabilities field says what a model can *do* rather than
		// where it runs or what it costs — and the first time Milestone 10's router has
		// a reason to refuse a route rather than merely prefer another one. "Send it to
		// whichever is cheaper" is a safe thing to say right up until one of them cannot
		// do the job, at which point cheaper means confidently wrong.
		Tools: c.Tools,

		// On Bedrock, structured output IS tool use: one tool, forced, whose schema is
		// the object you want back. So it tracks Tools — but it is a separate field,
		// because that is a fact about this provider and not about the world.
		StructuredOutput: c.Tools,

		Reasoning: c.Reasoning,
	}
}

// EstimatedCostUSD prices a completed generation.
//
// It exists so a log line can carry the cost of the inference that produced it. "This
// blog post cost $0.04" is a fact somebody will eventually want, and reconstructing it
// later from token counts and a price list nobody wrote down is miserable.
func (c Config) EstimatedCostUSD(usage llm.Usage) float64 {
	const perMillion = 1_000_000.0
	return float64(usage.PromptTokens)/perMillion*c.InputCostPer1M +
		float64(usage.CompletionTokens)/perMillion*c.OutputCostPer1M
}

// Redacted returns the configuration for logging. There is no secret in it to remove —
// which is itself worth showing, because it is the difference between IAM and an API key.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"region":          c.Region,
		"modelId":         c.ModelID,
		"endpoint":        orNone(c.Endpoint),
		"credentials":     "(AWS IAM — resolved by the SDK's default chain; no static key)",
		"contextTokens":   c.ContextTokens,
		"maxTokens":       c.MaxTokens,
		"temperature":     c.Temperature,
		"stream":          c.Stream,
		"timeout":         c.Timeout.String(),
		"idleTimeout":     c.IdleTimeout.String(),
		"retryAttempts":   c.RetryAttempts,
		"awsSdkRetries":   "(disabled — this integration owns the retry policy)",
		"inputCostPer1M":  c.InputCostPer1M,
		"outputCostPer1M": c.OutputCostPer1M,
		"topP":            orDefault(c.TopP),
		"tools":           c.Tools,
		"reasoning":       c.Reasoning,
		"promptCache":     c.PromptCache,
	}
}

// orDefault shows an unset numeric knob as what it actually is, rather than as 0 — which
// would read as "TopP is zero" when it means "TopP is not in play at all".
func orDefault(f float64) any {
	if f == 0 {
		return "(unset — tune temperature OR topP, not both)"
	}
	return f
}

func orNone(s string) string {
	if s == "" {
		return "(default AWS endpoint)"
	}
	return s
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a duration like 90s or 2m, got %q", ErrConfig, key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w: %s must be positive, got %q", ErrConfig, key, raw)
	}
	return d, nil
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a whole number, got %q", ErrConfig, key, raw)
	}
	return n, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a number, got %q", ErrConfig, key, raw)
	}
	if f < 0 {
		return 0, fmt.Errorf("%w: %s must not be negative, got %q", ErrConfig, key, raw)
	}
	return f, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%w: %s must be true or false, got %q", ErrConfig, key, raw)
	}
	return b, nil
}
