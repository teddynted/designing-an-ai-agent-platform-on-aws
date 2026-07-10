// Package releasenotes assembles and renders the body of a GitHub Release.
//
// It is separate from the changelog package because the two documents are read
// differently. CHANGELOG.md is a ledger: terse, chronological, grouped by Keep a
// Changelog's six categories, and committed into the tag it describes. A GitHub
// Release is an announcement: it leads with what matters, groups by what a
// reader cares about, and hides the machinery.
//
// The text of an entry is never invented. A bullet is a pull request title, or
// failing that a commit subject — both written by a person. The Highlights
// paragraph comes from the `summary` and `highlights` fields of RELEASES.yaml,
// which a human writes and a reviewer sees. Nothing here paraphrases, and
// nothing calls a language model: a release note is a record, and a wrong claim
// in it is worse than a terse one. This is the same rule the commit classifier
// follows, for the same reason.
package releasenotes

// Section is a heading in the release body.
//
// These are the divisions a reader of a release cares about — "what is new",
// "what broke", "what I can ignore" — and they are deliberately not the Keep a
// Changelog categories, which answer a different question for a different
// document.
type Section string

const (
	// Breaking comes first because it is the only section that can ruin
	// somebody's afternoon.
	Breaking Section = "Breaking Changes"

	// Security is its own section rather than a line in Bug Fixes. A security
	// fix filed under "fixed" is a security fix nobody notices.
	Security Section = "Security"

	Features      Section = "New Features"
	Improvements  Section = "Improvements"
	BugFixes      Section = "Bug Fixes"
	Documentation Section = "Documentation"

	// Internal is where the machinery goes: CI, refactors, tests, dependencies.
	// True, and of no interest to someone deciding whether to upgrade.
	Internal Section = "Internal"
)

// Order is the sequence sections are rendered in. Empty sections are omitted, so
// most releases show only a few of these.
var Order = []Section{
	Breaking,
	Security,
	Features,
	Improvements,
	BugFixes,
	Documentation,
	Internal,
}

// emoji prefixes each heading, matching the conventions of the projects this
// format is modelled on.
var emoji = map[Section]string{
	Breaking:      "⚠️",
	Security:      "🔒",
	Features:      "✨",
	Improvements:  "🔄",
	BugFixes:      "🐛",
	Documentation: "📚",
	Internal:      "🏗",
}

func (s Section) String() string { return string(s) }

// Heading renders the section's markdown heading, emoji included.
func (s Section) Heading() string {
	if e, ok := emoji[s]; ok {
		return e + " " + string(s)
	}
	return string(s)
}
