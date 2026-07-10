// Package changelog turns a range of commits into human-readable release
// notes and CHANGELOG.md entries.
//
// Commit subjects are interpreted as Conventional Commits
// (https://www.conventionalcommits.org/en/v1.0.0/). A subject that does not
// follow the convention is not discarded: it is filed under "Other Changes" so
// that nothing silently disappears from a release.
//
// The package operates on its own Commit type rather than on git.Commit, so
// that rendering stays independent of how commits were obtained.
package changelog

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// Commit is the subset of a Git commit that release notes are derived from.
type Commit struct {
	SHA         string
	Subject     string
	Body        string
	AuthorName  string
	AuthorEmail string
}

// Repository identifies the forge the release belongs to, so that entries can
// link to commits and comparisons. A zero Repository disables linking.
type Repository struct {
	Host  string
	Owner string
	Name  string
}

// known reports whether enough is known to build URLs.
func (r Repository) known() bool { return r.Host != "" && r.Owner != "" && r.Name != "" }

// CommitURL returns a permalink to a commit, or "" when the repository is
// unknown.
func (r Repository) CommitURL(sha string) string {
	if !r.known() || sha == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/%s/commit/%s", r.Host, r.Owner, r.Name, sha)
}

// CompareURL returns a diff link between two refs.
func (r Repository) CompareURL(from, to string) string {
	if !r.known() || from == "" || to == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/%s/compare/%s...%s", r.Host, r.Owner, r.Name, from, to)
}

// CommitsURL returns a link to the history leading up to a ref. It is the
// fallback for the very first release, which has nothing to compare against.
func (r Repository) CommitsURL(ref string) string {
	if !r.known() || ref == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/%s/commits/%s", r.Host, r.Owner, r.Name, ref)
}

// Release is everything needed to render one version's notes.
type Release struct {
	Tag         string
	Version     semver.Version
	PreviousTag string
	Date        time.Time
	Repo        Repository
	Commits     []Commit
}

// HistoryURL links to the changes this release contains: a comparison against
// the previous tag, or the full history for a first release.
func (r Release) HistoryURL() string {
	if url := r.Repo.CompareURL(r.PreviousTag, r.Tag); url != "" {
		return url
	}
	return r.Repo.CommitsURL(r.Tag)
}

// Entry is a commit classified by the Conventional Commits grammar.
type Entry struct {
	Commit Commit

	// Type is the lower-cased commit type, such as "feat". It is empty when the
	// subject does not follow the convention.
	Type string
	// Scope is the optional parenthesised scope, such as "cli".
	Scope string
	// Subject is the description, with the type, scope, and "!" removed.
	Subject string

	// Breaking is set by a "!" before the colon or a BREAKING CHANGE footer.
	Breaking bool
	// BreakingNote is the text of the BREAKING CHANGE footer, if any.
	BreakingNote string
}

// header matches "type(scope)!: description". The scope and the "!" are
// optional; the specification requires a space after the colon.
var header = regexp.MustCompile(`^([a-zA-Z]+)(?:\(([^()]+)\))?(!)?:[ \t]+(.+)$`)

// breakingPrefixes are the two footer spellings the specification allows.
var breakingPrefixes = []string{"BREAKING CHANGE:", "BREAKING-CHANGE:"}

// Parse classifies a single commit. It never fails: a subject that does not
// match the grammar yields an Entry with an empty Type.
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

// breakingNote extracts the BREAKING CHANGE footer, folding any continuation
// lines into a single paragraph.
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

// Section is one heading in the rendered output, fed by one or more commit
// types. A Section whose Types contains "" also collects commits whose type
// matches no other section.
type Section struct {
	Title  string
	Types  []string
	Hidden bool
}

// DefaultSections is the standard layout: the changes a consumer of the
// software cares about are shown, and housekeeping is hidden.
//
// Hidden sections are still parsed, so enabling one is a configuration change
// rather than a code change.
func DefaultSections() []Section {
	return []Section{
		{Title: "Features", Types: []string{"feat"}},
		{Title: "Bug Fixes", Types: []string{"fix"}},
		{Title: "Performance Improvements", Types: []string{"perf"}},
		{Title: "Reverts", Types: []string{"revert"}},
		{Title: "Documentation", Types: []string{"docs"}},
		{Title: "Code Refactoring", Types: []string{"refactor"}},
		{Title: "Other Changes", Types: []string{""}},
		{Title: "Build System", Types: []string{"build"}, Hidden: true},
		{Title: "Continuous Integration", Types: []string{"ci"}, Hidden: true},
		{Title: "Tests", Types: []string{"test"}, Hidden: true},
		{Title: "Styles", Types: []string{"style"}, Hidden: true},
		{Title: "Chores", Types: []string{"chore"}, Hidden: true},
	}
}

// claimedTypes is the set of commit types that some section names explicitly.
func claimedTypes(sections []Section) map[string]bool {
	claimed := make(map[string]bool)
	for _, s := range sections {
		for _, t := range s.Types {
			claimed[t] = true
		}
	}
	return claimed
}

// classified returns the entries belonging to a section, preserving order.
// Entries whose type is claimed by no section fall through to the catch-all
// section, the one that declares the empty type.
func classified(entries []Entry, section Section, claimed map[string]bool) []Entry {
	wants := make(map[string]bool, len(section.Types))
	catchAll := false
	for _, t := range section.Types {
		wants[t] = true
		if t == "" {
			catchAll = true
		}
	}

	var out []Entry
	for _, e := range entries {
		if wants[e.Type] || (catchAll && !claimed[e.Type]) {
			out = append(out, e)
		}
	}
	return out
}

// RenderNotes renders the body of a GitHub Release.
func RenderNotes(rel Release, sections []Section) string {
	var b strings.Builder
	b.WriteString(body(rel, sections))

	if url := rel.HistoryURL(); url != "" {
		label := "Full Changelog"
		if rel.PreviousTag == "" {
			label = "Commit history"
		}
		fmt.Fprintf(&b, "\n**%s**: %s\n", label, url)
	}
	return b.String()
}

// RenderEntry renders one version's section of CHANGELOG.md, in the
// Keep a Changelog layout.
func RenderEntry(rel Release, sections []Section) string {
	var b strings.Builder

	version := rel.Version.String()
	if url := rel.HistoryURL(); url != "" {
		fmt.Fprintf(&b, "## [%s](%s) - %s\n\n", version, url, rel.Date.Format(time.DateOnly))
	} else {
		fmt.Fprintf(&b, "## [%s] - %s\n\n", version, rel.Date.Format(time.DateOnly))
	}
	b.WriteString(body(rel, sections))
	return b.String()
}

// body renders the sections of a release, normalised to end in exactly one
// newline so that callers can append a footer without guessing at the spacing.
func body(rel Release, sections []Section) string {
	rendered := renderSections(rel, ParseAll(rel.Commits), sections)
	if strings.TrimSpace(rendered) == "" {
		return "_No user-facing changes._\n"
	}
	return strings.TrimRight(rendered, "\n") + "\n"
}

// renderSections writes the breaking-change callout followed by every visible,
// non-empty section. Breaking changes are listed twice on purpose: once under
// the callout, where they cannot be missed, and once under the section for
// their type, where they belong chronologically.
func renderSections(rel Release, entries []Entry, sections []Section) string {
	var b strings.Builder

	var breaking []Entry
	for _, e := range entries {
		if e.Breaking {
			breaking = append(breaking, e)
		}
	}
	if len(breaking) > 0 {
		b.WriteString("### Breaking Changes\n\n")
		for _, e := range breaking {
			b.WriteString(bullet(rel.Repo, e))
			if e.BreakingNote != "" {
				fmt.Fprintf(&b, "  %s\n", e.BreakingNote)
			}
		}
		b.WriteString("\n")
	}

	claimed := claimedTypes(sections)
	for _, s := range sections {
		if s.Hidden {
			continue
		}
		list := classified(entries, s, claimed)
		if len(list) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", s.Title)
		for _, e := range list {
			b.WriteString(bullet(rel.Repo, e))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// bullet renders a single list item, linking to the commit when possible.
func bullet(repo Repository, e Entry) string {
	var b strings.Builder
	b.WriteString("- ")
	if e.Scope != "" {
		fmt.Fprintf(&b, "**%s:** ", e.Scope)
	}
	b.WriteString(e.Subject)

	if short := shortSHA(e.Commit.SHA); short != "" {
		if url := repo.CommitURL(e.Commit.SHA); url != "" {
			fmt.Fprintf(&b, " ([%s](%s))", short, url)
		} else {
			fmt.Fprintf(&b, " (%s)", short)
		}
	}
	b.WriteString("\n")
	return b.String()
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
