package bedrock

import (
	"encoding/json"
	"strings"

	brdocument "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// This file is Milestone 9: everything that turns Bedrock's Converse API from "produces
// text" into "reasons, returns a schema, and calls the platform's tools".
//
// It is deliberately separate from bedrock.go, because it answers a different question.
// bedrock.go answers "how do we talk to Bedrock at all". This answers "how do we ask a
// frontier model to DO something", and the two have very different failure modes.

// isClaude reports whether a model ID names a Claude model, including through a
// cross-region inference profile ("us.anthropic.claude-…").
//
// A string match on a model ID is not elegant. The alternative is worse: Bedrock's
// catalogue (ListFoundationModels) does not report tool-use or reasoning support, so
// there is nothing to ask. See the note on [llm.Capabilities] — capability is asserted,
// not discovered, because the only discovery mechanism AWS offers is a failed invocation.
func isClaude(modelID string) bool {
	id := strings.ToLower(modelID)
	// Strip a regional inference-profile prefix: "us.", "eu.", "apac.".
	if i := strings.Index(id, "anthropic."); i >= 0 {
		id = id[i:]
	}
	return strings.HasPrefix(id, "anthropic.claude")
}

// toolConfig maps the platform's tools onto Converse's tool configuration.
//
// Note what is NOT sent: [llm.ToolSpec.Effect]. The model is never told which tools are
// dangerous. Its opinion on that is not an authorisation boundary — the registry is, and
// the loop is — and telling it would only create the comfortable illusion that something
// was being enforced.
func toolConfig(req llm.Request) *types.ToolConfiguration {
	if len(req.Tools) == 0 {
		return nil
	}

	tools := make([]types.Tool, 0, len(req.Tools))
	for _, spec := range req.Tools {
		tools = append(tools, &types.ToolMemberToolSpec{
			Value: types.ToolSpecification{
				Name:        ptr(spec.Name),
				Description: ptr(spec.Description),
				InputSchema: &types.ToolInputSchemaMemberJson{
					// NewLazyDocument, not a marshalled string: Converse wants the schema as a
					// JSON document, and handing it a string produces a validation error that
					// says nothing useful about why.
					Value: brdocument.NewLazyDocument(spec.Schema),
				},
			},
		})
	}

	cfg := &types.ToolConfiguration{Tools: tools}

	// Forcing a specific tool is how [llm.Structured] guarantees a schema-shaped answer:
	// with exactly one tool available and no choice about calling it, the model's only
	// possible move is to fill in the form.
	if req.ToolChoice != "" {
		cfg.ToolChoice = &types.ToolChoiceMemberTool{
			Value: types.SpecificToolChoice{Name: ptr(req.ToolChoice)},
		}
	}
	return cfg
}

// reasoningFields builds the extended-thinking configuration.
//
// This is the one place Converse's unified surface leaks: thinking is not a first-class
// field, it is passed through additionalModelRequestFields in **Anthropic's own** request
// format. So a Claude-specific shape lands in the provider — which is exactly where it
// should land, and exactly why the platform's own [llm.ReasoningConfig] is a different,
// simpler thing.
func reasoningFields(req llm.Request) brdocument.Interface {
	if req.Reasoning == nil {
		return nil
	}
	return brdocument.NewLazyDocument(map[string]any{
		"thinking": map[string]any{
			"type": "enabled",
			// Billed as OUTPUT tokens, and drawn from the same budget as the answer. The
			// Service refuses a budget that would leave no room to reply.
			"budget_tokens": req.Reasoning.BudgetTokens,
		},
	})
}

// systemBlocks builds the system prompt, optionally ending it with a cache point.
//
// # Why caching matters so much more here than it looks
//
// In a tool loop the entire conversation is re-sent every turn: system prompt, every tool
// schema, every previous message. The system prompt and the tool schemas are the same
// bytes each time — and they are frequently the *largest* part of the request.
//
// A cache point tells Bedrock "everything above this is stable", and the stable prefix is
// then billed at roughly a tenth of the input price on every subsequent turn. On a
// six-turn loop with a large tool set, that is not a micro-optimisation; it is most of the
// bill.
//
// The catch, and it is a real one: a cached prefix must be IDENTICAL, byte for byte. Put
// anything that varies — a timestamp, a correlation ID, "today is Tuesday" — above the
// cache point and the cache never hits, silently, and you pay full price forever while
// believing you are not.
func (c *Client) systemBlocks(req llm.Request) []types.SystemContentBlock {
	if strings.TrimSpace(req.System) == "" {
		return nil
	}

	blocks := []types.SystemContentBlock{
		&types.SystemContentBlockMemberText{Value: req.System},
	}

	// Only worth a cache point when there is a stable prefix worth caching AND more than
	// one turn to amortise it over. Caching a prefix that is read exactly once costs a
	// cache write and saves nothing.
	if c.cfg.PromptCache && len(req.Tools) > 0 {
		blocks = append(blocks, &types.SystemContentBlockMemberCachePoint{
			Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
		})
	}
	return blocks
}

// messagesWithTools maps the platform's conversation onto Converse's, including the parts
// that only exist once a model can call tools.
//
// The ordering rules here are Bedrock's and they are strict:
//
//   - a reasoning block must come FIRST in an assistant message, before any text;
//   - a tool result must be in a USER message, never an assistant one;
//   - every toolUse must be answered by exactly one toolResult, matched on ID.
//
// Get any of them wrong and Bedrock rejects the turn with a ValidationException that
// describes the symptom rather than the rule.
func (c *Client) messagesWithTools(req llm.Request) []types.Message {
	if len(req.Messages) == 0 {
		return []types.Message{{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: req.Prompt}},
		}}
	}

	out := make([]types.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := types.ConversationRoleUser
		if m.Role == llm.RoleAssistant {
			role = types.ConversationRoleAssistant
		}

		var blocks []types.ContentBlock

		// Reasoning first. Bedrock requires it, and it requires the SIGNATURE back
		// untouched — it is how the service knows the thinking it is being shown is the
		// thinking it produced. Drop it, and a tool-using conversation with reasoning
		// enabled fails on its second turn with an error that never mentions signatures.
		if m.Reasoning != nil {
			switch {
			case len(m.Reasoning.Redacted) > 0:
				blocks = append(blocks, &types.ContentBlockMemberReasoningContent{
					Value: &types.ReasoningContentBlockMemberRedactedContent{
						Value: m.Reasoning.Redacted,
					},
				})
			case m.Reasoning.Text != "":
				blocks = append(blocks, &types.ContentBlockMemberReasoningContent{
					Value: &types.ReasoningContentBlockMemberReasoningText{
						Value: types.ReasoningTextBlock{
							Text:      ptr(m.Reasoning.Text),
							Signature: ptr(m.Reasoning.Signature),
						},
					},
				})
			}
		}

		if m.Content != "" {
			blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Content})
		}

		for _, call := range m.ToolCalls {
			blocks = append(blocks, &types.ContentBlockMemberToolUse{
				Value: types.ToolUseBlock{
					ToolUseId: ptr(call.ID),
					Name:      ptr(call.Name),
					Input:     brdocument.NewLazyDocument(rawToAny(call.Arguments)),
				},
			})
		}

		for _, result := range m.ToolResults {
			status := types.ToolResultStatusSuccess
			if result.IsError {
				// The model is TOLD the tool failed. That is not a leak, it is the mechanism
				// by which it recovers — it tries different arguments, or it explains that it
				// cannot do the thing, and both are far better than the platform giving up.
				status = types.ToolResultStatusError
			}
			blocks = append(blocks, &types.ContentBlockMemberToolResult{
				Value: types.ToolResultBlock{
					ToolUseId: ptr(result.ID),
					Status:    status,
					Content: []types.ToolResultContentBlock{
						&types.ToolResultContentBlockMemberText{Value: result.Content},
					},
				},
			})
		}

		if len(blocks) == 0 {
			continue
		}
		out = append(out, types.Message{Role: role, Content: blocks})
	}
	return out
}

// contentOf pulls the platform's Response out of a Converse message: the text, the tool
// calls it wants run, and the reasoning it did on the way.
func contentOf(blocks []types.ContentBlock) (string, []llm.ToolCall, *llm.ReasoningBlock) {
	var text strings.Builder
	var calls []llm.ToolCall
	var reasoning *llm.ReasoningBlock

	for _, block := range blocks {
		switch b := block.(type) {
		case *types.ContentBlockMemberText:
			text.WriteString(b.Value)

		case *types.ContentBlockMemberToolUse:
			calls = append(calls, llm.ToolCall{
				ID:        deref(b.Value.ToolUseId),
				Name:      deref(b.Value.Name),
				Arguments: argumentsOf(b.Value.Input),
			})

		case *types.ContentBlockMemberReasoningContent:
			switch r := b.Value.(type) {
			case *types.ReasoningContentBlockMemberReasoningText:
				reasoning = &llm.ReasoningBlock{
					Text:      deref(r.Value.Text),
					Signature: deref(r.Value.Signature),
				}
			case *types.ReasoningContentBlockMemberRedactedContent:
				// The provider encrypted its own thinking. We cannot read it; we must still
				// carry it back, or the next turn is rejected.
				reasoning = &llm.ReasoningBlock{Redacted: r.Value}
			}
		}
	}
	return text.String(), calls, reasoning
}

// argumentsOf pulls the model's arguments out of a Smithy document.
//
// It has to go through UnmarshalSmithyDocument, and that is not a detail — json.Marshal on
// the document yields an EMPTY OBJECT. A document is a lazily-encoded protocol type, not a
// struct with public fields, so the obvious code compiles, runs, and silently produces
// `{}` for every tool call the model ever makes. The visible symptom is bizarre and
// misleading: every call fails schema validation with "missing required argument", and the
// model, told this, dutifully re-sends exactly the arguments it sent the first time, until
// the loop hits its bound.
//
// A test that asserted only "a tool call came back" would have passed. It took asserting
// on the ARGUMENTS to find it.
func argumentsOf(input brdocument.Interface) json.RawMessage {
	if input == nil {
		return json.RawMessage("{}")
	}

	var args map[string]any
	err := input.UnmarshalSmithyDocument(&args)

	// Deliberately checking the VALUE and not just the error, which looks like sloppiness
	// and is not.
	//
	// The SDK's lazy document populates the target correctly and STILL returns a non-nil
	// error ("unsupported json type, *map[string]interface {}") for a generic map target.
	// Trusting the error and discarding the value gives every tool call empty arguments —
	// and the symptom is thoroughly misleading: the model is told "missing required
	// argument", it dutifully re-sends exactly what it sent before, and the loop grinds
	// round to its bound while the actual arguments were there the whole time.
	//
	// So: if we got the arguments, use them.
	if len(args) == 0 {
		if err != nil {
			return json.RawMessage("{}")
		}
		// A tool with no arguments is legitimate — list_workflows takes none.
		return json.RawMessage("{}")
	}

	out, err := json.Marshal(args)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

// rawToAny decodes the model's argument JSON so the SDK can re-encode it as a document.
func rawToAny(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]any{}
	}
	return v
}

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
