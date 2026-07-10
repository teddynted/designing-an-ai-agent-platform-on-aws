package releasenotes_test

import (
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/releasenotes"
)

func TestSectionOfPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		body    string
		labels  []string
		paths   []string
		want    releasenotes.Section
	}{
		// 1. A breaking marker outranks every other signal, including a label
		//    that says otherwise.
		{
			name:    "bang beats a documentation label",
			subject: "feat!: drop the v1 endpoint",
			labels:  []string{"documentation"},
			paths:   []string{"docs/a.md"},
			want:    releasenotes.Breaking,
		},
		{
			name:    "footer beats a label",
			subject: "feat: rework routing",
			body:    "BREAKING CHANGE: the header moved",
			labels:  []string{"enhancement"},
			want:    releasenotes.Breaking,
		},
		{
			name:    "breaking-change label beats a conventional type",
			subject: "docs: explain the seam",
			labels:  []string{"breaking-change"},
			want:    releasenotes.Breaking,
		},

		// 2. A label beats a conventional type and the paths.
		{
			name:    "label beats conventional type",
			subject: "docs: explain the seam",
			labels:  []string{"feature"},
			paths:   []string{"docs/a.md"},
			want:    releasenotes.Features,
		},
		{
			name:    "security label",
			subject: "Update the base image",
			labels:  []string{"security"},
			want:    releasenotes.Security,
		},
		{
			name:    "ci label lands in internal",
			subject: "Pin the runner",
			labels:  []string{"ci"},
			want:    releasenotes.Internal,
		},
		{
			name:    "the first recognised label wins; unknown ones are skipped",
			subject: "Do a thing",
			labels:  []string{"needs-triage", "good first issue", "bug"},
			want:    releasenotes.BugFixes,
		},

		// 3. A conventional type beats the paths.
		{
			name:    "feat beats an internal path",
			subject: "feat: add rename detection",
			paths:   []string{"internal/git/repo.go"},
			want:    releasenotes.Features,
		},
		{name: "fix", subject: "fix: correct the parser", want: releasenotes.BugFixes},
		{name: "perf", subject: "perf: cache the tag list", want: releasenotes.Improvements},
		{name: "refactor", subject: "refactor: split the service", want: releasenotes.Internal},
		{name: "chore", subject: "chore: tidy the makefile", want: releasenotes.Internal},
		{name: "security type", subject: "security: pin the action", want: releasenotes.Security},
		{name: "scoped type", subject: "feat(git): add detection", want: releasenotes.Features},

		// 4. The paths, when nothing better is available.
		{
			name:    "documentation by path",
			subject: "Rework the overview",
			paths:   []string{"docs/architecture/01-overview.md", "README.md"},
			want:    releasenotes.Documentation,
		},
		{
			name:    "examples are documentation",
			subject: "Show the flag in use",
			paths:   []string{"examples/basic.go"},
			want:    releasenotes.Documentation,
		},
		{
			name:    "a root markdown file is documentation",
			subject: "Rework the guide",
			paths:   []string{"RELEASE_MANAGEMENT.md"},
			want:    releasenotes.Documentation,
		},
		{
			name:    "CI is internal",
			subject: "Pin the runner",
			paths:   []string{".github/workflows/release.yml"},
			want:    releasenotes.Internal,
		},
		{
			name:    "tests are internal",
			subject: "Cover the rename parser",
			paths:   []string{"internal/git/repo_test.go"},
			want:    releasenotes.Internal,
		},
		{
			name:    "dependency manifests are internal",
			subject: "Bump goccy",
			paths:   []string{"go.mod", "go.sum"},
			want:    releasenotes.Internal,
		},

		// A change touching code *and* its docs is a change to the code. Only a
		// change that is nothing but documentation is documentation.
		{
			name:    "code plus docs is not documentation",
			subject: "Rework the seam",
			paths:   []string{"internal/release/ports.go", "docs/a.md"},
			want:    releasenotes.Improvements,
		},

		// 5. The imperative verb, via the changelog classifier.
		{name: "verb: add", subject: "Add the roadmap registry", want: releasenotes.Features},
		{name: "verb: fix", subject: "Fix the numstat parser", want: releasenotes.BugFixes},
		{name: "verb: harden", subject: "Harden the tag parser", want: releasenotes.Security},
		{name: "verb: restore", subject: "Restore milestone framing", want: releasenotes.Improvements},
		{name: "verb: remove", subject: "Remove milestone framing", want: releasenotes.Improvements},

		// An unrecognised change asserts the least.
		{name: "unknown verb", subject: "Frobnicate the widget", want: releasenotes.Improvements},

		// A dropped changelog commit still needs a section here.
		{name: "housekeeping keeps a section", subject: "chore: tidy", want: releasenotes.Internal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			commit := git.Commit{Subject: tc.subject, Body: tc.body}
			if got := releasenotes.SectionOf(commit, tc.labels, tc.paths); got != tc.want {
				t.Errorf("SectionOf(%q) = %s, want %s", tc.subject, got, tc.want)
			}
		})
	}
}

// No paths means the file heuristics are skipped, not guessed at.
func TestSectionOfWithoutPaths(t *testing.T) {
	commit := git.Commit{Subject: "Rework the overview"}
	if got := releasenotes.SectionOf(commit, nil, nil); got != releasenotes.Improvements {
		t.Errorf("SectionOf() = %s, want Improvements", got)
	}
}

func TestSectionHeadings(t *testing.T) {
	tests := []struct {
		section releasenotes.Section
		want    string
	}{
		{releasenotes.Breaking, "⚠️ Breaking Changes"},
		{releasenotes.Security, "🔒 Security"},
		{releasenotes.Features, "✨ New Features"},
		{releasenotes.Improvements, "🔄 Improvements"},
		{releasenotes.BugFixes, "🐛 Bug Fixes"},
		{releasenotes.Documentation, "📚 Documentation"},
		{releasenotes.Internal, "🏗 Internal"},
	}
	for _, tc := range tests {
		if got := tc.section.Heading(); got != tc.want {
			t.Errorf("%s.Heading() = %q, want %q", tc.section, got, tc.want)
		}
	}
}

// Breaking changes come first, and Internal last. A reader scanning the top of a
// release must not miss the section that can ruin their afternoon.
func TestSectionOrder(t *testing.T) {
	if releasenotes.Order[0] != releasenotes.Breaking {
		t.Errorf("Breaking Changes must come first, got %s", releasenotes.Order[0])
	}
	if releasenotes.Order[1] != releasenotes.Security {
		t.Errorf("Security must come second, got %s", releasenotes.Order[1])
	}
	if last := releasenotes.Order[len(releasenotes.Order)-1]; last != releasenotes.Internal {
		t.Errorf("Internal must come last, got %s", last)
	}
}
