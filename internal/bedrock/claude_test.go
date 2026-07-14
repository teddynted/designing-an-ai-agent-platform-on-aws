package bedrock

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brdocument "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// textOut is Bedrock answering in prose.
func textOut(text string) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		StopReason: types.StopReasonEndTurn,
		Output: &types.ConverseOutputMemberMessage{
			Value: types.Message{
				Role:    types.ConversationRoleAssistant,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: text}},
			},
		},
		Usage: &types.TokenUsage{InputTokens: ptr(int32(10)), OutputTokens: ptr(int32(5))},
	}
}

// claudeClient is a client whose fake runtime records what it was sent.
func claudeClient(t *testing.T, out *bedrockruntime.ConverseOutput) (*Client, *fakeRuntime) {
	t.Helper()
	rt := &fakeRuntime{out: out}
	return newClient(t, rt, &fakeCatalog{}), rt
}

// toolSpec is the sort of tool the platform actually registers.
func toolSpec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "run_workflow",
		Description: "Trigger a workflow. THIS CHANGES THINGS.",
		Schema: llm.Object(map[string]any{
			"workflow": llm.String("which one", "blog-generator"),
		}, "workflow"),
		Effect: llm.Write,
	}
}

func TestIsClaude(t *testing.T) {
	tests := map[string]bool{
		"anthropic.claude-3-5-haiku-20241022-v1:0":     true,
		"us.anthropic.claude-sonnet-4-20250514-v1:0":   true, // a cross-region inference profile
		"eu.anthropic.claude-3-5-sonnet-20240620-v1:0": true,
		"meta.llama3-70b-instruct-v1:0":                false,
		"amazon.nova-lite-v1:0":                        false,
	}
	for model, want := range tests {
		if got := isClaude(model); got != want {
			t.Errorf("isClaude(%q) = %v, want %v", model, got, want)
		}
	}
}

// Claude gets the capabilities that make Milestone 9 possible; a Llama on the same
// provider does not, and the platform will refuse rather than pretend.
func TestCapabilitiesAreInferredFromTheModel(t *testing.T) {
	setEnv(t, map[string]string{EnvModelID: "us.anthropic.claude-sonnet-4-20250514-v1:0"})
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	caps := cfg.Capabilities()
	if !caps.Tools || !caps.StructuredOutput || !caps.Reasoning {
		t.Errorf("Claude should be able to do all three: %+v", caps)
	}
	// And it still must not claim to be local. The prompt still leaves.
	if caps.Local {
		t.Error("Bedrock is not local, whatever model it is running")
	}

	setEnv(t, map[string]string{EnvModelID: "meta.llama3-70b-instruct-v1:0"})
	cfg, err = ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Capabilities().Reasoning {
		t.Error("extended thinking is Claude's; the platform must not claim it for Llama")
	}
}

// An operator can override the inference, because a string match on a model ID is a guess
// and the person running it may know better.
func TestCapabilitiesCanBeOverridden(t *testing.T) {
	setEnv(t, map[string]string{
		EnvModelID: "meta.llama3-70b-instruct-v1:0",
		EnvTools:   "true",
	})
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if !cfg.Capabilities().Tools {
		t.Error("BEDROCK_TOOLS=true must be honoured")
	}
}

// The tools reach Converse in the shape it expects, and the model is NEVER told which of
// them are dangerous.
func TestToolsAreSentToConverseWithoutTheirEffect(t *testing.T) {
	client, rt := claudeClient(t, textOut("done"))

	if _, err := client.Generate(context.Background(), llm.Request{
		Prompt: "Run the blog workflow.",
		System: "You are the platform assistant.",
		Tools:  []llm.ToolSpec{toolSpec()},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := rt.gotIn

	if got.ToolConfig == nil || len(got.ToolConfig.Tools) != 1 {
		t.Fatal("the tool did not reach Bedrock")
	}

	spec, ok := got.ToolConfig.Tools[0].(*types.ToolMemberToolSpec)
	if !ok {
		t.Fatal("wrong tool member type")
	}
	if *spec.Value.Name != "run_workflow" {
		t.Errorf("name = %q", *spec.Value.Name)
	}

	// The schema must be a JSON document, not a string. Sending a string produces a
	// validation error that says nothing about why.
	if _, ok := spec.Value.InputSchema.(*types.ToolInputSchemaMemberJson); !ok {
		t.Error("the input schema must be a JSON document")
	}

	// THE assertion: the Effect is platform metadata and never crosses the wire. Telling the
	// model which tools are dangerous would create the illusion that its judgement was an
	// authorisation boundary. It is not; the registry is.
	blob, _ := json.Marshal(spec.Value)
	if strings.Contains(strings.ToLower(string(blob)), "\"effect\"") {
		t.Errorf("the tool's Effect leaked to the model: %s", blob)
	}
}

// Structured output is a forced tool call. One tool, no choice, so prose is not available.
func TestForcedToolChoiceIsSent(t *testing.T) {
	client, rt := claudeClient(t, textOut("x"))

	if _, err := client.Generate(context.Background(), llm.Request{
		Prompt:     "Triage.",
		Tools:      []llm.ToolSpec{toolSpec()},
		ToolChoice: "run_workflow",
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := rt.gotIn

	choice, ok := got.ToolConfig.ToolChoice.(*types.ToolChoiceMemberTool)
	if !ok {
		t.Fatal("ToolChoice was not a specific tool")
	}
	if *choice.Value.Name != "run_workflow" {
		t.Errorf("forced tool = %q", *choice.Value.Name)
	}
}

// Bedrock's tool_use response becomes the platform's ToolCalls, and the stop reason says
// so — which is what stops the Service treating a tool call as an empty completion.
func TestAToolUseResponseBecomesToolCalls(t *testing.T) {
	client, _ := claudeClient(t, &bedrockruntime.ConverseOutput{
		StopReason: types.StopReasonToolUse,
		Output: &types.ConverseOutputMemberMessage{
			Value: types.Message{
				Role: types.ConversationRoleAssistant,
				Content: []types.ContentBlock{
					&types.ContentBlockMemberToolUse{
						Value: types.ToolUseBlock{
							ToolUseId: ptr("tool-1"),
							Name:      ptr("run_workflow"),
							Input:     brdocument.NewLazyDocument(map[string]any{"workflow": "blog-generator"}),
						},
					},
				},
			},
		},
		Usage: &types.TokenUsage{InputTokens: ptr(int32(10)), OutputTokens: ptr(int32(5))},
	})

	res, err := client.Generate(context.Background(), llm.Request{
		Prompt: "Run it.", Tools: []llm.ToolSpec{toolSpec()},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if res.FinishReason != "tool_use" {
		t.Errorf("finishReason = %q, want tool_use", res.FinishReason)
	}
	if !res.WantsTools() || len(res.ToolCalls) != 1 {
		t.Fatalf("want one tool call, got %+v", res.ToolCalls)
	}

	call := res.ToolCalls[0]
	if call.ID != "tool-1" || call.Name != "run_workflow" {
		t.Errorf("call = %+v", call)
	}

	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("the model's arguments must be usable JSON: %v", err)
	}
	if args["workflow"] != "blog-generator" {
		t.Errorf("arguments = %v", args)
	}
}

// The reasoning block, including its signature, survives the round trip — because Bedrock
// rejects the NEXT turn of a tool-using conversation without it.
func TestReasoningSurvivesTheRoundTrip(t *testing.T) {
	client, rt := claudeClient(t, &bedrockruntime.ConverseOutput{
		StopReason: types.StopReasonEndTurn,
		Output: &types.ConverseOutputMemberMessage{
			Value: types.Message{
				Content: []types.ContentBlock{
					&types.ContentBlockMemberReasoningContent{
						Value: &types.ReasoningContentBlockMemberReasoningText{
							Value: types.ReasoningTextBlock{
								Text: ptr("The user wants the blog workflow."), Signature: ptr("sig-xyz"),
							},
						},
					},
					&types.ContentBlockMemberText{Value: "I'll run it."},
				},
			},
		},
	})

	// Out: the reasoning comes back with its signature.
	res, err := client.Generate(context.Background(), llm.Request{
		Prompt:    "Run it.",
		Reasoning: &llm.ReasoningConfig{BudgetTokens: 1024},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Reasoning == nil || res.Reasoning.Signature != "sig-xyz" {
		t.Fatalf("the reasoning signature was lost: %+v", res.Reasoning)
	}
	if res.Content != "I'll run it." {
		t.Errorf("content = %q — the reasoning must not be mistaken for the answer", res.Content)
	}

	// And thinking was actually requested, in Anthropic's own shape.
	if rt.gotIn.AdditionalModelRequestFields == nil {
		t.Fatal("extended thinking was not requested")
	}

	// In: sending it back puts it FIRST in the assistant message, which is Bedrock's rule.
	if _, err := client.Generate(context.Background(), llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Run it."},
			{
				Role:      llm.RoleAssistant,
				Content:   "I'll run it.",
				Reasoning: &llm.ReasoningBlock{Text: "thinking", Signature: "sig-xyz"},
			},
			{Role: llm.RoleUser, Content: "Thanks."},
		},
		Reasoning: &llm.ReasoningConfig{BudgetTokens: 1024},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	assistant := rt.gotIn.Messages[1]
	if len(assistant.Content) < 2 {
		t.Fatal("the assistant message lost a block")
	}
	if _, ok := assistant.Content[0].(*types.ContentBlockMemberReasoningContent); !ok {
		t.Error("reasoning must be the FIRST block in an assistant message — Bedrock's rule, " +
			"and it rejects the turn otherwise")
	}
}

// Tool results go back in a USER message, matched to the call they answer by ID.
func TestToolResultsAreSentBackMatchedByID(t *testing.T) {
	client, rt := claudeClient(t, textOut("done"))

	if _, err := client.Generate(context.Background(), llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Run it."},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID: "tool-1", Name: "run_workflow", Arguments: json.RawMessage(`{"workflow":"blog-generator"}`),
			}}},
			{Role: llm.RoleUser, ToolResults: []llm.ToolResult{{
				ID: "tool-1", Name: "run_workflow", Content: `{"status":"accepted"}`,
			}}},
		},
		Tools: []llm.ToolSpec{toolSpec()},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	result := rt.gotIn.Messages[2]
	if result.Role != types.ConversationRoleUser {
		t.Errorf("a tool result must be in a USER message, got %q", result.Role)
	}
	block, ok := result.Content[0].(*types.ContentBlockMemberToolResult)
	if !ok {
		t.Fatal("the tool result did not reach Bedrock")
	}
	if *block.Value.ToolUseId != "tool-1" {
		t.Errorf("toolUseId = %q — a mismatched ID makes the model reason about the wrong "+
			"answer, untraceably", *block.Value.ToolUseId)
	}
	if block.Value.Status != types.ToolResultStatusSuccess {
		t.Errorf("status = %q", block.Value.Status)
	}
}

// A failed tool is reported to the model AS a failure, which is how it recovers.
func TestAFailedToolIsMarkedAsAnErrorToTheModel(t *testing.T) {
	client, rt := claudeClient(t, textOut("I see, I'll try something else."))

	if _, err := client.Generate(context.Background(), llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Run it."},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID: "t1", Name: "run_workflow", Arguments: json.RawMessage(`{}`),
			}}},
			{Role: llm.RoleUser, ToolResults: []llm.ToolResult{{
				ID: "t1", Name: "run_workflow", IsError: true, Content: "n8n is unreachable",
			}}},
		},
		Tools: []llm.ToolSpec{toolSpec()},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	block := rt.gotIn.Messages[2].Content[0].(*types.ContentBlockMemberToolResult)
	if block.Value.Status != types.ToolResultStatusError {
		t.Error("a failed tool must be reported to the model as an error, or it cannot recover")
	}
}

// A cache point is only worth writing when there is a stable prefix AND more than one turn
// to amortise it over.
func TestPromptCachingIsAppliedOnlyWhereItPays(t *testing.T) {
	client, rt := claudeClient(t, textOut("x"))
	client.cfg.PromptCache = true

	// With tools: the system prompt and the tool schemas are re-sent every turn, so caching
	// the stable prefix is most of the saving.
	if _, err := client.Generate(context.Background(), llm.Request{
		Prompt: "go", System: "You are the platform assistant.", Tools: []llm.ToolSpec{toolSpec()},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(rt.gotIn.System) != 2 {
		t.Fatalf("want a system block and a cache point, got %d blocks", len(rt.gotIn.System))
	}
	if _, ok := rt.gotIn.System[1].(*types.SystemContentBlockMemberCachePoint); !ok {
		t.Error("a tool-using request should end its system prompt with a cache point")
	}

	// Without tools it is a single-shot call, read once. A cache write that is never read
	// costs money and saves nothing.
	if _, err := client.Generate(context.Background(), llm.Request{
		Prompt: "go", System: "You are the platform assistant.",
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(rt.gotIn.System) != 1 {
		t.Error("a single-shot call should not pay for a cache write it will never read")
	}
}
