package releasenotes

import (
	"path"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
)

// Choosing a section, from the most reliable signal available.
//
// The precedence below is ordered by how much a human meant it:
//
//  1. A breaking-change marker. Explicit, and it outranks everything.
//  2. A pull request label. Someone chose it, in a list the project curates.
//  3. A Conventional Commit type. Someone typed it, in a vocabulary the
//     specification defines.
//  4. The files touched. Not intent, but strong evidence: a change confined to
//     docs/ is a documentation change whatever its verb claims.
//  5. The leading imperative verb, via the commit classifier.
//
// Anything unrecognised lands in Improvements — the section that asserts least
// about a change the tooling did not understand.

// labelToSection maps the project's curated label vocabulary onto sections.
var labelToSection = map[string]Section{
	"breaking-change": Breaking,
	"breaking":        Breaking,

	"security": Security,

	"feature": Features,
	"feat":    Features,

	"enhancement": Improvements,
	"performance": Improvements,
	"perf":        Improvements,

	"bug":    BugFixes,
	"bugfix": BugFixes,
	"fix":    BugFixes,

	"documentation": Documentation,
	"docs":          Documentation,

	"ci":           Internal,
	"build":        Internal,
	"dependencies": Internal,
	"deps":         Internal,
	"refactor":     Internal,
	"tests":        Internal,
	"test":         Internal,
	"chore":        Internal,
}

// typeToSection maps Conventional Commit types onto sections.
var typeToSection = map[string]Section{
	"feat":     Features,
	"feature":  Features,
	"fix":      BugFixes,
	"bugfix":   BugFixes,
	"hotfix":   BugFixes,
	"security": Security,
	"perf":     Improvements,
	"docs":     Documentation,
	"refactor": Internal,
	"test":     Internal,
	"tests":    Internal,
	"ci":       Internal,
	"build":    Internal,
	"chore":    Internal,
	"deps":     Internal,
	"style":    Internal,
}

// categoryToSection is the fallback, from the changelog classifier's category.
var categoryToSection = map[release.Category]Section{
	release.Added:      Features,
	release.Fixed:      BugFixes,
	release.Security:   Security,
	release.Changed:    Improvements,
	release.Deprecated: Improvements,
	release.Removed:    Improvements,
}

// documentationPaths are the roots whose contents are documentation. A change
// confined to them is documentation, whatever its commit subject claims.
var documentationPaths = []string{"docs/", "examples/", "example/", "tutorials/", "wiki/"}

// documentationFiles are documentation wherever they sit.
var documentationFiles = []string{
	"README.md", "CONTRIBUTING.md", "CHANGELOG.md", "RELEASE_MANAGEMENT.md",
	"CODE_OF_CONDUCT.md", "SECURITY.md", "LICENSE",
}

// internalPaths are the machinery: build, CI, and the release tooling itself.
var internalPaths = []string{".github/", "internal/", "cmd/", "scripts/", "hack/"}

// internalFiles are build and dependency manifests.
var internalFiles = []string{"Makefile", "go.mod", "go.sum", "Dockerfile", ".gitignore"}

// isDocumentation reports whether every path touched is documentation.
//
// Every, not any: a change that edits a package and its README is a change to
// the package. Only a change that is *nothing but* documentation belongs under
// Documentation.
func isDocumentation(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if !isDocumentationPath(p) {
			return false
		}
	}
	return true
}

func isDocumentationPath(p string) bool {
	for _, root := range documentationPaths {
		if strings.HasPrefix(p, root) {
			return true
		}
	}
	base := path.Base(p)
	for _, name := range documentationFiles {
		if base == name {
			return true
		}
	}
	// A stray markdown file at the repository root is documentation.
	return !strings.Contains(p, "/") && strings.HasSuffix(p, ".md")
}

// isInternal reports whether every path touched is machinery, or a test.
func isInternal(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if !isInternalPath(p) {
			return false
		}
	}
	return true
}

func isInternalPath(p string) bool {
	if strings.HasSuffix(p, "_test.go") {
		return true
	}
	for _, root := range internalPaths {
		if strings.HasPrefix(p, root) {
			return true
		}
	}
	base := path.Base(p)
	for _, name := range internalFiles {
		if base == name {
			return true
		}
	}
	return false
}

// SectionOf chooses the section for one change.
//
// labels may be empty, which is the ordinary case when no token is configured or
// the change arrived without a pull request. paths may be empty, in which case
// the file heuristics are skipped rather than guessed at.
func SectionOf(commit git.Commit, labels []string, paths []string) Section {
	// 1. Breaking outranks everything, from either marker.
	if commit.IsBreaking() || hasLabel(labels, "breaking-change", "breaking") {
		return Breaking
	}

	// 2. A label somebody chose.
	for _, label := range labels {
		if section, ok := labelToSection[strings.ToLower(strings.TrimSpace(label))]; ok {
			return section
		}
	}

	// 3. A Conventional Commit type somebody typed.
	if commitType, ok := conventionalType(commit.Subject); ok {
		if section, known := typeToSection[commitType]; known {
			return section
		}
	}

	// 4. The files touched. Evidence, not intent — but a change confined to
	//    docs/ is documentation however its verb reads.
	if isDocumentation(paths) {
		return Documentation
	}
	if isInternal(paths) {
		return Internal
	}

	// 5. The leading imperative verb, via the changelog classifier. Reusing it
	//    means the two documents cannot disagree about what a commit is.
	if classified, keep := release.Classify(commit); keep {
		if section, ok := categoryToSection[classified.Category]; ok {
			return section
		}
	}

	return Improvements
}

// conventionalType extracts the type from `type(scope)!: description`.
func conventionalType(subject string) (string, bool) {
	head, _, found := strings.Cut(subject, ":")
	if !found || head == "" || strings.Contains(head, " ") {
		return "", false
	}
	head = strings.TrimSuffix(head, "!")
	if open := strings.Index(head, "("); open >= 0 {
		head = head[:open]
	}
	head = strings.ToLower(strings.TrimSpace(head))
	if head == "" {
		return "", false
	}
	for _, r := range head {
		if r < 'a' || r > 'z' {
			return "", false
		}
	}
	return head, true
}

func hasLabel(labels []string, wanted ...string) bool {
	for _, label := range labels {
		normalised := strings.ToLower(strings.TrimSpace(label))
		for _, w := range wanted {
			if normalised == w {
				return true
			}
		}
	}
	return false
}
