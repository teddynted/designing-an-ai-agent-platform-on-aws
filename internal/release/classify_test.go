package release_test

import (
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
)

func TestClassifyConventionalCommits(t *testing.T) {
	tests := []struct {
		name            string
		subject         string
		body            string
		wantCategory    release.Category
		wantDescription string
		wantBreaking    bool
	}{
		{"feat", "feat: add the roadmap registry", "", release.Added, "Add the roadmap registry", false},
		{"fix", "fix: correct the numstat parser", "", release.Fixed, "Correct the numstat parser", false},
		{"docs", "docs: explain the seam", "", release.Changed, "Explain the seam", false},
		{"perf", "perf: cache the tag list", "", release.Changed, "Cache the tag list", false},
		{"refactor", "refactor: split the service", "", release.Changed, "Split the service", false},
		{"security", "security: pin the action", "", release.Security, "Pin the action", false},
		{"deprecate", "deprecate: the v1 endpoint", "", release.Deprecated, "The v1 endpoint", false},
		{"remove", "remove: the old parser", "", release.Removed, "The old parser", false},

		// A scope is appended rather than dropped.
		{"scope", "feat(git): add rename detection", "", release.Added, "Add rename detection (git)", false},

		// Breaking changes are Changed regardless of type, and marked.
		{"breaking bang", "feat!: drop the v1 endpoint", "", release.Changed, "Drop the v1 endpoint", true},
		{"breaking scope bang", "feat(api)!: drop v1", "", release.Changed, "Drop v1 (api)", true},
		{"breaking footer", "feat: rework routing", "BREAKING CHANGE: header moved", release.Changed, "Rework routing", true},

		// Trailing punctuation goes; the first letter is capitalised.
		{"tidy", "fix: stop the crash.", "", release.Fixed, "Stop the crash", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, keep := release.Classify(git.Commit{Subject: tc.subject, Body: tc.body})
			if !keep {
				t.Fatalf("Classify(%q) dropped the commit", tc.subject)
			}
			if got.Category != tc.wantCategory {
				t.Errorf("Category = %s, want %s", got.Category, tc.wantCategory)
			}
			if got.Description != tc.wantDescription {
				t.Errorf("Description = %q, want %q", got.Description, tc.wantDescription)
			}
			if got.Breaking != tc.wantBreaking {
				t.Errorf("Breaking = %v, want %v", got.Breaking, tc.wantBreaking)
			}
		})
	}
}

// This repository writes ordinary English imperatives, not Conventional Commits.
// The leading verb carries the category.
func TestClassifyImperativeMood(t *testing.T) {
	tests := []struct {
		subject string
		want    release.Category
	}{
		{"Draw service glyphs in the architecture SVG", release.Added},
		{"Add the release management module", release.Added},
		{"Introduce a model gateway seam", release.Added},
		{"Document the release workflow", release.Added},

		{"Stop the VPC border striking an annotation", release.Fixed},
		{"Fix the rename parser", release.Fixed},
		{"Prevent a downgrade of VERSION", release.Fixed},

		{"Remove milestone framing throughout", release.Removed},
		{"Delete the stale diagram", release.Removed},
		{"Drop the unused port", release.Removed},

		{"Restore milestone framing", release.Changed},
		{"Rename the gateway package", release.Changed},
		{"Simplify the classifier", release.Changed},

		{"Harden the sandbox", release.Security},

		// An unrecognised verb asserts least rather than guessing.
		{"Frobnicate the widget", release.Changed},
		{"Milestone 1: initial architecture", release.Changed},
	}
	for _, tc := range tests {
		t.Run(tc.subject, func(t *testing.T) {
			got, keep := release.Classify(git.Commit{Subject: tc.subject})
			if !keep {
				t.Fatalf("Classify(%q) dropped the commit", tc.subject)
			}
			if got.Category != tc.want {
				t.Errorf("Category = %s, want %s", got.Category, tc.want)
			}
			if got.Description != tc.subject {
				t.Errorf("Description = %q, want the subject unchanged", got.Description)
			}
		})
	}
}

// "Milestone 1: initial architecture" has a colon but is not a Conventional
// Commit. It must fall through to the imperative heuristic, not be parsed as a
// commit of type "Milestone".
func TestClassifyColonThatIsNotAConventionalType(t *testing.T) {
	got, keep := release.Classify(git.Commit{Subject: "Milestone 1: initial architecture"})
	if !keep {
		t.Fatal("commit was dropped")
	}
	if got.Description != "Milestone 1: initial architecture" {
		t.Errorf("Description = %q; the whole subject should survive", got.Description)
	}
}

func TestClassifyDropsNoise(t *testing.T) {
	for _, tc := range []struct{ name, subject string }{
		{"merge pull request", "Merge pull request #5 from teddynted/revert"},
		{"merge branch", "Merge branch 'main' into feature"},
		{"chore", "chore: tidy the makefile"},
		{"ci", "ci: pin the runner"},
		{"build", "build: bump go"},
		{"test", "test: add a case"},
		{"style", "style: gofmt"},
		{"version bump", "Bump version to 0.2.0"},
		{"prepare release", "Prepare release v0.2.0"},
		{"release", "Release v0.2.0"},
		{"empty", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, keep := release.Classify(git.Commit{Subject: tc.subject}); keep {
				t.Errorf("Classify(%q) should drop the commit", tc.subject)
			}
		})
	}
}

// Security wins over the verb, because a security fix filed under Fixed is a
// security fix nobody notices.
func TestClassifySecurityHintsOverrideTheVerb(t *testing.T) {
	tests := []struct{ name, subject, body string }{
		{"cve in subject", "Update the base image for CVE-2026-1234", ""},
		{"vulnerability in body", "Update the parser", "closes a vulnerability in the tag reader"},
		{"injection", "Escape the template to stop injection", ""},
		{"rce", "Patch the RCE in the gateway", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, keep := release.Classify(git.Commit{Subject: tc.subject, Body: tc.body})
			if !keep {
				t.Fatal("commit was dropped")
			}
			if got.Category != release.Security {
				t.Errorf("Category = %s, want Security", got.Category)
			}
		})
	}
}

func TestClassifiedCommitEntry(t *testing.T) {
	commit := git.Commit{SHA: "abcdef1234", Subject: "feat: add a thing"}
	classified, _ := release.Classify(commit)

	if got, want := classified.Entry(true), "Add a thing (abcdef1)"; got != want {
		t.Errorf("Entry(true) = %q, want %q", got, want)
	}
	if got, want := classified.Entry(false), "Add a thing"; got != want {
		t.Errorf("Entry(false) = %q, want %q", got, want)
	}

	breaking, _ := release.Classify(git.Commit{SHA: "abcdef1234", Subject: "feat!: drop v1"})
	if got, want := breaking.Entry(true), "**Breaking:** Drop v1 (abcdef1)"; got != want {
		t.Errorf("Entry(true) = %q, want %q", got, want)
	}
}

func TestClassifyAllPreservesOrderAndDropsNoise(t *testing.T) {
	commits := []git.Commit{
		{SHA: "1", Subject: "feat: first"},
		{SHA: "2", Subject: "Merge pull request #1 from x"},
		{SHA: "3", Subject: "chore: tidy"},
		{SHA: "4", Subject: "fix: second"},
	}
	got := release.ClassifyAll(commits)
	if len(got) != 2 {
		t.Fatalf("ClassifyAll kept %d commits, want 2", len(got))
	}
	if got[0].Commit.SHA != "1" || got[1].Commit.SHA != "4" {
		t.Errorf("order not preserved: %v", got)
	}
}

func TestClassifyAllOnNoCommits(t *testing.T) {
	if got := release.ClassifyAll(nil); len(got) != 0 {
		t.Errorf("ClassifyAll(nil) = %v", got)
	}
}
