package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeFormatter stands in for internal/format. The Service must be testable without it —
// if it were not, llm would have to know what YAML is, which is exactly the coupling the
// Formatter interface exists to prevent.
type fakeFormatter struct {
	name  string
	valid func(string) bool
}

func (f fakeFormatter) Name() string { return f.name }

func (f fakeFormatter) Clean(raw string) string {
	// The real one unwraps fenced blocks; this one just trims, which is enough to prove the
	// Service calls Clean before Validate.
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "Here you go:"))
}

func (f fakeFormatter) Validate(content string) error {
	if f.valid(content) {
		return nil
	}
	return errors.New("line 3 is indented with a TAB")
}

func yamlish() fakeFormatter {
	return fakeFormatter{
		name:  "yaml",
		valid: func(s string) bool { return !strings.Contains(s, "\t") },
	}
}

func TestComposeReturnsAValidArtefact(t *testing.T) {
	provider := capableProvider(answerTurn("Here you go:\nname: blog-generator\n"))
	svc, _ := newService(provider)

	got, _, err := svc.Compose(context.Background(), Request{
		Prompt: "Write the workflow config.", CorrelationID: "c",
	}, yamlish())
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	// Cleaned: the model's chatty preamble is gone.
	if strings.Contains(got, "Here you go") {
		t.Errorf("Compose returned the model's commentary as part of the artefact: %q", got)
	}
	if got != "name: blog-generator" {
		t.Errorf("got %q", got)
	}
}

// THE test of this file. A generation that produced invalid YAML is a FAILED generation, and
// the model is shown its own mistake while there is still context to fix it.
//
// The alternative — return the text and let a caller find out later — pushes the failure into
// a component that did nothing wrong, at a time when the model is no longer around.
func TestAnInvalidArtefactIsHandedBackToTheModelWithTheReason(t *testing.T) {
	provider := capableProvider(
		answerTurn("name: blog\nsteps:\n\t- summarise\n"), // a tab: not valid YAML
		answerTurn("name: blog\nsteps:\n  - summarise\n"), // repaired
	)
	svc, logs := newService(provider)

	got, _, err := svc.Compose(context.Background(), Request{
		Prompt: "Write it.", CorrelationID: "c",
	}, yamlish())
	if err != nil {
		t.Fatalf("the repair should have succeeded: %v", err)
	}
	if strings.Contains(got, "\t") {
		t.Error("the invalid artefact was returned")
	}

	// The model was told the ACTUAL problem. "Invalid YAML" gets a different invalid answer;
	// "line 3 is indented with a TAB" gets a correct one.
	second := provider.got[1]
	last := second.Messages[len(second.Messages)-1]
	if !strings.Contains(last.Content, "TAB") {
		t.Errorf("the model must be told what was wrong: %q", last.Content)
	}
	if !strings.Contains(last.Content, "ONLY") {
		t.Errorf("the repair prompt must ask for the artefact alone, or the model will "+
			"apologise in prose and break it again: %q", last.Content)
	}

	// And the artefact itself is never logged: it is derived from the prompt, and the prompt
	// is repository content.
	if strings.Contains(logs.String(), "blog-generator") || strings.Contains(logs.String(), "summarise") {
		t.Error("the model's output reached the logs")
	}
}

// Repair is bounded. A model that has produced invalid YAML twice has misunderstood the
// task, and a third attempt spends money on the same misunderstanding.
func TestComposeRepairIsBounded(t *testing.T) {
	bad := answerTurn("name: blog\n\tbroken\n")
	provider := capableProvider(bad, bad, bad, bad)
	svc, _ := newService(provider)

	_, _, err := svc.Compose(context.Background(), Request{
		Prompt: "Write it.", CorrelationID: "c",
	}, yamlish())

	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("want ErrInvalidResponse, got %v", err)
	}
	if provider.calls != 1+DefaultFormatRepairs {
		t.Errorf("calls = %d, want 1 attempt + %d repair(s) — each one re-sends the whole "+
			"conversation and is billed for it", provider.calls, DefaultFormatRepairs)
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("the error should name the format that could not be produced: %v", err)
	}
}
