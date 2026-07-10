package release

import "strings"

// Error is a release failure explained the way a person needs it: what went
// wrong, why, and what to do about it.
//
// It wraps a sentinel so that errors.Is keeps working, and formats itself as a
// complete message so that a caller can print it without knowing its structure.
type Error struct {
	// Cause is the sentinel this error wraps, such as ErrTagExists.
	Cause error
	// What states the failure in one sentence, ending in a full stop.
	What string
	// Why gives the supporting detail: the offending paths, the conflicting
	// tag, the underlying git error. It may be empty and may span lines.
	Why string
	// Solutions are the ways out, most likely first. Each is a short imperative
	// phrase, optionally naming the exact command to run.
	Solutions []string
}

// Error renders the full message: summary, detail, and remedies.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.What)

	if e.Why != "" {
		b.WriteString("\n\n")
		b.WriteString(e.Why)
	}
	if len(e.Solutions) > 0 {
		b.WriteString("\n\nPossible solutions:\n")
		for _, s := range e.Solutions {
			b.WriteString("\n• ")
			b.WriteString(s)
		}
	}
	return b.String()
}

// Unwrap exposes the sentinel, so callers keep classifying failures with
// errors.Is rather than by matching message text.
func (e *Error) Unwrap() error { return e.Cause }

// indent prefixes every line of s, for embedding a list inside Why.
func indent(lines []string) string {
	return "  " + strings.Join(lines, "\n  ")
}
