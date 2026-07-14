package format

import (
	"errors"
	"strings"
	"testing"
)

// Models wrap things. Asked for JSON, a model says "Here is the JSON you asked for:" and
// then a fenced block and then "Let me know if you'd like changes!" — all helpful, none of
// it parses. A pipeline that assumes the model is never chatty breaks the first time it is.
func TestCleanUnwrapsWhatTheModelActuallySaid(t *testing.T) {
	raw := "Here is the JSON you asked for:\n\n```json\n{\"severity\":\"low\"}\n```\n\nLet me know if you'd like me to change anything!"

	got := Clean(JSON, raw)
	if got != `{"severity":"low"}` {
		t.Errorf("Clean() = %q, want just the artefact", got)
	}
	if err := Validate(JSON, got); err != nil {
		t.Errorf("the unwrapped artefact should be valid: %v", err)
	}
}

// Markdown is the exception, and getting this wrong would be silently destructive: a
// Markdown document may legitimately CONTAIN fenced code blocks, so unwrapping would return
// the first code sample instead of the document.
func TestCleanDoesNotUnwrapMarkdown(t *testing.T) {
	doc := "# Title\n\nSome prose.\n\n```go\nfmt.Println(\"hi\")\n```\n\nMore prose."

	got := Clean(Markdown, doc)
	if !strings.Contains(got, "# Title") || !strings.Contains(got, "More prose.") {
		t.Errorf("Clean(Markdown) threw away the document and kept a code sample:\n%s", got)
	}
}

func TestValidateJSON(t *testing.T) {
	if err := Validate(JSON, `{"a":1}`); err != nil {
		t.Errorf("valid JSON rejected: %v", err)
	}
	// The classic: a model writes JSON the way a human writes a list.
	err := Validate(JSON, `{"a":1,}`)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("a trailing comma must be caught here, not by whatever parses it next: %v", err)
	}
}

func TestValidateYAML(t *testing.T) {
	valid := "name: blog-generator\nsteps:\n  - summarise\n  - publish\n"
	if err := Validate(YAML, valid); err != nil {
		t.Errorf("valid YAML rejected: %v", err)
	}

	// The single most common way a model produces YAML that is right in its head and wrong
	// in a parser. The error must name the actual problem, or the model cannot fix it.
	tabbed := "name: blog\nsteps:\n\t- summarise\n"
	err := Validate(YAML, tabbed)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a tab-indented YAML must be rejected: %v", err)
	}
	if !strings.Contains(err.Error(), "TAB") {
		t.Errorf("the error must name the tab: %v", err)
	}

	if err := Validate(YAML, "just: [unclosed\n"); !errors.Is(err, ErrInvalid) {
		t.Errorf("malformed YAML must be rejected: %v", err)
	}
}

// THE Mermaid tests. Every one of these is a bug that actually broke a diagram in this
// repository — and every one of them fails SILENTLY, as a red error box on a rendered page
// that nobody sees until a reader does.
func TestValidateMermaid(t *testing.T) {
	tests := []struct {
		name    string
		diagram string
		wantErr bool
		because string
	}{
		{
			"valid flowchart",
			"flowchart TB\n    a[\"start\"] --> b[\"end of it\"]\n",
			false, "",
		},
		{
			"valid sequence diagram",
			"sequenceDiagram\n    A->>B: hello\n    Note over A: thinking\n",
			false, "",
		},
		{
			"no diagram type",
			"    a[\"start\"] --> b[\"finish\"]\n",
			true, "without a type declaration it renders as nothing at all",
		},
		{
			"a reserved word as a node ID",
			"flowchart TB\n    call[\"bedrock:InvokeModel\"] --> gate1{\"IAM?\"}\n",
			true, "`call` is tokenised as CALLBACKNAME — this exact bug shipped in a diagram in this repo",
		},
		{
			"another reserved word",
			"flowchart TB\n    end[\"finish\"] --> a[\"x\"]\n",
			true, "`end` closes a block",
		},
		{
			"a semicolon inside a sequence Note",
			"sequenceDiagram\n    A->>B: hi\n    Note over A: slow is healthy; silent is not\n",
			true, "a semicolon ends the statement, and the rest of the note becomes a syntax error",
		},
		{
			"unbalanced bracket",
			"flowchart TB\n    a[\"start\" --> b[\"finish\"]\n",
			true, "",
		},
		{
			"unclosed quote",
			"flowchart TB\n    a[\"start] --> b[\"finish\"]\n",
			true, "",
		},
		{
			"a semicolon elsewhere is fine",
			"flowchart TB\n    a[\"one; two\"] --> b[\"three\"]\n",
			false, "only a sequenceDiagram Note is affected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(Mermaid, tt.diagram)
			if tt.wantErr && !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() = %v, want ErrInvalid (%s)", err, tt.because)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// The error has to tell the model what to do, not merely that it failed.
func TestTheMermaidErrorNamesTheReservedWord(t *testing.T) {
	err := Validate(Mermaid, "flowchart TB\n    call[\"x\"] --> b[\"y\"]\n")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "call") || !strings.Contains(err.Error(), "Rename") {
		t.Errorf("the error must name the word AND say what to do: %v", err)
	}
}

func TestValidateTable(t *testing.T) {
	good := "| a | b |\n| --- | --- |\n| 1 | 2 |\n"
	if err := Validate(Table, good); err != nil {
		t.Errorf("a valid table was rejected: %v", err)
	}

	// The table failure that matters: it RENDERS. Slightly wrong. Forever.
	ragged := "| a | b |\n| --- | --- |\n| 1 | 2 | 3 |\n"
	err := Validate(Table, ragged)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a ragged row must be caught: %v", err)
	}
	if !strings.Contains(err.Error(), "misaligned") {
		t.Errorf("the error should say what the consequence is: %v", err)
	}

	if err := Validate(Table, "| a | b |\n| 1 | 2 |\n"); !errors.Is(err, ErrInvalid) {
		t.Error("a table with no separator row renders as plain text and must be rejected")
	}
}

// Markdown validation is deliberately thin — almost anything is valid Markdown. What it
// catches is the one thing that is genuinely broken: an unclosed fence swallows the rest of
// the document, invisibly, until it is rendered.
func TestValidateMarkdown(t *testing.T) {
	if err := Validate(Markdown, "# Title\n\nProse.\n\n```go\nx := 1\n```\n"); err != nil {
		t.Errorf("valid Markdown rejected: %v", err)
	}
	if err := Validate(Markdown, "# Title\n\n```go\nx := 1\n"); !errors.Is(err, ErrInvalid) {
		t.Error("an unclosed code fence must be caught — it swallows the rest of the document")
	}
}

// An empty artefact is a failed generation, not an empty one.
func TestEmptyIsInvalid(t *testing.T) {
	for _, k := range []Kind{JSON, YAML, Mermaid, Table, Markdown} {
		if err := Validate(k, "   \n  "); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: empty output must be a failure", k)
		}
	}
	// Except Text, which is unvalidated by definition — and is named so that "unvalidated"
	// is a decision somebody made rather than a default nobody noticed.
	if err := Validate(Text, ""); err != nil {
		t.Errorf("Text is unvalidated by definition: %v", err)
	}
}

func TestParse(t *testing.T) {
	if k, err := Parse("MERMAID"); err != nil || k != Mermaid {
		t.Errorf("Parse(MERMAID) = %v, %v", k, err)
	}
	err := Parse2("xml")
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown format must be refused: %v", err)
	}
	if !strings.Contains(err.Error(), "mermaid") {
		t.Errorf("the error should list what IS known: %v", err)
	}
}

func Parse2(s string) error { _, err := Parse(s); return err }
