package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Effect says whether running a tool changes anything outside this process.
//
// It is the most important field in this file, and it is the reason Milestone 9 had to
// go back and correct Milestone 7.
//
// For two milestones the platform's position on retrying an inference was: it is safe.
// Generation reads a prompt and produces tokens; it has no side effects; the worst case
// of a retry is that you pay for the compute twice. That was true, and it is exactly the
// kind of comfortable assumption that a new capability quietly invalidates.
//
// Tool use invalidates it. The moment a model can call `run_workflow`, an inference can
// trigger an n8n run, open a pull request, and spend money — and "just retry it" becomes
// "run the workflow twice", which is the failure Milestone 5 spent an entire milestone
// learning to avoid.
//
// So the platform makes the distinction explicit, at the point of registration, rather
// than discovering it in an invoice:
//
//   - [Read] tools may be retried freely. They answer questions.
//   - [Write] tools may not. They change the world, and the world does not roll back.
//
// The model is NOT told which is which. Its opinion about what is dangerous is not a
// security control — see [ToolSpec].
type Effect string

const (
	// Read tools observe. Listing workflows, reading a diff, fetching a status.
	Read Effect = "read"

	// Write tools act. Triggering a workflow, submitting an agent task.
	//
	// A Write tool must be idempotent on its [ToolCall.IdempotencyKey], because the
	// platform's promise is "at most once per key", not "exactly once ever" — the same
	// discipline as Milestones 5 and 6, for the same reason.
	Write Effect = "write"
)

// ToolSpec is a tool, as described TO the model.
//
// # What the model is told, and what it is not
//
// Name, Description and Schema are sent. [ToolSpec.Effect] is not: it is platform
// metadata, used to decide retryability and to gate authorisation, and it stays here.
//
// Telling the model "this one is dangerous" would be worse than useless. It would read
// as a suggestion, it would be obeyed exactly as often as the model felt like it, and it
// would create the impression that the model was enforcing something. **A model's
// judgement is not an authorisation boundary.** The registry is.
type ToolSpec struct {
	// Name is what the model calls. Stable, snake_case, and part of the platform's
	// public surface: renaming one is a breaking change to every prompt that mentions it.
	Name string

	// Description is the single highest-leverage string in a tool-using system.
	//
	// It is the only thing the model reads when deciding whether this tool is the right
	// one, and a vague description produces a model that calls the wrong tool
	// confidently. Say what it does, when to use it, and — the part everyone omits —
	// when NOT to.
	Description string

	// Schema is the JSON Schema of the arguments: an object schema, with "properties"
	// and "required".
	//
	// It is sent to the model as guidance and enforced by the platform as a contract.
	// Those are different jobs and only the second one is trustworthy: a model asked for
	// an integer will, eventually, send you the string "3".
	Schema map[string]any

	// Effect decides whether a failure after this tool ran can be retried. It is never
	// sent to the model.
	Effect Effect
}

// ToolCall is the model asking for a tool to be run.
type ToolCall struct {
	// ID is the provider's identifier for this call. It must be echoed back with the
	// result, or the model cannot tell which answer belongs to which question — and a
	// model that mismatches its tool results produces reasoning that is subtly,
	// untraceably wrong.
	ID string

	Name string

	// Arguments are the model's JSON. UNTRUSTED, and not yet validated against the
	// schema: this is what the model *said*, not what the platform has agreed to do.
	Arguments json.RawMessage

	// IdempotencyKey is derived — never random — from the correlation ID, the tool name
	// and a hash of the arguments. See [DeriveIdempotencyKey].
	IdempotencyKey string
}

// ToolResult is what the platform hands back to the model.
type ToolResult struct {
	// ID must equal the [ToolCall.ID] it answers.
	ID   string
	Name string

	// Content is what the model reads. It is a string because that is what a model can
	// read; structured results are JSON-encoded into it.
	Content string

	// IsError reports that the tool failed.
	//
	// A failed tool is NOT a failed inference. It is a fact the model is entitled to
	// know, and a good model recovers from it — it tries different arguments, or it says
	// it cannot do the thing. Hiding tool errors, or aborting the loop on the first one,
	// produces a system that is far more brittle than the model it is built on.
	IsError bool
}

// ToolRunner is the platform's side of tool use: what tools exist, and how to run one.
//
// It is an interface declared HERE, in llm, and implemented in internal/tools — the same
// dependency inversion as [Provider]. This package must never learn what a workflow is.
// It knows only that some tools are Write tools, and that this changes what it is allowed
// to do when something fails.
type ToolRunner interface {
	// Specs lists the tools the model may call, in a stable order.
	Specs() []ToolSpec

	// Run executes one call. The call's arguments have ALREADY been validated against
	// the tool's schema by the loop, so an implementation may trust their shape — and
	// must still not trust their content.
	Run(ctx context.Context, call ToolCall) (ToolResult, error)
}

// Lookup finds a spec by name.
func Lookup(specs []ToolSpec, name string) (ToolSpec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return s, true
		}
	}
	return ToolSpec{}, false
}

// DeriveIdempotencyKey builds the key a Write tool deduplicates on.
//
// Derived, never random — which is the whole point, and the lesson of Milestone 5. A
// random key makes every retry a new request and therefore guarantees the double-run it
// was supposed to prevent. This key is a pure function of (what caused this, which tool,
// with what arguments), so the same tool call, retried, is the same key, and the far side
// can recognise it as a repeat.
func DeriveIdempotencyKey(correlationID, name string, args json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(correlationID))
	h.Write([]byte{0})
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(canonicalJSON(args))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// canonicalJSON re-encodes JSON with sorted keys, so that two semantically identical
// argument objects that differ only in key order produce the SAME idempotency key.
//
// Models do not emit keys in a stable order. Without this, a retried tool call whose
// arguments are identical in every way that matters would hash differently and run twice,
// and the bug would be invisible, intermittent, and blamed on the network.
func canonicalJSON(raw json.RawMessage) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Not valid JSON. Hash the bytes as they came: a malformed call is still a call,
		// and it must still hash consistently.
		return raw
	}
	out, err := json.Marshal(canonical(v))
	if err != nil {
		return raw
	}
	return out
}

func canonical(v any) any {
	switch t := v.(type) {
	case map[string]any:
		// encoding/json marshals maps with sorted keys, so rebuilding the map is enough.
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = canonical(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = canonical(val)
		}
		return out
	default:
		return v
	}
}

// ValidateArguments checks the model's arguments against a tool's schema BEFORE the tool
// runs.
//
// This is not belt-and-braces, it is the load-bearing check. The schema is *advice* to the
// model and a *contract* for the platform: a model asked for an integer will eventually
// send `"3"`, or omit a required field, or invent one. If the tool implementation is the
// first thing to notice, then the tool implementation is the security boundary — and it
// was written by someone who assumed the arguments were already checked.
//
// It deliberately validates only what this platform's own schemas use — types, required
// fields, and enums. It is not a general JSON Schema engine, it does not pretend to be,
// and the Go type on the other side of the call is the second line of defence.
func ValidateArguments(spec ToolSpec, args json.RawMessage) error {
	if len(spec.Schema) == 0 {
		return nil
	}

	var got map[string]any
	if err := json.Unmarshal(args, &got); err != nil {
		return fmt.Errorf("%w: %s produced arguments that are not a JSON object: %v",
			ErrSchemaViolation, spec.Name, err)
	}

	props, _ := spec.Schema["properties"].(map[string]any)

	// Required fields. The most common model error by a wide margin, and the one most
	// likely to produce a plausible-looking wrong answer if the tool defaults it silently.
	if required, ok := spec.Schema["required"].([]string); ok {
		for _, key := range required {
			if _, present := got[key]; !present {
				return fmt.Errorf("%w: %s is missing required argument %q",
					ErrSchemaViolation, spec.Name, key)
			}
		}
	}

	// Unknown fields. A model that invents an argument has misunderstood the tool, and
	// running it anyway means running it with an intention we did not read.
	if props != nil {
		unknown := make([]string, 0)
		for key := range got {
			if _, ok := props[key]; !ok {
				unknown = append(unknown, key)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return fmt.Errorf("%w: %s was called with unknown argument(s): %s",
				ErrSchemaViolation, spec.Name, strings.Join(unknown, ", "))
		}
	}

	for key, value := range got {
		schema, ok := props[key].(map[string]any)
		if !ok {
			continue
		}
		if err := checkType(spec.Name, key, schema, value); err != nil {
			return err
		}
	}
	return nil
}

func checkType(tool, key string, schema map[string]any, value any) error {
	want, _ := schema["type"].(string)

	switch want {
	case "string":
		s, ok := value.(string)
		if !ok {
			return typeErr(tool, key, want, value)
		}
		// An enum is the one place a model's creativity is guaranteed to cost you: it
		// will confidently pick a value that is *nearly* one of the options.
		if raw, ok := schema["enum"].([]string); ok {
			for _, allowed := range raw {
				if s == allowed {
					return nil
				}
			}
			return fmt.Errorf("%w: %s.%s = %q, which is not one of: %s",
				ErrSchemaViolation, tool, key, s, strings.Join(raw, ", "))
		}
	case "integer":
		f, ok := value.(float64)
		if !ok || f != float64(int64(f)) {
			return typeErr(tool, key, want, value)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return typeErr(tool, key, want, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return typeErr(tool, key, want, value)
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return typeErr(tool, key, want, value)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return typeErr(tool, key, want, value)
		}
	}
	return nil
}

func typeErr(tool, key, want string, got any) error {
	return fmt.Errorf("%w: %s.%s should be %s, and the model sent %T (%v)",
		ErrSchemaViolation, tool, key, want, got, got)
}

// Object is a small helper for writing an argument schema without a wall of map literals.
func Object(properties map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

// String describes a string argument.
func String(description string, enum ...string) map[string]any {
	s := map[string]any{"type": "string", "description": description}
	if len(enum) > 0 {
		s["enum"] = enum
	}
	return s
}

// Integer describes an integer argument.
func Integer(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// Bool describes a boolean argument.
func Bool(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}
