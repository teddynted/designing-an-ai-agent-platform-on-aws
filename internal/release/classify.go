package release

import (
	"regexp"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
)

// Sorting commits into Keep a Changelog categories, without a model.
//
// Two strategies, tried in order.
//
// Conventional Commits. `feat: …` -> Added, `fix: …` -> Fixed, and so on. Exact,
// when the project uses it.
//
// Imperative mood. This repository does not use Conventional Commits. Its
// history reads "Restore milestone framing", "Draw service glyphs in the
// architecture SVG", "Stop the VPC border striking an annotation". Those are
// ordinary English imperatives, and the leading verb carries the category
// reliably enough to be useful: Draw and Add introduce things, Stop and Fix
// repair them, Remove removes them.
//
// The heuristic is honest about its limits. An unrecognised verb yields Changed
// — the category that asserts least — rather than a guess. Nothing here infers
// intent from the diff, and nothing calls a language model. A wrong guess in a
// changelog is worse than a vague one, because a changelog is read as a record.
//
// Housekeeping commits (chore, ci, build, test, style) and merge commits are
// dropped. They are true statements about the repository that no reader of a
// release note wants.

// conventional matches `type(scope)!: description`.
var conventional = regexp.MustCompile(`^(?P<type>[a-zA-Z]+)(?:\((?P<scope>[^)]*)\))?(?P<breaking>!)?:\s+(?P<description>.+)$`)

// leadingWord captures the first run of letters in a subject.
var leadingWord = regexp.MustCompile(`^[a-zA-Z]+`)

// dropped marks Conventional Commit types that are true and uninteresting.
var dropped = map[string]bool{
	"chore": true,
	"ci":    true,
	"build": true,
	"test":  true,
	"tests": true,
	"style": true,
}

var typeToCategory = map[string]Category{
	"feat":      Added,
	"feature":   Added,
	"add":       Added,
	"fix":       Fixed,
	"bugfix":    Fixed,
	"hotfix":    Fixed,
	"perf":      Changed,
	"refactor":  Changed,
	"revert":    Changed,
	"docs":      Changed,
	"deps":      Changed,
	"remove":    Removed,
	"removed":   Removed,
	"deprecate": Deprecated,
	"security":  Security,
}

// verbToCategory maps a leading imperative verb to a category.
var verbToCategory = map[string]Category{
	// Added
	"add":       Added,
	"introduce": Added,
	"create":    Added,
	"implement": Added,
	"support":   Added,
	"draw":      Added,
	"document":  Added,

	// Fixed
	"fix":     Fixed,
	"correct": Fixed,
	"repair":  Fixed,
	"resolve": Fixed,
	"prevent": Fixed,
	"stop":    Fixed,
	"avoid":   Fixed,
	"handle":  Fixed,

	// Removed
	"remove": Removed,
	"delete": Removed,
	"drop":   Removed,

	// Deprecated
	"deprecate": Deprecated,

	// Security
	"harden":   Security,
	"sanitise": Security,
	"sanitize": Security,

	// Changed
	"change":   Changed,
	"update":   Changed,
	"rename":   Changed,
	"restore":  Changed,
	"move":     Changed,
	"rework":   Changed,
	"refactor": Changed,
	"simplify": Changed,
	"improve":  Changed,
	"tighten":  Changed,
	"clarify":  Changed,
}

// securityHints promote a commit to Security regardless of its verb.
var securityHints = []string{"cve-", "vulnerabilit", "injection", "credential leak", "rce"}

// housekeepingSubjects are the release mechanics themselves, which never belong
// in the notes they generate.
var housekeepingSubjects = []string{"bump version", "prepare release", "release v"}

// ClassifiedCommit is a commit with its category and the text a changelog shows.
type ClassifiedCommit struct {
	Commit      git.Commit
	Category    Category
	Description string
	Breaking    bool
}

// Entry renders one changelog bullet, without the leading "- ".
func (c ClassifiedCommit) Entry(includeSHA bool) string {
	text := c.Description
	if c.Breaking {
		text = "**Breaking:** " + text
	}
	if includeSHA && c.Commit.ShortSHA() != "" {
		text += " (" + c.Commit.ShortSHA() + ")"
	}
	return text
}

// tidy capitalises, strips trailing punctuation, and leaves the rest alone.
func tidy(description string) string {
	text := strings.TrimRight(strings.TrimSpace(description), ".")
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

func isHousekeeping(subject string) bool {
	lowered := strings.ToLower(subject)
	for _, prefix := range housekeepingSubjects {
		if strings.HasPrefix(lowered, prefix) {
			return true
		}
	}
	return false
}

func mentionsSecurity(commit git.Commit) bool {
	haystack := strings.ToLower(commit.Subject + "\n" + commit.Body)
	for _, hint := range securityHints {
		if strings.Contains(haystack, hint) {
			return true
		}
	}
	return false
}

// Classify assigns a commit a category, reporting false when the commit should
// not appear in release notes at all.
func Classify(commit git.Commit) (ClassifiedCommit, bool) {
	subject := strings.TrimSpace(commit.Subject)
	if subject == "" || commit.IsMerge() || isHousekeeping(subject) {
		return ClassifiedCommit{}, false
	}

	breaking := commit.IsBreaking()

	if match := conventional.FindStringSubmatch(subject); match != nil {
		commitType := strings.ToLower(match[conventional.SubexpIndex("type")])
		if dropped[commitType] {
			return ClassifiedCommit{}, false
		}
		if category, known := typeToCategory[commitType]; known {
			description := match[conventional.SubexpIndex("description")]
			if scope := match[conventional.SubexpIndex("scope")]; scope != "" {
				description += " (" + scope + ")"
			}
			if breaking {
				category = Changed
			}
			return ClassifiedCommit{
				Commit:      commit,
				Category:    category,
				Description: tidy(description),
				Breaking:    breaking,
			}, true
		}
		// A colon that is not a Conventional Commit type — "Milestone 1: initial
		// architecture" — falls through to the imperative heuristic below.
	}

	if mentionsSecurity(commit) {
		return ClassifiedCommit{
			Commit:      commit,
			Category:    Security,
			Description: tidy(subject),
			Breaking:    breaking,
		}, true
	}

	category := Changed
	if word := leadingWord.FindString(subject); word != "" {
		if mapped, known := verbToCategory[strings.ToLower(word)]; known {
			category = mapped
		}
	}
	if breaking {
		category = Changed
	}
	return ClassifiedCommit{
		Commit:      commit,
		Category:    category,
		Description: tidy(subject),
		Breaking:    breaking,
	}, true
}

// ClassifyAll classifies a range, dropping merges and housekeeping. Order is
// preserved.
func ClassifyAll(commits []git.Commit) []ClassifiedCommit {
	var classified []ClassifiedCommit
	for _, commit := range commits {
		if c, keep := Classify(commit); keep {
			classified = append(classified, c)
		}
	}
	return classified
}
