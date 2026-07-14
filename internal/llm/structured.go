package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Schema describes the shape a structured answer must take.
//
// The JSON Schema is sent to the model. The Go type is what the platform actually
// enforces — see [Structured], and the note there about which of those two you should
// trust.
type Schema struct {
	// Name is what the schema is called. The model sees it, so it should read like a
	// thing rather than a type: "release_notes", not "ReleaseNotesDTO".
	Name string

	// Description tells the model what it is filling in. Worth writing properly: it is
	// the difference between a summary and a summary of the right thing.
	Description string

	// Definition is the JSON Schema — an object schema, as built by [Object].
	Definition map[string]any
}

// Validator is implemented by a result type that can check its own semantics.
//
// The schema can say "priority is a string". It cannot say "a critical finding must cite
// a file", and it certainly cannot say "the total must equal the sum of the parts". Those
// are the checks that catch a model which produced perfectly-shaped, entirely wrong JSON,
// and they are the ones worth writing.
type Validator interface {
	Validate() error
}

// DefaultRepairAttempts is how many times a schema violation is handed back to the model.
//
// One. Not zero, because a single "you sent a string where an integer belongs" is very
// often enough — models are good at fixing what you name precisely. Not three, because
// each repair re-sends the whole conversation, and a model that failed the schema twice is
// not going to be talked round on the third go; it has misunderstood the task, and the
// prompt is what needs fixing, not the retry count.
const DefaultRepairAttempts = 1

// Structured runs an inference whose answer must satisfy a schema, and unmarshals it.
//
// # The two lines of defence, and which one is real
//
//  1. The JSON Schema is sent to the model. This is ADVICE. It makes a well-shaped answer
//     much more likely and it guarantees nothing at all.
//  2. The answer is unmarshalled into T with unknown fields DISALLOWED, and then, if T
//     implements [Validator], validated. This is ENFORCEMENT, and it is the only reason
//     any of the code downstream can trust what it is holding.
//
// The distinction is not pedantic. The output of a language model is untrusted input — the
// same position Milestone 6 took about an agent's output, and for the same reason: it was
// produced by something that is trying to be plausible, from content that may itself be
// hostile. A prompt injected into a diff can ask the model to fill this struct with
// anything at all, and the model will oblige. What stops that being a problem is not the
// schema. It is the type on the other side, and what the caller does with it.
//
// # Why it is implemented as a forced tool call
//
// On Bedrock, the way to make a model return an object is to give it exactly one tool
// whose input schema IS the object, and force it to call that tool. So structured output
// is tool use, wearing a hat — which is why [Capabilities.StructuredOutput] tracks
// [Capabilities.Tools] in practice, and why a provider that cannot do one cannot do the
// other.
func Structured[T any](ctx context.Context, s *Service, req Request, schema Schema) (T, Response, error) {
	var zero T

	if !s.provider.Capabilities().StructuredOutput {
		// The failure this prevents: a small model given a schema does not refuse. It
		// produces something JSON-shaped, with the right keys and invented values, and it
		// does so with total confidence. Nothing anywhere reports an error and the answer
		// is fiction.
		return zero, Response{}, fmt.Errorf("%w: %s cannot be held to a schema. It would not "+
			"refuse — it would return confident, well-formed, invented JSON — so the platform "+
			"will not ask. Use a provider with StructuredOutput capability",
			ErrUnsupported, s.provider.Name())
	}

	if schema.Name == "" || len(schema.Definition) == 0 {
		return zero, Response{}, fmt.Errorf("%w: a structured request needs a named schema", ErrInvalidRequest)
	}

	// One tool, and the model is forced to call it. It cannot answer in prose because the
	// only move available to it is to fill in the form.
	spec := ToolSpec{
		Name:        schema.Name,
		Description: schema.Description,
		Schema:      schema.Definition,
		Effect:      Read, // Filling in a form changes nothing.
	}

	req.Tools = []ToolSpec{spec}
	req.ToolChoice = schema.Name

	messages := req.Messages
	if len(messages) == 0 {
		messages = []Message{{Role: RoleUser, Content: req.Prompt}}
	}

	var last error

	for attempt := 0; attempt <= DefaultRepairAttempts; attempt++ {
		turn := req
		turn.Prompt = ""
		turn.Messages = messages

		res, err := s.Generate(ctx, turn)
		if err != nil {
			return zero, res, err
		}
		if len(res.ToolCalls) == 0 {
			// Forced tool choice and no tool call. The provider ignored ToolChoice, which
			// means the platform's assumption about it is wrong — and quietly parsing the
			// prose instead would hide that.
			return zero, res, fmt.Errorf("%w: the model was required to answer with %s and "+
				"answered in prose instead", ErrInvalidResponse, schema.Name)
		}

		raw := res.ToolCalls[0].Arguments
		value, err := decodeStrict[T](raw)
		if err == nil {
			return value, res, nil
		}
		last = err

		if attempt == DefaultRepairAttempts {
			break
		}

		// Hand the violation back. Naming the exact problem is what makes this work: "the
		// JSON was invalid" gets you a different invalid answer, and "priority must be one
		// of low, medium, high and you sent 'urgent'" gets you a correct one.
		s.log.Warn("the model's structured output did not fit; asking it to repair",
			"schema", schema.Name, "attempt", attempt+1, "error", err, "errorKind", Kind(err))

		messages = append(messages,
			Message{Role: RoleAssistant, ToolCalls: res.ToolCalls, Reasoning: res.Reasoning},
			Message{Role: RoleUser, ToolResults: []ToolResult{{
				ID:      res.ToolCalls[0].ID,
				Name:    schema.Name,
				IsError: true,
				Content: "That did not match the schema: " + err.Error() +
					". Call the tool again, correcting exactly that.",
			}}},
		)
	}

	return zero, Response{}, last
}

// decodeStrict unmarshals with unknown fields disallowed, then runs the type's own
// semantic check if it has one.
//
// DisallowUnknownFields matters more than it looks. A model that invents a field has
// misunderstood the task, and the invented field is a signal — silently dropping it (which
// is encoding/json's default) throws away the evidence and leaves you with a struct that
// is *missing* something rather than one that is *wrong*, which is far harder to debug.
func decodeStrict[T any](raw json.RawMessage) (T, error) {
	var value T

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&value); err != nil {
		return value, fmt.Errorf("%w: %v", ErrSchemaViolation, err)
	}

	if v, ok := any(value).(Validator); ok {
		if err := v.Validate(); err != nil {
			// The JSON was well-formed and the CONTENT is wrong. This is the check the
			// schema could never have made, and the one worth having.
			return value, fmt.Errorf("%w: %v", ErrSchemaViolation, err)
		}
	}
	return value, nil
}
