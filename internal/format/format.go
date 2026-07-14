// Package format validates what a model produced, before anything downstream believes it.
//
// # Why this exists
//
// A model asked for YAML produces something that looks like YAML. A model asked for a
// Mermaid diagram produces something that looks like a Mermaid diagram. Both are *usually*
// right, and the failure mode when they are not is the one this platform keeps meeting:
// **not an error, but a plausible artefact that breaks something further downstream, later,
// somewhere else.**
//
//   - Invalid YAML written to a config file fails at deploy, not at generation.
//   - An invalid Mermaid diagram renders as a red error box in a blog post, and the first
//     person to notice is a reader.
//   - A JSON object with a trailing comma is a 500 in whatever parses it next.
//
// In every case the model has already been paid for, the log line says "inference
// completed", and the fault surfaces in a component that did nothing wrong.
//
// So: **validate at the boundary, where the model's output is still the model's output.**
// A generation that produced invalid YAML is a FAILED generation, and the platform says so
// while it still has the context to do something about it — which is usually to show the
// model its own mistake and ask again.
//
// # What this package is not
//
// It is not a renderer, and the Mermaid validator in particular is not a Mermaid parser.
// It catches the mistakes models actually make (see [validateMermaid]), and it will not
// catch every possible malformation. That is stated rather than hidden: a validator that
// over-promises is worse than one whose limits are written down, because people stop
// checking.
package format

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrInvalid means the model's output is not the format it was asked for.
var ErrInvalid = errors.New("the model's output is not valid for the requested format")

// Kind is a structured output format.
type Kind string

const (
	// JSON — for a machine. Use llm.Structured[T] when you want a typed value; use this
	// when you genuinely want JSON as an artefact.
	JSON Kind = "json"

	// YAML — for a config file, a CloudFormation fragment, an n8n workflow definition.
	YAML Kind = "yaml"

	// Markdown — for a document a human reads.
	Markdown Kind = "markdown"

	// Mermaid — for a diagram. The most fragile of the five, and the one most worth
	// validating: an invalid diagram does not fail, it renders as a red box on a page
	// somebody is reading.
	Mermaid Kind = "mermaid"

	// Table — a Markdown table. Structured enough that a downstream component may want
	// to parse it, which means it must be well-formed rather than merely pretty.
	Table Kind = "table"

	// Text — no structure, no validation. Named so that "unvalidated" is a decision
	// somebody made, rather than a default nobody noticed.
	Text Kind = "text"
)

// Kinds lists the formats that can be validated.
var Kinds = []Kind{JSON, YAML, Markdown, Mermaid, Table, Text}

// Parse turns a string into a Kind.
func Parse(s string) (Kind, error) {
	for _, k := range Kinds {
		if string(k) == strings.ToLower(strings.TrimSpace(s)) {
			return k, nil
		}
	}
	return "", fmt.Errorf("%w: unknown format %q (known: %s)", ErrInvalid, s, join(Kinds))
}

// fence matches a fenced code block, with or without a language tag.
var fence = regexp.MustCompile("(?s)```[a-zA-Z]*\\s*\\n(.*?)```")

// Clean extracts the artefact from what the model actually said.
//
// Models wrap things. Asked for JSON, a model returns "Here is the JSON you asked for:"
// followed by a fenced code block, followed by "Let me know if you'd like me to change
// anything!" — and all three of those are helpful, and none of them parse.
//
// Prompting can reduce this. It cannot eliminate it, and a pipeline that depends on the
// model never being chatty is a pipeline that breaks the first time it is.
//
// So Clean takes the first fenced block if there is one, and otherwise trims. For
// [Markdown] it does NOT unwrap, because a Markdown document may legitimately CONTAIN
// fenced code blocks, and unwrapping would return the first code sample instead of the
// document.
func Clean(kind Kind, raw string) string {
	out := strings.TrimSpace(raw)

	if kind == Markdown || kind == Text {
		return out
	}

	if m := fence.FindStringSubmatch(out); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return out
}

// Validate checks that the content really is what it claims to be.
func Validate(kind Kind, content string) error {
	content = strings.TrimSpace(content)

	if content == "" && kind != Text {
		return fmt.Errorf("%w: the model produced nothing", ErrInvalid)
	}

	switch kind {
	case JSON:
		return validateJSON(content)
	case YAML:
		return validateYAML(content)
	case Mermaid:
		return validateMermaid(content)
	case Table:
		return validateTable(content)
	case Markdown:
		return validateMarkdown(content)
	case Text:
		return nil
	default:
		return fmt.Errorf("%w: unknown format %q", ErrInvalid, kind)
	}
}

// CleanAndValidate is the pair, which is how it is almost always used.
func CleanAndValidate(kind Kind, raw string) (string, error) {
	content := Clean(kind, raw)
	if err := Validate(kind, content); err != nil {
		return content, err
	}
	return content, nil
}

func validateJSON(content string) error {
	var v any
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		// json.Unmarshal's error already names the offset and the token, which is exactly
		// what the model needs in order to fix it.
		return fmt.Errorf("%w: not valid JSON: %v", ErrInvalid, err)
	}
	return nil
}

func validateYAML(content string) error {
	// Tabs FIRST, before the parser gets a chance.
	//
	// yaml.v3 does catch a tab, and it reports it as "found character that cannot start any
	// token" on some line — which is true, and useless, and is not something a model can act
	// on. The whole value of validating at this boundary is that the error goes back to the
	// model, so the error has to name the actual mistake and the actual fix.
	for i, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "\t") {
			return fmt.Errorf("%w: line %d is indented with a TAB. YAML does not permit tabs "+
				"for indentation — use spaces", ErrInvalid, i+1)
		}
	}

	var v any
	if err := yaml.Unmarshal([]byte(content), &v); err != nil {
		return fmt.Errorf("%w: not valid YAML: %v", ErrInvalid, err)
	}
	if v == nil {
		// Valid YAML, and empty. `null` parses. It is not a config file.
		return fmt.Errorf("%w: the YAML is empty", ErrInvalid)
	}
	return nil
}

// mermaidTypes are the diagram declarations Mermaid understands.
var mermaidTypes = []string{
	"graph", "flowchart", "sequenceDiagram", "classDiagram", "stateDiagram",
	"stateDiagram-v2", "erDiagram", "journey", "gantt", "pie", "gitGraph",
	"mindmap", "timeline", "quadrantChart", "requirementDiagram", "C4Context",
	"sankey-beta", "xychart-beta", "block-beta",
}

// mermaidReserved are identifiers that Mermaid's flowchart grammar will not accept as node
// IDs, because they are keywords.
//
// This list is not theoretical. `call` is in it because a diagram in this repository used
// `call` as a node ID, and Mermaid tokenised it as CALLBACKNAME and failed to parse — which
// on GitHub renders as a red error box where the diagram should be, and which was found by
// rendering every diagram in CI rather than by reading them.
var mermaidReserved = []string{
	"call", "class", "click", "end", "graph", "style", "subgraph", "default",
	"linkStyle", "classDef", "direction",
}

var mermaidNodeID = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_-]*)\s*[\[({]`)

// validateMermaid catches the mistakes models (and humans) actually make.
//
// It is deliberately NOT a Mermaid parser. Writing one would be a project, and a
// half-written one is worse than a checklist, because it implies a completeness it does not
// have. What it does is check the four things that have actually broken a diagram in this
// repository — and every one of them fails silently, as a red box on a rendered page.
func validateMermaid(content string) error {
	lines := strings.Split(content, "\n")

	// 1. It must declare what kind of diagram it is. A model that forgets produces a block
	//    that renders as nothing at all.
	var declared string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "%%") {
			continue
		}
		for _, t := range mermaidTypes {
			if strings.HasPrefix(trimmed, t) {
				declared = t
			}
		}
		break
	}
	if declared == "" {
		return fmt.Errorf("%w: the diagram does not begin with a Mermaid diagram type "+
			"(flowchart, sequenceDiagram, …), so it will render as nothing", ErrInvalid)
	}

	// 2. Reserved words used as node IDs. Compiles, renders as a red error box.
	if strings.HasPrefix(declared, "flowchart") || strings.HasPrefix(declared, "graph") {
		for _, m := range mermaidNodeID.FindAllStringSubmatch(content, -1) {
			id := m[1]
			for _, reserved := range mermaidReserved {
				if id == reserved {
					return fmt.Errorf("%w: %q is a reserved word in Mermaid and cannot be a node "+
						"ID — the diagram will fail to parse. Rename the node", ErrInvalid, id)
				}
			}
		}
	}

	// 3. A semicolon inside a sequence-diagram Note terminates the statement, and everything
	//    after it becomes a parse error two lines later, pointing at innocent syntax.
	if declared == "sequenceDiagram" {
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "Note ") && strings.Contains(trimmed, ";") {
				return fmt.Errorf("%w: line %d — a semicolon inside a sequenceDiagram Note ends "+
					"the statement, and the rest of the note becomes a syntax error. Use a comma "+
					"or a dash", ErrInvalid, i+1)
			}
		}
	}

	// 4. Unbalanced brackets and quotes: the ordinary way a generated diagram is broken.
	if err := balanced(content); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// balanced checks brackets and quotes outside of quoted strings.
func balanced(content string) error {
	var stack []rune
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	inQuote := false

	for _, r := range content {
		if r == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch r {
		case '(', '[', '{':
			stack = append(stack, r)
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[r] {
				return fmt.Errorf("unbalanced %q", r)
			}
			stack = stack[:len(stack)-1]
		}
	}
	if inQuote {
		return errors.New(`an unclosed double quote`)
	}
	if len(stack) > 0 {
		return fmt.Errorf("unclosed %q", stack[len(stack)-1])
	}
	return nil
}

// validateTable checks a Markdown table is one a machine could parse.
//
// A table is the format most likely to be *nearly* right: a model will produce a beautiful
// table with one row that has an extra cell, and every renderer will display it, slightly
// wrong, forever.
func validateTable(content string) error {
	var rows [][]string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		rows = append(rows, cells(trimmed))
	}

	if len(rows) < 2 {
		return fmt.Errorf("%w: a Markdown table needs a header row and a separator row "+
			"(| --- | --- |)", ErrInvalid)
	}

	// The separator row is what makes it a table rather than a series of pipes.
	sep := rows[1]
	for _, c := range sep {
		if !strings.Contains(c, "-") {
			return fmt.Errorf("%w: the second row must be the separator (| --- | --- |), "+
				"and it is not — without it, this renders as plain text", ErrInvalid)
		}
	}

	want := len(rows[0])
	for i, row := range rows {
		if len(row) != want {
			return fmt.Errorf("%w: row %d has %d cells and the header has %d — the table will "+
				"render, misaligned, and nobody will notice for weeks", ErrInvalid, i+1, len(row), want)
		}
	}
	return nil
}

func cells(row string) []string {
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	parts := strings.Split(row, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// validateMarkdown is deliberately thin.
//
// Almost anything is valid Markdown — that is the point of Markdown — so a "validator" that
// pretended otherwise would be inventing rules. What it checks is the one thing that is
// actually broken rather than merely unusual: an unterminated code fence, which swallows the
// remainder of the document into a code block and is invisible until it is rendered.
func validateMarkdown(content string) error {
	if strings.Count(content, "```")%2 != 0 {
		return fmt.Errorf("%w: an unclosed code fence (```) — everything after it will be "+
			"swallowed into a code block when this is rendered", ErrInvalid)
	}
	return nil
}

func join(kinds []Kind) string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

// --- the llm.Formatter adapter ----------------------------------------------

// Formatter adapts a Kind to llm.Formatter.
//
// It lives here, not in llm, because llm must not learn what YAML is. The dependency points
// inward: internal/format implements an interface internal/llm declares, exactly as
// internal/ollama implements llm.Provider and internal/tools implements llm.ToolRunner.
type Formatter struct{ kind Kind }

// For returns a Formatter for a Kind.
func For(kind Kind) Formatter { return Formatter{kind: kind} }

// Name implements llm.Formatter.
func (f Formatter) Name() string { return string(f.kind) }

// Clean implements llm.Formatter.
func (f Formatter) Clean(raw string) string { return Clean(f.kind, raw) }

// Validate implements llm.Formatter.
func (f Formatter) Validate(content string) error { return Validate(f.kind, content) }
