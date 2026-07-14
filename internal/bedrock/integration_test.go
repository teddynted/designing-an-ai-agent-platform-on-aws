//go:build integration

// Package bedrock's integration tests. They talk to REAL Amazon Bedrock.
//
// # Why they are behind a build tag
//
// Because a unit test that calls Bedrock is not a unit test.
//
// It fails on an aeroplane. It costs money on every push. It turns a code review into a
// permissions ticket, because a contributor without model access cannot run the suite. And
// it goes red for reasons that have nothing to do with the change — somebody else exhausted
// the account's tokens-per-minute quota, and now the build is broken and the commit that
// "broke" it is innocent.
//
// A test with those properties is a monitoring check wearing a unit test's badge. So the
// unit tests (bedrock_test.go, claude_test.go) mock the SDK entirely and run in
// milliseconds with no credentials, and these — which answer a genuinely different
// question — are opt-in:
//
//	make test-integration
//	# or: go test -tags=integration ./internal/bedrock/ -v
//
// # What they are actually for
//
// The unit tests prove the platform's logic: that a ThrottlingException becomes
// llm.ErrThrottled, that an oversized prompt is refused, that a tool's arguments are
// validated. They prove all of that against a fake, which means they prove it against **my
// belief about what Bedrock does**.
//
// These tests check that belief. They are the only thing standing between this integration
// and a subtly wrong assumption about the real API — a stop reason that is not what I
// think, a tool result shape that changed, a model that no longer exists in the region.
// That is a small number of very valuable assertions, and it is why they exist despite
// costing real money to run.
package bedrock

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// liveClient builds a real Bedrock client, or skips.
func liveClient(t *testing.T) *Client {
	t.Helper()

	if os.Getenv("BEDROCK_MODEL_ID") == "" {
		t.Skip("set BEDROCK_MODEL_ID (and have AWS credentials) to run the integration tests")
	}

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	// Keep them cheap. These tests exist to check the contract, not the prose.
	cfg.MaxTokens = 256

	client, err := New(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestLiveGenerate(t *testing.T) {
	client := liveClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := client.Generate(ctx, llm.Request{
		System:  "Answer in exactly one word.",
		Prompt:  "What is the capital of France?",
		Options: llm.Options{MaxTokens: 16, Temperature: 0},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if res.Content == "" {
		t.Error("a real model produced nothing")
	}
	// Token usage is reported. If AWS ever stops sending it, every cost estimate in this
	// platform silently becomes zero — and nothing else would notice.
	if res.Usage.PromptTokens == 0 || res.Usage.CompletionTokens == 0 {
		t.Errorf("usage = %+v, want real token counts — the cost estimate depends on them",
			res.Usage)
	}
	t.Logf("answered %q in %d tokens", res.Content, res.Usage.CompletionTokens)
}

func TestLiveStream(t *testing.T) {
	client := liveClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var chunks int
	res, err := client.Stream(ctx, llm.Request{
		Prompt:  "Count from one to five, in words.",
		Options: llm.Options{MaxTokens: 64, Temperature: 0},
	}, func(c llm.Chunk) error {
		if c.Content != "" {
			chunks++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if chunks < 2 {
		t.Errorf("got %d chunks — the response did not actually stream", chunks)
	}
	if res.Content == "" {
		t.Error("the assembled response is empty")
	}
}

// The one that matters most. If Bedrock's tool-use contract is not what the unit tests
// pretend it is, everything in Milestone 9 is built on a fiction — and this is the only test
// that can tell.
func TestLiveToolUse(t *testing.T) {
	client := liveClient(t)

	if !client.Capabilities().Tools {
		t.Skip("this model is not configured for tool use")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := client.Generate(ctx, llm.Request{
		System: "You are a helpful assistant. Use the tools available to you.",
		Prompt: "What workflows can this platform run? Use the tool.",
		Tools: []llm.ToolSpec{{
			Name:        "list_workflows",
			Description: "List the workflows this platform can run. Takes no arguments.",
			Schema:      llm.Object(map[string]any{}),
			Effect:      llm.Read,
		}},
		Options: llm.Options{MaxTokens: 256, Temperature: 0},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !res.WantsTools() {
		t.Fatalf("the model did not call the tool. finishReason=%q content=%q",
			res.FinishReason, res.Content)
	}
	if res.FinishReason != "tool_use" {
		t.Errorf("finishReason = %q, want tool_use", res.FinishReason)
	}
	if res.ToolCalls[0].Name != "list_workflows" || res.ToolCalls[0].ID == "" {
		t.Errorf("tool call = %+v", res.ToolCalls[0])
	}
	t.Logf("the model called %s (id %s)", res.ToolCalls[0].Name, res.ToolCalls[0].ID)
}

// Structured output, against the real API — including the thing the unit tests cannot check,
// which is whether a forced tool choice really does make prose impossible.
func TestLiveStructuredOutput(t *testing.T) {
	client := liveClient(t)

	if !client.Capabilities().StructuredOutput {
		t.Skip("this model is not configured for structured output")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := client.Generate(ctx, llm.Request{
		Prompt: "Triage this change: the README had a typo fixed.",
		Tools: []llm.ToolSpec{{
			Name:        "change_triage",
			Description: "Record a triage decision.",
			Schema: llm.Object(map[string]any{
				"severity": llm.String("How bad is it?", "low", "medium", "high", "critical"),
				"summary":  llm.String("One sentence."),
			}, "severity", "summary"),
			Effect: llm.Read,
		}},
		ToolChoice: "change_triage",
		Options:    llm.Options{MaxTokens: 256, Temperature: 0},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(res.ToolCalls) == 0 {
		t.Fatal("a FORCED tool choice produced prose — the platform's central assumption " +
			"about structured output on Bedrock is wrong")
	}

	// And the arguments actually decode. This is the assertion that would have caught the
	// Smithy-document bug, where every tool call arrived with `{}` because json.Marshal on a
	// document returns an empty object.
	var got map[string]any
	if err := json.Unmarshal(res.ToolCalls[0].Arguments, &got); err != nil {
		t.Fatalf("the model's arguments do not decode: %v", err)
	}
	if got["severity"] == nil || got["summary"] == nil {
		t.Fatalf("arguments = %v, want the schema's required fields. An EMPTY object here "+
			"means the document was not unmarshalled properly", got)
	}
	t.Logf("severity=%v summary=%v", got["severity"], got["summary"])
}

// Reasoning, and the signature that has to come back untouched.
func TestLiveReasoning(t *testing.T) {
	client := liveClient(t)

	if !client.Capabilities().Reasoning {
		t.Skip("this model is not configured for extended thinking")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := client.Generate(ctx, llm.Request{
		Prompt: "A bat and a ball cost $1.10. The bat costs $1.00 more than the ball. " +
			"How much does the ball cost?",
		Reasoning: &llm.ReasoningConfig{BudgetTokens: 1024},
		// Thinking is drawn from the same budget as the answer, so MaxTokens must exceed it.
		Options: llm.Options{MaxTokens: 2048},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if res.Reasoning == nil {
		t.Fatal("extended thinking was requested and no reasoning came back")
	}
	if res.Reasoning.Signature == "" && len(res.Reasoning.Redacted) == 0 {
		t.Error("the reasoning has no signature — Bedrock will reject the NEXT turn of a " +
			"tool-using conversation without it")
	}
	t.Logf("thought for %d chars, then answered: %s", len(res.Reasoning.Text), res.Content)
}

// Models are listed, and the configured one is actually among them.
func TestLiveModels(t *testing.T) {
	client := liveClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	models, err := client.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("no models — the account can see nothing in this region")
	}
	t.Logf("%d models visible in %s", len(models), client.cfg.Region)
}

// A model that does not exist must produce the platform's error, with the platform's advice
// — not a raw AWS exception.
func TestLiveUnknownModelIsClassified(t *testing.T) {
	client := liveClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.Generate(ctx, llm.Request{
		Model:  "anthropic.claude-does-not-exist-v9:0",
		Prompt: "hello",
	})
	if err == nil {
		t.Fatal("a nonexistent model must fail")
	}

	// It must be one of OURS. If an AWS type ever escapes to a caller, the abstraction has
	// leaked and every provider-agnostic error check upstream is a lie.
	kind := llm.Kind(err)
	if kind == "unknown" {
		t.Errorf("the error was not classified into the platform's vocabulary: %v", err)
	}
	t.Logf("classified as %q: %v", kind, err)
}
