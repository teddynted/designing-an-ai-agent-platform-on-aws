package changelog

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Entry is a commit classified by the Conventional Commits grammar.
type Entry struct {
	Commit Commit

	// Type is the lower-cased commit type, such as "feat". It is empty when the
	// subject does not follow the convention.
	Type string
	// Scope is the optional parenthesised scope, such as "cli".
	Scope string
	// Subject is the description exactly as written, with the type, scope, and
	// "!" removed. Use Title for a form suitable for release notes.
	Subject string

	// Breaking is set by a "!" before the colon or a BREAKING CHANGE footer.
	Breaking bool
	// BreakingNote is the text of the BREAKING CHANGE footer, if any.
	BreakingNote string
}

// header matches "type(scope)!: description". The scope and the "!" are
// optional; the specification requires whitespace after the colon.
var header = regexp.MustCompile(`^([a-zA-Z]+)(?:\(([^()]+)\))?(!)?:[ \t]+(.+)$`)

// breakingPrefixes are the two footer spellings the specification allows.
var breakingPrefixes = []string{"BREAKING CHANGE:", "BREAKING-CHANGE:"}

// Parse classifies a single commit. It never fails: a subject that does not
// match the grammar yields an Entry with an empty Type, which the categories
// file under "Other Changes".
func Parse(c Commit) Entry {
	e := Entry{Commit: c, Subject: strings.TrimSpace(c.Subject)}

	if m := header.FindStringSubmatch(e.Subject); m != nil {
		e.Type = strings.ToLower(m[1])
		e.Scope = m[2]
		e.Breaking = m[3] == "!"
		e.Subject = m[4]
	}
	if note := breakingNote(c.Body); note != "" {
		e.Breaking = true
		e.BreakingNote = note
	}
	return e
}

// ParseAll classifies every commit, preserving order.
func ParseAll(commits []Commit) []Entry {
	entries := make([]Entry, 0, len(commits))
	for _, c := range commits {
		entries = append(entries, Parse(c))
	}
	return entries
}

// Title renders the subject the way release notes should read: the Conventional
// Commit prefix is already gone, the first letter is capitalised, and a
// trailing full stop is dropped.
//
// So "feat: add semantic versioning." becomes "Add semantic versioning".
func (e Entry) Title() string { return Title(e.Subject) }

// Title formats a bare commit description for display. It is exported so that
// custom templates can apply the same rules to their own strings.
func Title(subject string) string {
	s := strings.TrimSpace(subject)

	// Drop a single trailing full stop, but leave an ellipsis intact.
	if strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "..") {
		s = strings.TrimSuffix(s, ".")
	}
	return capitalise(s)
}

// capitalise upper-cases the first rune, unless doing so would corrupt an
// identifier that deliberately starts lower-case, such as "gRPC" or "iOS".
func capitalise(s string) string {
	first, size := utf8.DecodeRuneInString(s)
	if size == 0 || !unicode.IsLower(first) {
		return s
	}
	if rest := s[size:]; rest != "" {
		if second, _ := utf8.DecodeRuneInString(rest); unicode.IsUpper(second) {
			return s
		}
	}
	return string(unicode.ToUpper(first)) + s[size:]
}

// breakingNote extracts the BREAKING CHANGE footer, folding any continuation
// lines into a single paragraph. The footer must begin a line: prose that
// merely mentions the phrase is not a footer.
func breakingNote(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range breakingPrefixes {
			if !strings.HasPrefix(trimmed, prefix) {
				continue
			}
			parts := []string{strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))}
			for _, next := range lines[i+1:] {
				if strings.TrimSpace(next) == "" {
					break
				}
				parts = append(parts, strings.TrimSpace(next))
			}
			return strings.TrimSpace(strings.Join(parts, " "))
		}
	}
	return ""
}
