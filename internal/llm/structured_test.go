package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Triage is the sort of thing the platform actually wants back from a model: not prose,
// but a decision it can branch on.
type Triage struct {
	Severity string   `json:"severity"`
	Summary  string   `json:"summary"`
	Files    []string `json:"files"`
}

// Validate is the check the JSON Schema could never make.
//
// A schema can say "severity is one of low/medium/high/critical". It cannot say "if you
// called it critical, you must be able to point at the file that made you say so" — and
// that is exactly the rule that catches a model which has produced perfectly-shaped,
// entirely invented JSON.
func (t Triage) Validate() error {
	if t.Severity == "critical" && len(t.Files) == 0 {
		return fmt.Errorf("a critical finding must cite at least one file")
	}
	return nil
}

func triageSchema() Schema {
	return Schema{
		Name:        "change_triage",
		Description: "Triage a change.",
		Definition: Object(map[string]any{
			"severity": String("How bad is it?", "low", "medium", "high", "critical"),
			"summary":  String("One sentence."),
			"files":    map[string]any{"type": "array", "description": "Files that caused the rating"},
		}, "severity", "summary"),
	}
}

// structuredTurn is a model answering with a forced tool call, which is how Bedrock does
// structured output.
func structuredTurn(args string) Response {
	return Response{
		Model: "claude", FinishReason: "tool_use", Attempts: 1,
		Usage:     Usage{PromptTokens: 500, CompletionTokens: 40},
		ToolCalls: []ToolCall{{ID: "c1", Name: "change_triage", Arguments: json.RawMessage(args)}},
	}
}

func TestStructuredReturnsATypedValue(t *testing.T) {
	provider := capableProvider(structuredTurn(
		`{"severity":"low","summary":"Docs only.","files":[]}`))
	svc, _ := newService(provider)

	got, _, err := Structured[Triage](context.Background(), svc,
		Request{Prompt: "Triage this.", CorrelationID: "c"}, triageSchema())
	if err != nil {
		t.Fatalf("Structured: %v", err)
	}
	if got.Severity != "low" || got.Summary != "Docs only." {
		t.Errorf("got %+v, want the model's answer, typed", got)
	}

	// It must be sent as ONE tool, forced. That is what makes prose impossible: the only
	// move available to the model is to fill in the form.
	sent := provider.got[0]
	if len(sent.Tools) != 1 || sent.ToolChoice != "change_triage" {
		t.Errorf("tools=%d choice=%q, want exactly one forced tool", len(sent.Tools), sent.ToolChoice)
	}
}

// The model invents a field. encoding/json's DEFAULT would silently drop it — leaving a
// struct that is merely missing something rather than one that is visibly wrong, which is
// far harder to debug. DisallowUnknownFields keeps the evidence.
func TestAnInventedFieldIsRejectedRatherThanDropped(t *testing.T) {
	provider := capableProvider(
		structuredTurn(`{"severity":"low","summary":"ok","exploit":"rm -rf /"}`),
		structuredTurn(`{"severity":"low","summary":"ok"}`), // the repair
	)
	svc, _ := newService(provider)

	got, _, err := Structured[Triage](context.Background(), svc,
		Request{Prompt: "Triage.", CorrelationID: "c"}, triageSchema())
	if err != nil {
		t.Fatalf("the repair should have succeeded: %v", err)
	}
	if got.Severity != "low" {
		t.Errorf("got %+v", got)
	}
	if provider.calls != 2 {
		t.Errorf("calls = %d, want a first attempt and one repair", provider.calls)
	}
}

// The repair loop: the model is TOLD exactly what was wrong, and fixes it.
//
// Naming the precise problem is what makes this work at all. "Invalid JSON" gets you a
// different invalid answer; "severity must be one of low, medium, high, critical and you
// sent 'urgent'" gets you a correct one.
func TestASchemaViolationIsHandedBackWithTheReason(t *testing.T) {
	provider := capableProvider(
		structuredTurn(`{"severity":"critical","summary":"Something!","files":[]}`), // fails Validate()
		structuredTurn(`{"severity":"critical","summary":"Creds in config.","files":["config.go"]}`),
	)
	svc, _ := newService(provider)

	got, _, err := Structured[Triage](context.Background(), svc,
		Request{Prompt: "Triage.", CorrelationID: "c"}, triageSchema())
	if err != nil {
		t.Fatalf("the repair should have succeeded: %v", err)
	}
	if len(got.Files) != 1 {
		t.Errorf("got %+v, want the repaired answer", got)
	}

	// The second request must contain the specific complaint.
	second := provider.got[1]
	last := second.Messages[len(second.Messages)-1]
	if len(last.ToolResults) == 0 || !last.ToolResults[0].IsError {
		t.Fatal("the violation must go back as an error result")
	}
	if !strings.Contains(last.ToolResults[0].Content, "must cite at least one file") {
		t.Errorf("the model must be told the ACTUAL problem: %q", last.ToolResults[0].Content)
	}
}

// Repair is bounded. A model that fails the schema twice has misunderstood the task, and
// asking a third time just spends money on the same misunderstanding.
func TestRepairIsBounded(t *testing.T) {
	provider := capableProvider(
		structuredTurn(`{"severity":"critical","summary":"x","files":[]}`),
		structuredTurn(`{"severity":"critical","summary":"x","files":[]}`),
		structuredTurn(`{"severity":"critical","summary":"x","files":[]}`),
	)
	svc, _ := newService(provider)

	_, _, err := Structured[Triage](context.Background(), svc,
		Request{Prompt: "Triage.", CorrelationID: "c"}, triageSchema())

	if !errors.Is(err, ErrSchemaViolation) {
		t.Fatalf("want ErrSchemaViolation, got %v", err)
	}
	if provider.calls != 1+DefaultRepairAttempts {
		t.Errorf("calls = %d, want 1 attempt + %d repair(s). Each repair re-sends the whole "+
			"conversation and is billed for it", provider.calls, DefaultRepairAttempts)
	}
}

// A provider that cannot be held to a schema is REFUSED.
//
// The failure being prevented: a 3B local model handed a schema does not refuse. It
// produces confident, well-formed, invented JSON, and the platform would parse it happily
// and act on it. Nothing anywhere would report an error.
func TestAProviderWithoutStructuredOutputIsRefused(t *testing.T) {
	local := newFake() // StructuredOutput is false
	svc, _ := newService(local)

	_, _, err := Structured[Triage](context.Background(), svc,
		Request{Prompt: "Triage.", CorrelationID: "c"}, triageSchema())

	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	if local.calls != 0 {
		t.Error("nothing must be sent to a provider that cannot serve it")
	}
	if !strings.Contains(err.Error(), "invented JSON") {
		t.Errorf("the error must say why refusing beats asking: %v", err)
	}
}

// If the provider ignores a forced tool choice and answers in prose, that is a broken
// assumption and it must surface — not be papered over by parsing the prose.
func TestProseWhereASchemaWasRequiredIsAnError(t *testing.T) {
	provider := capableProvider(answerTurn("I think it's fine, honestly."))
	svc, _ := newService(provider)

	if _, _, err := Structured[Triage](context.Background(), svc,
		Request{Prompt: "Triage.", CorrelationID: "c"}, triageSchema()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("want ErrInvalidResponse, got %v", err)
	}
}

// --- argument validation -----------------------------------------------------

func TestValidateArguments(t *testing.T) {
	spec := ToolSpec{
		Name: "run_workflow",
		Schema: Object(map[string]any{
			"workflow": String("which", "blog-generator", "release-notes"),
			"dryRun":   Bool("do not really run it"),
			"limit":    Integer("how many"),
		}, "workflow"),
	}

	tests := []struct {
		name    string
		args    string
		wantErr bool
		because string
	}{
		{"valid", `{"workflow":"blog-generator"}`, false, ""},
		{"all fields", `{"workflow":"release-notes","dryRun":true,"limit":3}`, false, ""},
		{
			"missing required", `{"dryRun":true}`, true,
			"the most common model error, and the one most likely to be silently defaulted",
		},
		{
			"not in the enum", `{"workflow":"deploy-to-prod"}`, true,
			"a model will confidently pick a value that is NEARLY one of the options",
		},
		{
			"string where an integer belongs", `{"workflow":"blog-generator","limit":"3"}`, true,
			"a model asked for an integer will eventually send you the string \"3\"",
		},
		{
			"a float where an integer belongs", `{"workflow":"blog-generator","limit":1.5}`, true, "",
		},
		{
			"invented argument", `{"workflow":"blog-generator","sudo":true}`, true,
			"a model that invents an argument has misunderstood the tool",
		},
		{"not an object", `"just a string"`, true, ""},
		{"not even JSON", `{oh dear`, true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateArguments(spec, json.RawMessage(tt.args))
			if tt.wantErr && !errors.Is(err, ErrSchemaViolation) {
				t.Fatalf("ValidateArguments() = %v, want ErrSchemaViolation (%s)", err, tt.because)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateArguments() = %v, want nil", err)
			}
		})
	}
}

// The error has to be specific enough for the model to act on. "Invalid arguments" is not
// a repair instruction; it is a shrug.
func TestTheValidationErrorNamesTheProblem(t *testing.T) {
	spec := ToolSpec{
		Name:   "run_workflow",
		Schema: Object(map[string]any{"workflow": String("which", "blog-generator")}, "workflow"),
	}

	err := ValidateArguments(spec, json.RawMessage(`{"workflow":"blog-generatorr"}`))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"blog-generatorr", "blog-generator"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must show what was sent AND what was allowed; got %q", err)
		}
	}
}
