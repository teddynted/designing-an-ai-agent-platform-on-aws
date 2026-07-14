// Package bedrock talks to Amazon Bedrock. It is the second implementation of
// llm.Provider, and the first test of whether Milestone 7's abstraction was real.
//
// # What the abstraction had to absorb
//
// Milestone 7 claimed that adding a provider would be an *implementation*, not a
// rewrite. Mostly that held. What it did not survive untouched was the error
// vocabulary: Ollama has no authentication and does not throttle, so `llm` had no word
// for "your credentials are wrong" or "you are over quota". Bedrock has both, constantly.
// See the note in llm.ErrUnauthorized.
//
// That is the ordinary way an abstraction is wrong — not badly designed, but designed
// from a sample of one — and it is the argument for building the second implementation
// before three call sites have grown around the first.
//
// # Why Converse, and not InvokeModel
//
// Bedrock has two ways in. InvokeModel takes a raw JSON body **in each model family's own
// format**: Anthropic wants `{"messages": …, "anthropic_version": …}`, Meta wants
// `{"prompt": "<s>[INST]…"}`, Amazon wants `{"inputText": …}`. Writing against it means
// this package grows a switch on the model ID, and every new model is a code change —
// which is exactly the provider-specific logic the platform is supposed to be free of.
//
// Converse is AWS's unified messages API: one request shape, one response shape, across
// every model that supports messages. It is the same architectural move as this
// platform's own llm.Provider, made one layer down, and taking it means the model ID is
// **configuration** rather than a branch.
//
// The cost is that Converse is a slightly smaller surface than a raw model body — no
// model-specific exotica — and for a platform that summarises diffs that is not a cost
// at all.
//
// # Authentication: there is no secret here
//
// Bedrock is authenticated with IAM. The SDK resolves temporary credentials through its
// default chain — an EC2 instance profile, a Lambda execution role, an OIDC web identity.
// There is no BEDROCK_API_KEY, no secret in the environment, and nothing to leak into a
// log. It is the single nicest thing about a hosted provider that lives in your own cloud.
package bedrock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// converseAPI is the slice of the Bedrock runtime client this package uses.
//
// It is an interface so the tests can substitute a fake. That matters more here than
// elsewhere: the alternative is tests that need live AWS credentials, a region, and a
// model the account has been granted access to — which is not a unit test, it is an
// integration test wearing one's clothes, and it will be skipped in CI within a month.
type converseAPI interface {
	Converse(context.Context, *bedrockruntime.ConverseInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(context.Context, *bedrockruntime.ConverseStreamInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

// catalogAPI is the control-plane client, which is a *different service* from the
// runtime one: bedrock lists models, bedrock-runtime invokes them. They have separate
// endpoints, separate IAM actions, and separate VPC endpoints.
type catalogAPI interface {
	ListFoundationModels(context.Context, *bedrock.ListFoundationModelsInput, ...func(*bedrock.Options)) (*bedrock.ListFoundationModelsOutput, error)
}

// Client is an llm.Provider backed by Amazon Bedrock.
type Client struct {
	cfg     Config
	runtime converseAPI
	catalog catalogAPI
	log     *slog.Logger

	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// Option customises a Client.
type Option func(*Client)

// WithRuntime replaces the Bedrock runtime client. Tests use it.
func WithRuntime(api converseAPI) Option { return func(c *Client) { c.runtime = api } }

// WithCatalog replaces the control-plane client. Tests use it.
func WithCatalog(api catalogAPI) Option { return func(c *Client) { c.catalog = api } }

// WithSleep replaces the retry backoff sleep, so tests run instantly.
func WithSleep(f func(context.Context, time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// WithJitter replaces the backoff jitter, so tests are deterministic.
func WithJitter(f func(time.Duration) time.Duration) Option {
	return func(c *Client) { c.jitter = f }
}

// New builds a Client, resolving AWS credentials through the SDK's default chain.
func New(ctx context.Context, cfg Config, log *slog.Logger, opts ...Option) (*Client, error) {
	c := &Client{
		cfg:    cfg,
		log:    log,
		sleep:  httpx.SleepCtx,
		jitter: httpx.FullJitter,
	}
	for _, opt := range opts {
		opt(c)
	}

	// A test that injected both clients needs no AWS at all — not even a credential
	// lookup, which would otherwise reach for the metadata service and hang.
	if c.runtime != nil && c.catalog != nil {
		return c, nil
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),

		// THE IMPORTANT LINE.
		//
		// The AWS SDK retries by default — three attempts, with its own backoff. This
		// integration also retries, because every other provider in this platform does
		// and the behaviour has to be the same across them.
		//
		// Leave both on and they MULTIPLY: three of ours over three of the SDK's is nine
		// requests fired at an endpoint that is throttling precisely because it is getting
		// too many requests. Worse, they hide each other — the SDK's retries are invisible
		// to us, so the `attempts` field in our logs would be a confident lie, and the
		// duration would include waits nobody could account for.
		//
		// So the SDK gets exactly one attempt, and this package owns the retry policy.
		awsconfig.WithRetryMaxAttempts(1),
	)
	if err != nil {
		// Almost always a missing region or an unresolvable credential chain, and both are
		// configuration rather than a runtime failure.
		return nil, fmt.Errorf("%w: could not resolve AWS credentials or region: %v", ErrConfig, err)
	}

	runtimeOpts := func(o *bedrockruntime.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	}
	catalogOpts := func(o *bedrock.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	}

	if c.runtime == nil {
		c.runtime = bedrockruntime.NewFromConfig(awsCfg, runtimeOpts)
	}
	if c.catalog == nil {
		c.catalog = bedrock.NewFromConfig(awsCfg, catalogOpts)
	}
	return c, nil
}

// Name implements llm.Provider.
func (c *Client) Name() string { return "bedrock" }

// Capabilities implements llm.Provider.
func (c *Client) Capabilities() llm.Capabilities { return c.cfg.Capabilities() }

// Models implements llm.Provider: the foundation models this account can see.
//
// Note the word "see". Bedrock lists every model it offers in the region, including ones
// this account has never been granted access to — so a model appearing here does NOT mean
// it can be invoked. That distinction is why llm.ErrModelAccessDenied exists, and it is
// the most common way a correct-looking Bedrock call fails on a fresh account.
func (c *Client) Models(ctx context.Context) ([]llm.Model, error) {
	var models []llm.Model

	_, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, _ int) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		out, err := c.catalog.ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{
			ByOutputModality: bedrocktypes.ModelModalityText,
		})
		if err != nil {
			return c.classify(err)
		}

		models = models[:0]
		for _, m := range out.ModelSummaries {
			models = append(models, llm.Model{
				Name:   aws.ToString(m.ModelId),
				Family: aws.ToString(m.ProviderName),
			})
		}
		return nil
	})

	return models, err
}

// Generate implements llm.Provider: one call, the whole completion.
func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	var out llm.Response

	attempts, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, _ int) error {
		ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()

		res, err := c.runtime.Converse(ctx, &bedrockruntime.ConverseInput{
			ModelId:                      aws.String(c.model(req)),
			Messages:                     c.messagesWithTools(req),
			System:                       c.systemBlocks(req),
			InferenceConfig:              c.inference(req),
			ToolConfig:                   toolConfig(req),
			AdditionalModelRequestFields: reasoningFields(req),
		})
		if err != nil {
			return c.classify(err)
		}

		message, ok := res.Output.(*types.ConverseOutputMemberMessage)
		if !ok {
			return fmt.Errorf("%w: Bedrock returned no message", llm.ErrInvalidResponse)
		}

		text, calls, reasoning := contentOf(message.Value.Content)
		out = llm.Response{
			Model:        c.model(req),
			Content:      text,
			ToolCalls:    calls,
			Reasoning:    reasoning,
			Usage:        usageOf(res.Usage, res.Metrics),
			FinishReason: finishReason(res.StopReason),
		}
		// The AWS request ID is the only handle a support case has. It costs one log field
		// and it is the difference between "Bedrock was slow" and a ticket AWS can act on.
		if id, ok := awsmiddleware.GetRequestIDMetadata(res.ResultMetadata); ok {
			c.log.Debug("bedrock request", "awsRequestId", id, "model", c.model(req))
		}
		return nil
	})

	out.Attempts = attempts
	return out, err
}

// Stream implements llm.Provider.
//
// The same two rules as Milestone 7, for the same reasons:
//
//   - a stall — silence, rather than slowness — is detected by an idle timer that is reset
//     on every token, because a total timeout cannot tell a slow model from a hung one;
//   - and once a token has reached the caller, a failure is [llm.ErrStreamBroken] and is
//     NOT retried, because a retry would hand them a second beginning.
//
// The mechanism differs (Bedrock's event stream is a channel, not an NDJSON body) and the
// rules do not. That is the abstraction earning its keep: the *policy* lives in llm and is
// identical for both providers, and only the plumbing is different.
func (c *Client) Stream(ctx context.Context, req llm.Request, sink llm.Sink) (llm.Response, error) {
	var out llm.Response
	var emitted atomic.Bool

	attempts, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, _ int) error {
		if emitted.Load() {
			return fmt.Errorf("%w: refusing to resend a stream that has already produced output",
				llm.ErrStreamBroken)
		}

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		res, err := c.runtime.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
			ModelId:         aws.String(c.model(req)),
			Messages:        c.messages(req),
			System:          c.system(req),
			InferenceConfig: c.inferenceStream(req),
		})
		if err != nil {
			return c.classify(err)
		}

		stream := res.GetStream()
		defer stream.Close()

		var content strings.Builder
		var usage llm.Usage
		reason := "stop"

		events := stream.Events()
		for {
			select {
			case <-ctx.Done():
				return c.streamError(emitted.Load(), fmt.Errorf("%w: %v", context.Canceled, ctx.Err()))

			case <-time.After(c.cfg.IdleTimeout):
				// Silence. Not slowness — a model producing tokens resets this timer every
				// time round the loop, so reaching here means nothing arrived at all.
				return c.streamError(emitted.Load(),
					fmt.Errorf("%w: no token for %s", llm.ErrStalled, c.cfg.IdleTimeout))

			case event, open := <-events:
				if !open {
					// The channel closed. Err() is where a modeled exception mid-stream lands —
					// a throttle that hit us after the first token, say.
					if err := stream.Err(); err != nil {
						return c.streamError(emitted.Load(), c.classify(err))
					}
					out = llm.Response{
						Model:        c.model(req),
						Content:      content.String(),
						Usage:        usage,
						FinishReason: reason,
					}
					return nil
				}

				switch e := event.(type) {
				case *types.ConverseStreamOutputMemberContentBlockDelta:
					delta, ok := e.Value.Delta.(*types.ContentBlockDeltaMemberText)
					if !ok || delta.Value == "" {
						continue
					}
					content.WriteString(delta.Value)
					emitted.Store(true)
					if err := sink(llm.Chunk{Content: delta.Value}); err != nil {
						// The caller stopped it. Not our failure, and not to be retried.
						return fmt.Errorf("%w: %v", context.Canceled, err)
					}

				case *types.ConverseStreamOutputMemberMessageStop:
					reason = finishReason(e.Value.StopReason)
					_ = sink(llm.Chunk{Done: true})

				case *types.ConverseStreamOutputMemberMetadata:
					// The counters arrive at the END of a stream, not with the tokens. So the
					// cost of a generation is only knowable once it is over — which is fine,
					// and worth knowing when you go looking for them mid-stream.
					usage = usageOf(e.Value.Usage, nil)
				}
			}
		}
	})

	out.Attempts = attempts
	return out, err
}

// streamError makes a failure terminal once output has escaped, wrapping BOTH the
// consequence (do not retry) and the cause (what actually went wrong), so a log can
// report the second and a retry policy can obey the first.
func (c *Client) streamError(emitted bool, err error) error {
	if !emitted {
		return err
	}
	return fmt.Errorf("%w: %w", llm.ErrStreamBroken, err)
}

// --- request building -------------------------------------------------------

func (c *Client) model(req llm.Request) string {
	if req.Model != "" {
		return req.Model
	}
	return c.cfg.ModelID
}

// messages maps the platform's request onto Converse's message list. Both the
// single-shot Prompt form and the Messages form collapse to the same thing here, which
// is the whole benefit of Converse.
func (c *Client) messages(req llm.Request) []types.Message {
	if len(req.Messages) > 0 {
		out := make([]types.Message, 0, len(req.Messages))
		for _, m := range req.Messages {
			role := types.ConversationRoleUser
			if m.Role == llm.RoleAssistant {
				role = types.ConversationRoleAssistant
			}
			out = append(out, types.Message{
				Role:    role,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Content}},
			})
		}
		return out
	}
	return []types.Message{{
		Role:    types.ConversationRoleUser,
		Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: req.Prompt}},
	}}
}

// system maps the system prompt. Converse carries it as a separate field rather than as
// a message with a "system" role — which is a real difference from Ollama's chat API, and
// exactly the sort of thing the provider is here to absorb so that no caller ever learns
// about it.
func (c *Client) system(req llm.Request) []types.SystemContentBlock {
	if strings.TrimSpace(req.System) == "" {
		return nil
	}
	return []types.SystemContentBlock{
		&types.SystemContentBlockMemberText{Value: req.System},
	}
}

func (c *Client) inference(req llm.Request) *types.InferenceConfiguration {
	maxTokens := req.Options.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.cfg.MaxTokens
	}
	temperature := req.Options.Temperature
	if temperature == 0 {
		temperature = c.cfg.Temperature
	}

	cfg := &types.InferenceConfiguration{
		MaxTokens:     aws.Int32(int32(maxTokens)),
		Temperature:   aws.Float32(float32(temperature)),
		StopSequences: req.Options.Stop,
	}

	// TopP is only sent when it is asked for. Sending both TopP and Temperature to Claude
	// is not an error and it is not a good idea — they are two knobs on the same
	// distribution, and Anthropic's guidance is to tune one. A default that quietly sent
	// both would mean nobody in this platform was ever tuning either.
	topP := req.Options.TopP
	if topP == 0 {
		topP = c.cfg.TopP
	}
	if topP > 0 {
		cfg.TopP = aws.Float32(float32(topP))
	}
	return cfg
}

// inferenceStream is the same configuration; the streaming API takes an identical type.
func (c *Client) inferenceStream(req llm.Request) *types.InferenceConfiguration {
	return c.inference(req)
}

// --- response mapping -------------------------------------------------------

func textOf(blocks []types.ContentBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if text, ok := block.(*types.ContentBlockMemberText); ok {
			b.WriteString(text.Value)
		}
	}
	return b.String()
}

func usageOf(u *types.TokenUsage, metrics *types.ConverseMetrics) llm.Usage {
	usage := llm.Usage{}
	if u != nil {
		usage.PromptTokens = int(aws.ToInt32(u.InputTokens))
		usage.CompletionTokens = int(aws.ToInt32(u.OutputTokens))
	}
	if metrics != nil && metrics.LatencyMs != nil {
		latency := time.Duration(aws.ToInt64(metrics.LatencyMs)) * time.Millisecond
		usage.EvalDuration = latency
		if latency > 0 && usage.CompletionTokens > 0 {
			usage.TokensPerSecond = float64(usage.CompletionTokens) / latency.Seconds()
		}
	}
	// Note what is absent: LoadDuration. Bedrock has no model-load cost to report,
	// because the model is always loaded — that is what you are paying for. It is the
	// clearest single illustration of managed-versus-self-hosted there is.
	return usage
}

// finishReason maps Bedrock's stop reasons onto the platform's.
//
// "max_tokens" becomes "length", which the Service logs as a WARNING — the answer was cut
// off, and a truncated blog post looks a great deal like a finished one.
func finishReason(reason types.StopReason) string {
	switch reason {
	case types.StopReasonToolUse:
		// Not an ending at all: the model stopped to ASK for something. A caller that
		// treats this as an answer gets an empty string and no explanation of why.
		return "tool_use"
	case types.StopReasonMaxTokens:
		return "length"
	case types.StopReasonStopSequence:
		return "stop-sequence"
	case types.StopReasonContentFiltered:
		// The model refused. It is a *result*, not a failure of the plumbing, and the
		// caller needs to know the difference.
		return "content-filtered"
	default:
		return "stop"
	}
}

// --- errors -----------------------------------------------------------------

// classify turns an AWS error into one of llm's, so that nothing above this package ever
// sees an AWS type.
//
// This is where "avoid leaking AWS implementation details" is actually enforced. A caller
// gets llm.ErrThrottled, not *types.ThrottlingException, and can therefore be written
// once for every provider.
func (c *Client) classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: Bedrock did not answer within %s", llm.ErrTimeout, c.cfg.Timeout)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %v", context.Canceled, err)
	}

	requestID := requestIDOf(err)

	var throttling *types.ThrottlingException
	var quota *types.ServiceQuotaExceededException
	if errors.As(err, &throttling) || errors.As(err, &quota) {
		// The defining failure of a hosted provider under load, and NOT an outage:
		// Bedrock is fine, and we are over quota. Retryable, with backoff and jitter —
		// which is the one thing that actually helps.
		return fmt.Errorf("%w: Bedrock is rate-limiting this account for %s%s",
			llm.ErrThrottled, c.cfg.ModelID, requestID)
	}

	var denied *types.AccessDeniedException
	if errors.As(err, &denied) {
		// Two very different problems arrive as AccessDeniedException, and sending someone
		// to look at the wrong one costs an afternoon.
		return fmt.Errorf("%w: %s. Either the IAM role lacks bedrock:InvokeModel for this "+
			"model, or the ACCOUNT has not been granted access to it — request it under "+
			"Bedrock → Model access in the console. Model: %s%s",
			llm.ErrModelAccessDenied, message(err), c.cfg.ModelID, requestID)
	}

	var notFound *types.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return fmt.Errorf("%w: %q does not exist in %s%s",
			llm.ErrModelNotFound, c.cfg.ModelID, c.cfg.Region, requestID)
	}

	var validation *types.ValidationException
	if errors.As(err, &validation) {
		msg := strings.ToLower(message(err))
		switch {
		case strings.Contains(msg, "too long"), strings.Contains(msg, "too many tokens"),
			strings.Contains(msg, "exceeds"), strings.Contains(msg, "context"):
			// Bedrock, unlike Ollama, REFUSES an oversized prompt rather than silently
			// truncating it. That is a genuine mercy — the platform's own check exists
			// precisely because the other provider does not do this.
			return fmt.Errorf("%w: Bedrock rejected the prompt as too large for %s%s",
				llm.ErrContextExceeded, c.cfg.ModelID, requestID)
		case strings.Contains(msg, "on-demand throughput isn"), strings.Contains(msg, "inference profile"):
			// The footgun: newer models are only available on-demand through a cross-region
			// inference profile, and the bare model ID gets you a validation error that does
			// not explain itself.
			return fmt.Errorf("%w: %q needs a cross-region INFERENCE PROFILE, not a bare model "+
				"ID — try the regional prefix, e.g. \"us.%s\"%s",
				llm.ErrInvalidRequest, c.cfg.ModelID, c.cfg.ModelID, requestID)
		default:
			return fmt.Errorf("%w: Bedrock rejected the request: %s%s",
				llm.ErrInvalidRequest, message(err), requestID)
		}
	}

	var modelTimeout *types.ModelTimeoutException
	if errors.As(err, &modelTimeout) {
		return fmt.Errorf("%w: the model did not respond in time%s", llm.ErrTimeout, requestID)
	}

	var notReady *types.ModelNotReadyException
	var unavailable *types.ServiceUnavailableException
	var internal *types.InternalServerException
	if errors.As(err, &notReady) || errors.As(err, &unavailable) || errors.As(err, &internal) {
		return fmt.Errorf("%w: Bedrock answered %s%s", llm.ErrUnavailable, apiCode(err), requestID)
	}

	var modelErr *types.ModelErrorException
	if errors.As(err, &modelErr) {
		return fmt.Errorf("%w: the model failed: %s%s", llm.ErrInvalidResponse, message(err), requestID)
	}

	var streamErr *types.ModelStreamErrorException
	if errors.As(err, &streamErr) {
		return fmt.Errorf("%w: the stream failed: %s%s", llm.ErrInvalidResponse, message(err), requestID)
	}

	// Credentials that are missing, expired, or refused by STS never become a Bedrock
	// exception at all — they fail before the call is signed.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if strings.Contains(code, "Unrecognized") || strings.Contains(code, "InvalidSignature") ||
			strings.Contains(code, "ExpiredToken") || strings.Contains(code, "Incomplete") {
			return fmt.Errorf("%w: AWS rejected our credentials (%s)%s", llm.ErrUnauthorized, code, requestID)
		}
		return fmt.Errorf("%w: Bedrock answered %s: %s%s", llm.ErrUnavailable, code, apiErr.ErrorMessage(), requestID)
	}

	return fmt.Errorf("%w: %v", llm.ErrUnavailable, err)
}

// requestIDOf extracts the AWS request ID, which is the only handle an AWS support case
// has. It is formatted for appending to an error message, and is empty when there is none
// (a failure that never reached AWS has no ID, and saying "request: " with nothing after
// it is worse than saying nothing).
func requestIDOf(err error) string {
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.ServiceRequestID() != "" {
		return " (aws request id: " + re.ServiceRequestID() + ")"
	}
	return ""
}

func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return "an error"
}

// message extracts an AWS error's human-readable text.
//
// It deliberately does NOT include the raw error, which can be several hundred characters
// of SDK operation trace — useful in a debugger and pure noise in a log line that a human
// is trying to read at 3am.
func message(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorMessage()
	}
	return err.Error()
}

// policy is the retry policy.
func (c *Client) policy() httpx.Policy {
	return httpx.Policy{
		Attempts:  c.cfg.RetryAttempts,
		Delay:     c.cfg.RetryDelay,
		Retryable: retryable,
		Sleep:     c.sleep,
		Jitter:    c.jitter,
	}
}

// retryable.
//
// Throttling is the interesting one, and it is the reason a hosted provider needs a retry
// policy at all: it is *expected*, it is not an outage, and backing off with jitter is
// precisely the correct response to it. Under load this is the failure you get, and a
// platform that treats it as fatal is a platform that falls over the moment it is busy.
//
// And, as everywhere in this platform: never a stream that has already emitted.
func retryable(err error) bool {
	switch {
	case errors.Is(err, llm.ErrStreamBroken):
		return false
	case errors.Is(err, llm.ErrThrottled):
		return true
	case errors.Is(err, llm.ErrUnavailable), errors.Is(err, llm.ErrTimeout), errors.Is(err, llm.ErrStalled):
		return true
	default:
		// Not retried: ErrUnauthorized (a policy will not change because you asked twice),
		// ErrModelAccessDenied (an entitlement will not appear), ErrModelNotFound,
		// ErrContextExceeded (the prompt is the same size on the second attempt).
		return false
	}
}
