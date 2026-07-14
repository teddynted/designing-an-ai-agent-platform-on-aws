package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// --- a scripted provider that can ask for tools ------------------------------

// scriptedProvider replays a fixed sequence of responses, one per turn, so a test can say
// "first the model asks for a tool, then it answers" without an HTTP server or a model.
type scriptedProvider struct {
	caps  Capabilities
	turns []Response
	errs  []error

	calls int
	got   []Request
}

func (p *scriptedProvider) Name() string               { return "scripted" }
func (p *scriptedProvider) Capabilities() Capabilities { return p.caps }
func (p *scriptedProvider) Models(context.Context) ([]Model, error) {
	return nil, nil
}
func (p *scriptedProvider) Stream(_ context.Context, _ Request, _ Sink) (Response, error) {
	return Response{}, errors.New("not used")
}

func (p *scriptedProvider) Generate(_ context.Context, req Request) (Response, error) {
	i := p.calls
	p.calls++
	p.got = append(p.got, req)

	if i < len(p.errs) && p.errs[i] != nil {
		return Response{Attempts: 1}, p.errs[i]
	}
	if i < len(p.turns) {
		return p.turns[i], nil
	}
	// Ran off the end of the script: keep asking for a tool forever, which is exactly what
	// a stuck model does and is what the iteration bound exists to stop.
	return toolTurn("list_workflows", `{}`), nil
}

func capableProvider(turns ...Response) *scriptedProvider {
	return &scriptedProvider{
		caps: Capabilities{
			MaxContextTokens: 200000,
			Tools:            true,
			StructuredOutput: true,
			Reasoning:        true,
			// Priced, so the loop's cost accounting has something to accumulate.
			CostPer1MInputTokensUSD:  3.0,
			CostPer1MOutputTokensUSD: 15.0,
		},
		turns: turns,
	}
}

func toolTurn(name, args string) Response {
	return Response{
		Model:        "claude",
		FinishReason: "tool_use",
		Attempts:     1,
		Usage:        Usage{PromptTokens: 1000, CompletionTokens: 50},
		ToolCalls: []ToolCall{{
			ID: "call-" + name, Name: name, Arguments: json.RawMessage(args),
		}},
	}
}

func answerTurn(text string) Response {
	return Response{
		Model:        "claude",
		Content:      text,
		FinishReason: "stop",
		Attempts:     1,
		Usage:        Usage{PromptTokens: 1200, CompletionTokens: 80},
	}
}

// --- a runner whose tools record what happened -------------------------------

type fakeRunner struct {
	specs []ToolSpec
	ran   []string
	keys  []string
	err   error
}

func (r *fakeRunner) Specs() []ToolSpec { return r.specs }

func (r *fakeRunner) Run(_ context.Context, call ToolCall) (ToolResult, error) {
	r.ran = append(r.ran, call.Name)
	r.keys = append(r.keys, call.IdempotencyKey)
	if r.err != nil {
		return ToolResult{}, r.err
	}
	return ToolResult{ID: call.ID, Name: call.Name, Content: `{"ok":true}`}, nil
}

func runner() *fakeRunner {
	return &fakeRunner{specs: []ToolSpec{
		{
			Name: "list_workflows", Description: "list them",
			Schema: Object(map[string]any{}), Effect: Read,
		},
		{
			Name: "run_workflow", Description: "run one — THIS CHANGES THINGS",
			Schema: Object(map[string]any{
				"workflow": String("which", "blog-generator", "release-notes"),
				"reason":   String("why, for the audit log"),
			}, "workflow"),
			Effect: Write,
		},
	}}
}

// --- the happy path ----------------------------------------------------------

func TestTheLoopRunsToolsAndThenAnswers(t *testing.T) {
	provider := capableProvider(
		toolTurn("list_workflows", `{}`),
		answerTurn("There are two workflows."),
	)
	svc, _ := newService(provider)
	run := runner()

	convo, err := svc.Converse(context.Background(), Request{
		Prompt: "What can this platform run?", CorrelationID: "push:abc",
	}, run, LoopPolicy{})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}

	if convo.Content != "There are two workflows." {
		t.Errorf("content = %q, want the final answer", convo.Content)
	}
	if len(convo.Turns) != 2 {
		t.Errorf("turns = %d, want 2 (one tool call, one answer)", len(convo.Turns))
	}
	if len(run.ran) != 1 || run.ran[0] != "list_workflows" {
		t.Errorf("ran %v, want the one tool the model asked for", run.ran)
	}

	// Usage is the SUM across turns. It is the number people are surprised by, and a loop
	// that reported only the last turn's usage would under-report the bill by most of it.
	wantPrompt := 1000 + 1200
	if convo.Usage.PromptTokens != wantPrompt {
		t.Errorf("promptTokens = %d, want %d — the loop must ACCUMULATE, because every turn "+
			"re-sends the whole conversation", convo.Usage.PromptTokens, wantPrompt)
	}
	if convo.EstimatedCostUSD <= 0 {
		t.Error("a priced provider must produce a cost estimate")
	}

	// Nothing changed in the world, so this is safe to retry.
	if convo.EffectsCommitted {
		t.Error("only a read tool ran; nothing was committed")
	}
}

// THE test of the milestone.
//
// Milestones 7 and 8 both said, in bold, that retrying an inference is safe because
// generation has no side effects. The moment a model can call run_workflow, that is false:
// the workflow HAS RUN. If the conversation then fails, retrying it from the top runs the
// workflow a second time — which is the exact failure Milestone 5 spent a milestone
// learning to avoid.
func TestAFailureAfterAWriteToolIsNotRetryable(t *testing.T) {
	provider := capableProvider(
		toolTurn("run_workflow", `{"workflow":"blog-generator"}`), // the workflow RUNS
		Response{}, // and then the next turn fails
	)
	provider.errs = []error{nil, ErrThrottled}

	svc, logs := newService(provider)
	run := runner()

	convo, err := svc.Converse(context.Background(), Request{
		Prompt: "Run the blog workflow.", CorrelationID: "push:abc",
	}, run, LoopPolicy{})

	if err == nil {
		t.Fatal("want an error")
	}
	if !convo.EffectsCommitted {
		t.Fatal("a Write tool ran; the conversation must record that the world moved")
	}

	// The cause survives, so a log can still say WHAT went wrong...
	if !errors.Is(err, ErrThrottled) {
		t.Errorf("the cause must survive: %v", err)
	}
	// ...and the consequence is attached, so a retry policy can see that it must not.
	if !errors.Is(err, ErrEffectsCommitted) {
		t.Fatalf("a failure after a Write tool must be ErrEffectsCommitted, or a caller will "+
			"retry it and run the workflow twice: %v", err)
	}

	// And the one-line answer to "may I try that again?" is no.
	if Retryable(err) {
		t.Error("Retryable() said yes. The workflow has already run.")
	}

	// The log has to say so loudly: this is the line an operator reads at 3am before
	// deciding whether to re-run the job.
	if !strings.Contains(logs.String(), `"safeToRetry":false`) {
		t.Error("the failure log must say safeToRetry=false")
	}
	if !strings.Contains(logs.String(), `"errorKind":"effects_committed"`) {
		t.Error("the errorKind must be effects_committed, not throttled — the fact that " +
			"decides what a human does next is that the world already moved")
	}
}

// The mirror of the above: a read-only tool commits nothing, so the same failure IS
// retryable. If this were not true, the platform would refuse to retry a summary because
// it once looked something up, which is uselessly conservative.
func TestAFailureAfterOnlyReadToolsIsStillRetryable(t *testing.T) {
	provider := capableProvider(toolTurn("list_workflows", `{}`), Response{})
	provider.errs = []error{nil, ErrThrottled}

	svc, _ := newService(provider)

	convo, err := svc.Converse(context.Background(), Request{
		Prompt: "What can run?", CorrelationID: "c",
	}, runner(), LoopPolicy{})

	if err == nil {
		t.Fatal("want an error")
	}
	if convo.EffectsCommitted {
		t.Error("a read tool changed nothing")
	}
	if errors.Is(err, ErrEffectsCommitted) {
		t.Error("nothing was committed; this must not be marked terminal")
	}
	if !Retryable(err) {
		t.Error("a throttle after read-only tools is retryable — nothing has happened yet")
	}
}

// A model that sends the wrong argument type is not an emergency. It is Tuesday.
//
// The failure is handed BACK to the model, which is what lets it correct itself — and the
// tool is never run, which is what stops it being run with arguments nobody validated.
func TestBadArgumentsGoBackToTheModelAndTheToolNeverRuns(t *testing.T) {
	provider := capableProvider(
		toolTurn("run_workflow", `{"workflow":"nonexistent-workflow"}`), // not in the enum
		answerTurn("Sorry — that workflow does not exist."),
	)
	svc, _ := newService(provider)
	run := runner()

	convo, err := svc.Converse(context.Background(), Request{
		Prompt: "Run it.", CorrelationID: "c",
	}, run, LoopPolicy{})
	if err != nil {
		t.Fatalf("a schema violation must not fail the conversation: %v", err)
	}

	// The load-bearing assertion: a Write tool with invalid arguments DID NOT RUN.
	if len(run.ran) != 0 {
		t.Fatalf("the tool ran with arguments that failed validation: %v", run.ran)
	}
	if convo.EffectsCommitted {
		t.Error("nothing ran, so nothing was committed")
	}

	// And the model was told exactly what was wrong, which is how it recovers.
	second := provider.got[1]
	last := second.Messages[len(second.Messages)-1]
	if len(last.ToolResults) != 1 || !last.ToolResults[0].IsError {
		t.Fatal("the validation failure must be handed back as an error result")
	}
	if !strings.Contains(last.ToolResults[0].Content, "not one of") {
		t.Errorf("the message must name the actual problem, or the model cannot fix it: %q",
			last.ToolResults[0].Content)
	}
}

// A model that invents a tool gets told the tool does not exist, and carries on. It does
// not take the platform down with it.
func TestAnInventedToolIsRejectedAndExplained(t *testing.T) {
	provider := capableProvider(
		toolTurn("delete_production", `{}`),
		answerTurn("I cannot do that."),
	)
	svc, _ := newService(provider)
	run := runner()

	if _, err := svc.Converse(context.Background(), Request{
		Prompt: "Delete production.", CorrelationID: "c",
	}, run, LoopPolicy{}); err != nil {
		t.Fatalf("an invented tool must not fail the conversation: %v", err)
	}
	if len(run.ran) != 0 {
		t.Error("a tool that is not in the registry must never reach the runner")
	}
}

// The bound. A model that never stops calling tools stops costing money at turn N.
func TestTheLoopIsBounded(t *testing.T) {
	// The scripted provider asks for a tool forever once its script runs out.
	provider := capableProvider()
	svc, _ := newService(provider)

	_, err := svc.Converse(context.Background(), Request{
		Prompt: "Go forever.", CorrelationID: "c",
	}, runner(), LoopPolicy{MaxIterations: 3})

	if !errors.Is(err, ErrToolLoop) {
		t.Fatalf("an unbounded model must be stopped: %v", err)
	}
	if provider.calls != 3 {
		t.Errorf("the model was called %d times, want exactly the bound (3). On a per-token "+
			"API the failure mode of an unbounded loop is a bill, not a hang", provider.calls)
	}
}

// The cost bound: the same idea as Milestone 6's agent budget, enforced from our side
// because we are the ones going round the loop.
func TestTheLoopStopsWhenItGetsTooExpensive(t *testing.T) {
	provider := capableProvider()
	svc, _ := newService(provider)

	_, err := svc.Converse(context.Background(), Request{
		Prompt: "Spend everything.", CorrelationID: "c",
	}, runner(), LoopPolicy{MaxIterations: 100, MaxCostUSD: 0.00001})

	if !errors.Is(err, ErrToolLoop) {
		t.Fatalf("the loop must stop at the cost bound: %v", err)
	}
	if provider.calls > 3 {
		t.Errorf("it kept going for %d turns after passing the budget", provider.calls)
	}
}

// A provider that cannot use tools is REFUSED, not asked and hoped over.
//
// This is the silent-truncation lesson again: a model given a capability it does not have
// does not error, it invents. Ollama would ignore the tools and answer from memory,
// fluently and wrongly, and nothing in any log would say so.
func TestAProviderWithoutToolsIsRefused(t *testing.T) {
	local := newFake() // Local Ollama-alike: Tools is false.
	svc, _ := newService(local)

	_, err := svc.Converse(context.Background(), Request{
		Prompt: "Run a workflow.", CorrelationID: "c",
	}, runner(), LoopPolicy{})

	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	if local.calls != 0 {
		t.Error("the request must never be sent to a provider that cannot serve it")
	}
	if !strings.Contains(err.Error(), "invents") {
		t.Errorf("the error should say WHY refusing beats asking: %v", err)
	}
}

// The idempotency key is DERIVED, and the same call always produces the same key — which
// is the only thing that makes a Write tool safe to see twice.
func TestTheIdempotencyKeyIsDerivedAndStable(t *testing.T) {
	args := json.RawMessage(`{"workflow":"blog-generator","reason":"asked"}`)
	// Same arguments, different key ORDER. A model does not emit keys in a stable order,
	// and two calls that are identical in every way that matters must not hash differently.
	reordered := json.RawMessage(`{"reason":"asked","workflow":"blog-generator"}`)

	a := DeriveIdempotencyKey("push:abc", "run_workflow", args)
	b := DeriveIdempotencyKey("push:abc", "run_workflow", reordered)
	if a != b {
		t.Errorf("key order changed the key (%s vs %s) — the same call would run twice", a, b)
	}

	// A different cause is a different key: the same workflow, legitimately run for two
	// different events, must not be deduplicated into one.
	if c := DeriveIdempotencyKey("push:def", "run_workflow", args); c == a {
		t.Error("a different correlation must produce a different key")
	}
	// And it is not random.
	if a != DeriveIdempotencyKey("push:abc", "run_workflow", args) {
		t.Error("the key must be a pure function of its inputs; a random key guarantees the " +
			"double-run it exists to prevent")
	}
}

// The tools reach the runner with a key, so a Write tool can deduplicate.
func TestWriteToolsReceiveAnIdempotencyKey(t *testing.T) {
	provider := capableProvider(
		toolTurn("run_workflow", `{"workflow":"blog-generator"}`),
		answerTurn("Started."),
	)
	svc, _ := newService(provider)
	run := runner()

	if _, err := svc.Converse(context.Background(), Request{
		Prompt: "Run it.", CorrelationID: "push:abc",
	}, run, LoopPolicy{}); err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if len(run.keys) != 1 || run.keys[0] == "" {
		t.Fatal("a tool must receive an idempotency key, or a retry becomes a double-run")
	}
}

// The arguments are never logged: they are derived from the prompt, and the prompt is
// repository content. The tool NAME is logged, because "which tool ran?" must be
// answerable.
func TestToolArgumentsAreNotLogged(t *testing.T) {
	secret := "ghp_thisisacredentialthatleakedintoadiff"
	provider := capableProvider(
		toolTurn("run_workflow", `{"workflow":"blog-generator","reason":"`+secret+`"}`),
		answerTurn("Done."),
	)
	svc, logs := newService(provider)

	if _, err := svc.Converse(context.Background(), Request{
		Prompt: "Run it.", CorrelationID: "c",
	}, runner(), LoopPolicy{}); err != nil {
		t.Fatalf("Converse: %v", err)
	}

	if strings.Contains(logs.String(), secret) {
		t.Error("the model's tool arguments reached the logs — they are derived from the " +
			"prompt, which is repository content")
	}
	if !strings.Contains(logs.String(), `"tool":"run_workflow"`) {
		t.Error("the tool name must be logged: 'why did this pull request appear?' has to " +
			"be answerable")
	}
	if !strings.Contains(logs.String(), "CHANGES SOMETHING") {
		t.Error("a Write tool crossing the boundary must be conspicuous in the log")
	}
}

// Reasoning has to be carried back into the next turn, verbatim, or Bedrock rejects it.
func TestReasoningIsCarriedIntoTheNextTurn(t *testing.T) {
	thinking := &ReasoningBlock{Text: "The user wants the blog workflow.", Signature: "sig-abc"}

	first := toolTurn("list_workflows", `{}`)
	first.Reasoning = thinking

	provider := capableProvider(first, answerTurn("Two workflows."))
	svc, _ := newService(provider)

	if _, err := svc.Converse(context.Background(), Request{
		Prompt: "What can run?", CorrelationID: "c",
	}, runner(), LoopPolicy{}); err != nil {
		t.Fatalf("Converse: %v", err)
	}

	second := provider.got[1]
	var carried *ReasoningBlock
	for _, m := range second.Messages {
		if m.Reasoning != nil {
			carried = m.Reasoning
		}
	}
	if carried == nil {
		t.Fatal("the model's reasoning was dropped; Bedrock rejects the next turn without it")
	}
	if carried.Signature != "sig-abc" {
		t.Errorf("the signature must be echoed back untouched, got %q", carried.Signature)
	}
}

// A tool loop grows the conversation every turn. The context check must see the WHOLE
// thing — tools, results and all — or it will pass on turn one and let the model be
// silently truncated on turn six.
func TestTheContextCheckCountsToolsAndResults(t *testing.T) {
	req := Request{
		System: "sys",
		Tools: []ToolSpec{{
			Name: "run_workflow", Description: strings.Repeat("x", 500),
			Schema: Object(map[string]any{"workflow": String("which")}, "workflow"),
		}},
		Messages: []Message{
			{Role: RoleUser, Content: "go"},
			{Role: RoleUser, ToolResults: []ToolResult{{Content: strings.Repeat("y", 1000)}}},
		},
	}

	input := req.Input()
	for _, want := range []string{"run_workflow", strings.Repeat("y", 1000)} {
		if !strings.Contains(input, want) {
			t.Errorf("Input() must count %q — the conversation is what gets re-sent, and a "+
				"check that only measured the first prompt would miss the truncation", want[:12])
		}
	}
}
