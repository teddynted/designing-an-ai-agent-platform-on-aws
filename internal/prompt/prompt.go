// Package prompt keeps the platform's prompts in version control, where they belong.
//
// # Why a package, rather than a string literal
//
// A prompt is not a string. It is the part of the system that decides what the model
// actually does, it is the thing most likely to change the output, and it is — in a
// platform that pays per token — the thing most likely to change the bill. Kept as a
// literal in the middle of a function, it has none of the properties that suggests:
//
//   - **It cannot be reviewed.** A prompt change is a behaviour change, and it should
//     arrive in a pull request looking like one, as a diff someone can read, and not
//     buried in a Go file among the error handling.
//   - **It cannot be identified.** When the output changes and nobody touched the model,
//     the first question is "which prompt produced that?" — and a literal cannot answer.
//     Every prompt here has a [Prompt.Version]: the first twelve hex characters of the
//     SHA-256 of its bytes. It goes into the log line next to the completion, so the
//     output and the instruction that produced it are joined up permanently.
//   - **It cannot be tested.** A template with a typo in a variable name renders an empty
//     string into the middle of an instruction, and the model does not complain — it just
//     does something slightly different, forever. So [Load] fails on an unknown variable
//     rather than silently rendering nothing.
//
// # The system prompt is not a suggestion box
//
// The prompts here are the PLATFORM speaking. Nothing in them is ever built from
// repository content, from a model's output, or from anything a user typed. Content goes
// into the *user* turn, where the model reads it as data; instructions come from here,
// where the model reads them as instructions. Milestone 6 drew that line for the agent,
// Milestone 9's tools defend it (see internal/tools), and this package is where the
// platform's half of it lives.
package prompt

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"text/template"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// ErrNotFound means there is no such prompt.
var ErrNotFound = errors.New("no such prompt")

// ErrRender means the prompt could not be rendered — almost always a variable the data
// does not have.
var ErrRender = errors.New("the prompt could not be rendered")

//go:embed templates/*/*.md
var files embed.FS

// Categories are the platform's prompt CAPABILITIES, and the directory layout is the
// taxonomy — there is no registry to fall out of sync with the files.
//
// Organising by capability rather than by caller is deliberate. A prompt named
// "blog-generator-step-3" belongs to one workflow and dies with it; a prompt named
// "summarisation/diff-summary" is a thing the platform can DO, and the next caller that
// needs a diff summarised will find it instead of writing a fifth one.
const (
	// Summarisation — diffs, commits, events. The platform's most common inference.
	Summarisation = "summarisation"

	// Structured — prompts whose answer is data, not prose. Paired with llm.Structured.
	Structured = "structured"

	// Architecture — explanations and diagrams.
	Architecture = "architecture"

	// Writing — technical documentation and long-form prose.
	Writing = "writing"

	// Workflow — system prompts for the tool-using assistant. These are the prompts that
	// govern a model which can ACT, and they are the ones to read most carefully.
	Workflow = "workflow"
)

// Prompt is one versioned instruction to a model.
type Prompt struct {
	// Name is the qualified name: "summarisation/diff-summary".
	Name string

	// Category is the capability it belongs to: "summarisation".
	//
	// It is logged on every inference as promptCategory, which is what makes "what is this
	// platform spending its tokens on?" answerable by CAPABILITY and not merely by caller —
	// the difference between "the blog workflow costs a lot" and "summarisation costs a
	// lot, everywhere, and that is where an optimisation would pay".
	Category string

	// Version is the first twelve hex characters of the SHA-256 of the template.
	//
	// Content-addressed, deliberately: a hand-maintained version number is a version
	// number somebody forgets to bump, and a prompt whose version says it is unchanged
	// while its text is not is worse than no version at all.
	Version string

	// Text is the raw template, before rendering.
	Text string

	tmpl *template.Template
}

// Load returns a prompt by name.
func Load(name string) (Prompt, error) {
	raw, err := files.ReadFile("templates/" + name + ".md")
	if err != nil {
		return Prompt{}, fmt.Errorf("%w: %q (available: %s)", ErrNotFound, name, strings.Join(Names(), ", "))
	}

	category, _, ok := strings.Cut(name, "/")
	if !ok {
		return Prompt{}, fmt.Errorf("%w: %q has no category — prompts are organised by "+
			"capability, e.g. %q", ErrNotFound, name, "summarisation/diff-summary")
	}

	text := strings.TrimSpace(string(raw))

	// Option("missingkey=error") is the whole reason this is a package and not a call to
	// os.ReadFile. Go's default is to render a missing variable as "<no value>" — so a
	// renamed field produces a prompt that reads "Summarise the following <no value>",
	// which the model will cheerfully do its best with, and nothing anywhere will report
	// a problem. Failing loudly is the only safe behaviour for a template that instructs
	// a model.
	tmpl, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return Prompt{}, fmt.Errorf("%w: %q does not parse: %v", ErrRender, name, err)
	}

	sum := sha256.Sum256([]byte(text))
	return Prompt{
		Name:     name,
		Category: category,
		Version:  hex.EncodeToString(sum[:])[:12],
		Text:     text,
		tmpl:     tmpl,
	}, nil
}

// MustLoad is Load, for a prompt that is a programming error to be missing.
func MustLoad(name string) Prompt {
	p, err := Load(name)
	if err != nil {
		panic(err)
	}
	return p
}

// Render fills the template in.
func (p Prompt) Render(data any) (string, error) {
	var b bytes.Buffer
	if err := p.tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("%w: %s (%s): %v", ErrRender, p.Name, p.Version, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// Names lists every prompt in the library.
func Names() []string {
	entries, err := fs.Glob(files, "templates/*/*.md")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(strings.TrimPrefix(e, "templates/"), ".md"))
	}
	sort.Strings(names)
	return names
}

// InCategory lists the prompts for one capability.
func InCategory(category string) []Prompt {
	var out []Prompt
	for _, name := range Names() {
		if !strings.HasPrefix(name, category+"/") {
			continue
		}
		if p, err := Load(name); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// All loads every prompt. It exists so a test can assert that they all parse — which is
// the cheapest possible defence against a typo in a template shipping to production and
// being discovered by a model doing something odd three weeks later.
func All() ([]Prompt, error) {
	var out []Prompt
	for _, name := range Names() {
		p, err := Load(name)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Apply renders a prompt and stamps its identity onto a request.
//
// It exists so that no caller ever has to remember to copy three fields by hand — and so
// that an inference whose prompt came from this library is ALWAYS traceable back to it. A
// prompt whose version reaches the model but not the log is a prompt you cannot debug.
func (p Prompt) Apply(req *llm.Request, data any) error {
	text, err := p.Render(data)
	if err != nil {
		return err
	}
	req.Prompt = text
	req.PromptName = p.Name
	req.PromptCategory = p.Category
	req.PromptVersion = p.Version
	return nil
}

// System renders a prompt into the request's SYSTEM field instead.
//
// The distinction matters and it is the platform's oldest security rule: the system prompt
// is the PLATFORM speaking, and the user turn is where content goes. Repository content must
// never be rendered into a system prompt — that is how a diff becomes an instruction.
func (p Prompt) System(req *llm.Request, data any) error {
	text, err := p.Render(data)
	if err != nil {
		return err
	}
	req.System = text
	req.PromptName = p.Name
	req.PromptCategory = p.Category
	req.PromptVersion = p.Version
	return nil
}
