package llm

import (
	"context"
	"fmt"
)

// Formatter validates a structured artefact the model produced.
//
// It is an interface declared HERE and implemented in internal/format, for the same reason
// [Provider] and [ToolRunner] are: this package must not learn what YAML is. It knows only
// that some outputs have a shape, that something can check the shape, and that a generation
// which produced the wrong shape is a **failed** generation.
//
// That last point is the whole design. The alternative — return the text and let a caller
// discover the YAML is malformed — pushes the failure into a component that did nothing
// wrong, at a time when the model is no longer around to fix it.
type Formatter interface {
	// Clean extracts the artefact from what the model actually said. Models wrap things:
	// "Here is the YAML you asked for:" is helpful, and it does not parse.
	Clean(raw string) string

	// Validate reports whether the content is genuinely the format that was asked for.
	Validate(content string) error

	// Name is what the format is called, for logs and for the message shown to the model.
	Name() string
}

// DefaultFormatRepairs is how many times an invalid artefact is handed back to the model.
//
// One, for the same reason as [DefaultRepairAttempts]: a model shown its own syntax error
// usually fixes it on the first go, and a model that has failed twice has misunderstood the
// task rather than mistyped it — so the *prompt* is what needs changing, not the retry
// count. Each repair re-sends the whole conversation and is billed for it.
const DefaultFormatRepairs = 1

// Compose generates an artefact in a given format, and does not return it unless it is
// valid.
//
// # The failure this prevents
//
// A model asked for YAML produces something that looks like YAML. Usually it is. When it is
// not, nothing fails: the log says "inference completed", the tokens are paid for, and the
// malformed config surfaces at deploy time, in a component that did nothing wrong, hours
// later. The same is true of a Mermaid diagram that renders as a red error box on a page
// somebody is reading, and of a Markdown table with one row that has an extra cell.
//
// So validation happens **at the boundary**, while the model's output is still the model's
// output and while there is still enough context to do the obvious thing about it — show
// the model its own mistake and ask again.
func (s *Service) Compose(ctx context.Context, req Request, f Formatter) (string, Response, error) {
	messages := req.Messages
	if len(messages) == 0 {
		messages = []Message{{Role: RoleUser, Content: req.Prompt}}
	}

	var last error

	for attempt := 0; attempt <= DefaultFormatRepairs; attempt++ {
		turn := req
		turn.Prompt = ""
		turn.Messages = messages

		res, err := s.Generate(ctx, turn)
		if err != nil {
			return "", res, err
		}

		content := f.Clean(res.Content)
		if err := f.Validate(content); err == nil {
			return content, res, nil
		} else {
			last = err
		}

		if attempt == DefaultFormatRepairs {
			break
		}

		s.log.Warn("the model produced an invalid artefact; asking it to repair",
			"format", f.Name(),
			"attempt", attempt+1,
			"error", last,
			"errorKind", Kind(last),
			// Not the artefact itself: it is derived from the prompt, and the prompt is
			// repository content.
			"outputChars", len(content),
		)

		// Naming the exact fault is what makes this work. "Invalid YAML" gets a different
		// invalid answer; "line 4 is indented with a TAB, and YAML does not permit tabs" gets
		// a correct one.
		messages = append(messages,
			Message{Role: RoleAssistant, Content: res.Content},
			Message{Role: RoleUser, Content: fmt.Sprintf(
				"That is not valid %s: %v\n\nReturn ONLY the corrected %s, with no commentary "+
					"and no surrounding prose.", f.Name(), last, f.Name())},
		)
	}

	return "", Response{}, fmt.Errorf("%w: the model could not produce valid %s after %d attempts: %v",
		ErrInvalidResponse, f.Name(), DefaultFormatRepairs+1, last)
}
