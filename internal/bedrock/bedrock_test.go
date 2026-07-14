package bedrock

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// fakeRuntime is Bedrock, without AWS.
//
// No credentials, no region, no network, no model access grant. That is the point: a unit
// test that needs live AWS is an integration test wearing one's clothes, and it will be
// skipped in CI within a month.
type fakeRuntime struct {
	out    *bedrockruntime.ConverseOutput
	err    error
	errs   []error // one per attempt, for retry tests
	calls  int32
	gotIn  *bedrockruntime.ConverseInput
	gotStr *bedrockruntime.ConverseStreamInput
}

func (f *fakeRuntime) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	n := int(atomic.AddInt32(&f.calls, 1))
	f.gotIn = in
	if len(f.errs) > 0 {
		if n <= len(f.errs) && f.errs[n-1] != nil {
			return nil, f.errs[n-1]
		}
	} else if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

func (f *fakeRuntime) ConverseStream(_ context.Context, in *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	atomic.AddInt32(&f.calls, 1)
	f.gotStr = in
	if f.err != nil {
		return nil, f.err
	}
	// A real event stream cannot be constructed by hand from outside the SDK, so the
	// streaming tests that need one live in the llm package against a fake PROVIDER —
	// which is the abstraction paying for itself. What is tested HERE is everything that
	// happens before the first byte: the request shape, and the error classification.
	return nil, errors.New("streaming is exercised through the llm.Provider fake")
}

type fakeCatalog struct {
	out *bedrock.ListFoundationModelsOutput
	err error
}

func (f *fakeCatalog) ListFoundationModels(context.Context, *bedrock.ListFoundationModelsInput, ...func(*bedrock.Options)) (*bedrock.ListFoundationModelsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testConfig() Config {
	return Config{
		Region:          "us-east-1",
		ModelID:         "anthropic.claude-3-5-haiku-20241022-v1:0",
		ContextTokens:   200_000,
		MaxTokens:       512,
		Temperature:     0.2,
		Stream:          true,
		Timeout:         2 * time.Second,
		IdleTimeout:     200 * time.Millisecond,
		RetryAttempts:   3,
		RetryDelay:      time.Millisecond,
		InputCostPer1M:  0.80,
		OutputCostPer1M: 4.00,
	}
}

func newClient(t *testing.T, rt converseAPI, cat catalogAPI) *Client {
	t.Helper()
	c, err := New(context.Background(), testConfig(), discardLogger(),
		WithRuntime(rt), WithCatalog(cat),
		WithSleep(func(context.Context, time.Duration) error { return nil }),
		WithJitter(httpx.NoJitter),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func testRequest() llm.Request {
	return llm.Request{
		System:        "You are a technical writer.",
		Prompt:        "Summarise this diff.",
		Purpose:       "diff-summary",
		CorrelationID: "push:delivery-abc",
	}
}

func okOutput(text string) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{
			Value: types.Message{
				Role:    types.ConversationRoleAssistant,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: text}},
			},
		},
		StopReason: types.StopReasonEndTurn,
		Usage: &types.TokenUsage{
			InputTokens:  aws.Int32(120),
			OutputTokens: aws.Int32(30),
			TotalTokens:  aws.Int32(150),
		},
		Metrics: &types.ConverseMetrics{LatencyMs: aws.Int64(1500)},
	}
}

// --- the happy path ---------------------------------------------------------

func TestGenerate(t *testing.T) {
	rt := &fakeRuntime{out: okOutput("Spot instances cut the bill by 70%.")}
	c := newClient(t, rt, &fakeCatalog{})

	res, err := c.Generate(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if res.Content != "Spot instances cut the bill by 70%." {
		t.Errorf("Content = %q", res.Content)
	}
	if res.Usage.PromptTokens != 120 || res.Usage.CompletionTokens != 30 {
		t.Errorf("usage = %+v, want the token counts Bedrock reported", res.Usage)
	}
	// 30 tokens in 1.5s = 20/sec. The same field, computed the same way, as Ollama's —
	// which is what lets one dashboard span both providers.
	if res.Usage.TokensPerSecond != 20 {
		t.Errorf("TokensPerSecond = %v, want 20", res.Usage.TokensPerSecond)
	}
	// Bedrock has no model-load cost, because the model is always loaded. That absence is
	// the clearest single illustration of managed-versus-self-hosted there is.
	if res.Usage.LoadDuration != 0 {
		t.Errorf("LoadDuration = %v, want 0 — a managed model is always loaded", res.Usage.LoadDuration)
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
}

// The system prompt is a SEPARATE field in Converse, not a message with a "system" role
// as it is in Ollama's chat API. Getting this wrong silently drops it — the model simply
// never receives its instructions, and answers something reasonable to the wrong question.
func TestTheSystemPromptGoesInTheSystemField(t *testing.T) {
	rt := &fakeRuntime{out: okOutput("ok")}
	c := newClient(t, rt, &fakeCatalog{})

	if _, err := c.Generate(context.Background(), testRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(rt.gotIn.System) != 1 {
		t.Fatalf("System = %+v, want the system prompt carried in its own field", rt.gotIn.System)
	}
	block, ok := rt.gotIn.System[0].(*types.SystemContentBlockMemberText)
	if !ok || block.Value != "You are a technical writer." {
		t.Errorf("system block = %+v, want the system prompt", rt.gotIn.System[0])
	}
	// ...and it must NOT have been smuggled into the messages, where Bedrock would treat
	// it as something the user said.
	for _, m := range rt.gotIn.Messages {
		if m.Role != types.ConversationRoleUser && m.Role != types.ConversationRoleAssistant {
			t.Errorf("message role = %q — Converse has no system role", m.Role)
		}
	}
}

func TestTheConfiguredBudgetIsSent(t *testing.T) {
	rt := &fakeRuntime{out: okOutput("ok")}
	c := newClient(t, rt, &fakeCatalog{})

	if _, err := c.Generate(context.Background(), testRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Unbounded generation on a per-token provider is a model free to spend your money.
	if got := aws.ToInt32(rt.gotIn.InferenceConfig.MaxTokens); got != 512 {
		t.Errorf("MaxTokens = %d, want the configured budget (512)", got)
	}
	if got := aws.ToFloat32(rt.gotIn.InferenceConfig.Temperature); got != 0.2 {
		t.Errorf("Temperature = %v, want 0.2", got)
	}
}

// --- errors: the whole point of a provider ----------------------------------

// Nothing above this package may ever see an AWS type. A caller written against
// llm.ErrThrottled works for every provider; one written against
// *types.ThrottlingException works for exactly one.
func TestAWSErrorsBecomePlatformErrors(t *testing.T) {
	tests := []struct {
		name    string
		awsErr  error
		wantErr error
		// and the message must be actionable, not just correct
		wantIn string
	}{
		{
			"throttling is not an outage",
			&types.ThrottlingException{Message: aws.String("Too many requests")},
			llm.ErrThrottled, "rate-limiting",
		},
		{
			"a service quota is throttling too",
			&types.ServiceQuotaExceededException{Message: aws.String("quota exceeded")},
			llm.ErrThrottled, "rate-limiting",
		},
		{
			"access denied tells you BOTH things it could be",
			&types.AccessDeniedException{Message: aws.String("not authorized")},
			llm.ErrModelAccessDenied, "Model access",
		},
		{
			"an unknown model",
			&types.ResourceNotFoundException{Message: aws.String("no such model")},
			llm.ErrModelNotFound, "does not exist",
		},
		{
			"a prompt Bedrock refuses as too large",
			&types.ValidationException{Message: aws.String("Input is too long for requested model")},
			llm.ErrContextExceeded, "too large",
		},
		{
			"the inference-profile footgun explains itself",
			&types.ValidationException{Message: aws.String("Invocation of model ID X with on-demand throughput isn't supported")},
			llm.ErrInvalidRequest, "INFERENCE PROFILE",
		},
		{
			"the model timed out",
			&types.ModelTimeoutException{Message: aws.String("timeout")},
			llm.ErrTimeout, "",
		},
		{
			"the service is down",
			&types.ServiceUnavailableException{Message: aws.String("unavailable")},
			llm.ErrUnavailable, "",
		},
		{
			"the model itself failed",
			&types.ModelErrorException{Message: aws.String("model exploded")},
			llm.ErrInvalidResponse, "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &fakeRuntime{err: tt.awsErr}
			cfg := testConfig()
			cfg.RetryAttempts = 1
			c, err := New(context.Background(), cfg, discardLogger(), WithRuntime(rt), WithCatalog(&fakeCatalog{}),
				WithSleep(func(context.Context, time.Duration) error { return nil }), WithJitter(httpx.NoJitter))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = c.Generate(context.Background(), testRequest())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantIn != "" && !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q should contain %q — an error that is correct but not actionable "+
					"costs somebody an afternoon", err, tt.wantIn)
			}
		})
	}
}

// Throttling is the defining failure of a hosted provider under load, and it is NOT an
// outage — Bedrock is fine; we are over quota. It must be retried with backoff, which is
// the one response that actually helps.
func TestThrottlingIsRetried(t *testing.T) {
	rt := &fakeRuntime{
		errs: []error{
			&types.ThrottlingException{Message: aws.String("slow down")},
			&types.ThrottlingException{Message: aws.String("slow down")},
			nil, // the third attempt succeeds
		},
		out: okOutput("Recovered."),
	}
	c := newClient(t, rt, &fakeCatalog{})

	res, err := c.Generate(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("throttling must be absorbed, got: %v", err)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
	if atomic.LoadInt32(&rt.calls) != 3 {
		t.Errorf("called %d times, want 3", rt.calls)
	}
}

// The mirror image. These will not be fixed by asking again, and retrying an auth failure
// repeatedly is how an account gets flagged.
func TestWhatIsNotRetried(t *testing.T) {
	tests := []struct {
		name   string
		awsErr error
	}{
		{"access denied", &types.AccessDeniedException{Message: aws.String("no")}},
		{"unknown model", &types.ResourceNotFoundException{Message: aws.String("no")}},
		{"an oversized prompt", &types.ValidationException{Message: aws.String("Input is too long")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &fakeRuntime{err: tt.awsErr}
			c := newClient(t, rt, &fakeCatalog{}) // 3 attempts configured

			if _, err := c.Generate(context.Background(), testRequest()); err == nil {
				t.Fatal("want an error")
			}
			if n := atomic.LoadInt32(&rt.calls); n != 1 {
				t.Errorf("called %d times — this cannot be fixed by asking again", n)
			}
		})
	}
}

// --- models -----------------------------------------------------------------

func TestModels(t *testing.T) {
	cat := &fakeCatalog{out: &bedrock.ListFoundationModelsOutput{
		ModelSummaries: []bedrocktypes.FoundationModelSummary{
			{ModelId: aws.String("anthropic.claude-3-5-haiku-20241022-v1:0"), ProviderName: aws.String("Anthropic")},
			{ModelId: aws.String("meta.llama3-1-8b-instruct-v1:0"), ProviderName: aws.String("Meta")},
		},
	}}
	c := newClient(t, &fakeRuntime{}, cat)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].Family != "Anthropic" {
		t.Errorf("models = %+v, want both, with their providers", models)
	}
}

// --- capabilities: what a router will route on ------------------------------

// The two fields that differ most sharply from Ollama, and the reason Milestone 10 can
// exist at all.
func TestCapabilities(t *testing.T) {
	c := newClient(t, &fakeRuntime{}, &fakeCatalog{})
	caps := c.Capabilities()

	// FALSE. The prompt leaves the VPC. It stays inside AWS, in-region, and is not used
	// for training — but "leaves and is handled well" is a different claim from "does not
	// leave", and a router deciding what to do with a private repository must be able to
	// tell them apart.
	if caps.Local {
		t.Error("Bedrock is NOT local — the prompt leaves, and a router must be able to see that")
	}
	// Non-zero for the first time in this platform. Ollama's cost is the instance, paid
	// whether or not a token is generated; Bedrock's is per token. That inversion is the
	// entire basis of cost-aware routing.
	if caps.CostPer1MInputTokensUSD == 0 || caps.CostPer1MOutputTokensUSD == 0 {
		t.Error("a hosted provider has a per-token price, and a router needs to know it")
	}
	if !caps.Streaming {
		t.Error("Converse streams")
	}
}

func TestEstimatedCost(t *testing.T) {
	cfg := testConfig() // $0.80 in, $4.00 out, per million

	// 1M input + 1M output = 0.80 + 4.00
	got := cfg.EstimatedCostUSD(llm.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
	if got != 4.80 {
		t.Errorf("EstimatedCostUSD = %v, want 4.80", got)
	}
	// A realistic blog-post generation: a large prompt and a modest completion.
	real := cfg.EstimatedCostUSD(llm.Usage{PromptTokens: 20_000, CompletionTokens: 1_500})
	if real <= 0 || real > 0.1 {
		t.Errorf("a realistic generation cost %v, which does not look right", real)
	}
}

// --- the provider contract --------------------------------------------------

func TestClientImplementsProvider(t *testing.T) {
	var _ llm.Provider = (*Client)(nil)
}

// The credentials are AWS's problem, and there is no secret in the configuration to leak.
// That is the difference between IAM and an API key, and it is worth asserting rather than
// assuming.
func TestThereIsNoSecretInTheConfiguration(t *testing.T) {
	redacted := testConfig().Redacted()

	creds, ok := redacted["credentials"].(string)
	if !ok || !strings.Contains(creds, "IAM") {
		t.Errorf("credentials = %v, want it to say IAM resolves them", redacted["credentials"])
	}
	for key := range redacted {
		if strings.Contains(strings.ToLower(key), "key") || strings.Contains(strings.ToLower(key), "secret") {
			t.Errorf("the configuration has a %q field — Bedrock uses IAM; there must be no static credential", key)
		}
	}
}
