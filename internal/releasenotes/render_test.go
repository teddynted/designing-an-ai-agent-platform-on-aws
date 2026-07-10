package releasenotes_test

import (
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/releasenotes"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

func fullNotes() releasenotes.Notes {
	return releasenotes.Notes{
		Version:     version.MustParse("0.2.0"),
		Date:        release.MustDate("2026-07-10"),
		Repository:  "teddynted/platform",
		PreviousTag: "v0.1.0",
		Summary:     "A Go release-management module.",
		Highlights:  []string{"SemVer arithmetic", "Changelog generation from commit history"},
		Sections: map[releasenotes.Section][]releasenotes.Entry{
			releasenotes.Breaking: {
				{Title: "Drop the v1 endpoint", Number: 12, SHA: "aaaaaaa1", Breaking: true},
			},
			releasenotes.Features: {
				{Title: "Add the release management module", Number: 6, SHA: "bbbbbbb2"},
				{Title: "Add a gitignore", SHA: "ccccccc3"},
			},
			releasenotes.BugFixes: {
				{Title: "Correct the numstat parser", Number: 7, SHA: "ddddddd4"},
			},
			releasenotes.Documentation: {
				{Title: "Document the release workflow", Number: 8, SHA: "eeeeeee5"},
			},
		},
		Contributors: []releasenotes.Contributor{
			{Name: "Teddy Kekana", Login: "teddynted"},
			{Name: "Ada Lovelace"},
		},
		Statistics: releasenotes.Statistics{
			Commits: 4, FilesChanged: 56, Insertions: 9035, Deletions: 60, Contributors: 2,
		},
		Commits: []git.Commit{
			{SHA: "bbbbbbb2ffff", Subject: "Add the release management module"},
			{SHA: "ccccccc3ffff", Subject: "Add a gitignore"},
		},
	}
}

func TestRenderGolden(t *testing.T) {
	got := releasenotes.Render(fullNotes())

	want := `# v0.2.0

## 🚀 Highlights

A Go release-management module.

- SemVer arithmetic
- Changelog generation from commit history

## ⚠️ Breaking Changes

- Drop the v1 endpoint (#12)

## ✨ New Features

- Add the release management module (#6)
- Add a gitignore

## 🐛 Bug Fixes

- Correct the numstat parser (#7)

## 📚 Documentation

- Document the release workflow (#8)

## 📊 Release Statistics

| | |
|---|---|
| **Commits** | 4 |
| **Contributors** | 2 |
| **Files changed** | 56 |
| **Lines added** | 9035 |
| **Lines removed** | 60 |

## 🙌 Contributors

Thanks to:

- @teddynted
- Ada Lovelace

<details>
<summary>All commits in this release</summary>

- ` + "`bbbbbbb`" + ` Add the release management module
- ` + "`ccccccc`" + ` Add a gitignore

</details>

## 🔗 Full Changelog

https://github.com/teddynted/platform/compare/v0.1.0...v0.2.0
`
	if got != want {
		t.Errorf("Render() mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A "Bug Fixes" heading followed by nothing is worse than no heading.
func TestRenderOmitsEmptySections(t *testing.T) {
	notes := fullNotes()
	notes.Sections = map[releasenotes.Section][]releasenotes.Entry{
		releasenotes.Features: {{Title: "Add a thing"}},
	}
	got := releasenotes.Render(notes)

	for _, absent := range []string{"Bug Fixes", "Breaking Changes", "Documentation", "Internal", "Security"} {
		if strings.Contains(got, absent) {
			t.Errorf("empty section %q should be omitted:\n%s", absent, got)
		}
	}
	if !strings.Contains(got, "✨ New Features") {
		t.Errorf("a populated section should render:\n%s", got)
	}
}

// The breaking marker is redundant inside the Breaking Changes section, and
// necessary outside it.
func TestRenderBreakingMarker(t *testing.T) {
	notes := fullNotes()
	notes.Sections = map[releasenotes.Section][]releasenotes.Entry{
		releasenotes.Breaking:     {{Title: "Drop the v1 endpoint", Breaking: true}},
		releasenotes.Improvements: {{Title: "Rename a flag", Breaking: true}},
	}
	got := releasenotes.Render(notes)

	if strings.Contains(got, "**Breaking:** Drop the v1 endpoint") {
		t.Errorf("the marker is redundant inside Breaking Changes:\n%s", got)
	}
	if !strings.Contains(got, "**Breaking:** Rename a flag") {
		t.Errorf("a breaking entry outside the section must be marked:\n%s", got)
	}
}

func TestRenderSingleContributor(t *testing.T) {
	notes := fullNotes()
	notes.Contributors = []releasenotes.Contributor{{Name: "Teddy Kekana", Login: "teddynted"}}
	got := releasenotes.Render(notes)

	if !strings.Contains(got, "This release was written by @teddynted.") {
		t.Errorf("a single contributor should read as a sentence:\n%s", got)
	}
	if strings.Contains(got, "Thanks to:") {
		t.Errorf("no list for one person:\n%s", got)
	}
}

func TestRenderNoContributorsOmitsTheSection(t *testing.T) {
	notes := fullNotes()
	notes.Contributors = nil
	if strings.Contains(releasenotes.Render(notes), "Contributors\n\n") {
		t.Error("an empty contributors section should be omitted")
	}
}

// Hashes are for the reader who went looking; they are not what a release is
// about.
func TestRenderFoldsCommitHashesAway(t *testing.T) {
	got := releasenotes.Render(fullNotes())

	before, after, found := strings.Cut(got, "<details>")
	if !found {
		t.Fatalf("the raw commit list should be collapsed:\n%s", got)
	}
	if strings.Contains(before, "bbbbbbb") {
		t.Errorf("a commit hash appeared before the fold:\n%s", before)
	}
	if !strings.Contains(after, "`bbbbbbb`") {
		t.Errorf("the hash should be inside the fold:\n%s", after)
	}
	if !strings.Contains(after, "</details>") {
		t.Error("the details block should close")
	}
}

func TestRenderInitialReleaseLinksToCommits(t *testing.T) {
	notes := fullNotes()
	notes.PreviousTag = ""
	got := releasenotes.Render(notes)

	if strings.Contains(got, "/compare/") {
		t.Errorf("the first release has no predecessor to compare against:\n%s", got)
	}
	if !strings.Contains(got, "https://github.com/teddynted/platform/commits/v0.2.0") {
		t.Errorf("the first release should link to its commits:\n%s", got)
	}
}

func TestRenderWithoutARepositoryOmitsTheLink(t *testing.T) {
	notes := fullNotes()
	notes.Repository = ""
	if strings.Contains(releasenotes.Render(notes), "Full Changelog") {
		t.Error("no repository means no link")
	}
}

func TestRenderEmptyRelease(t *testing.T) {
	notes := releasenotes.Notes{
		Version:     version.MustParse("0.2.1"),
		Repository:  "teddynted/platform",
		PreviousTag: "v0.2.0",
		Summary:     "0 commits from 0 contributors, across 0 files.",
		Sections:    map[releasenotes.Section][]releasenotes.Entry{},
	}
	got := releasenotes.Render(notes)

	if !strings.Contains(got, "This release carries no user-facing changes.") {
		t.Errorf("an empty release should say so:\n%s", got)
	}
	if !strings.Contains(got, "# v0.2.1") {
		t.Errorf("the heading should still render:\n%s", got)
	}
	if !strings.Contains(got, "🚀 Highlights") {
		t.Errorf("the summary should still render:\n%s", got)
	}
}

// No section may appear twice, and no heading may be left dangling.
func TestRenderHasNoDuplicateHeadings(t *testing.T) {
	got := releasenotes.Render(fullNotes())
	for _, heading := range []string{"## 🚀 Highlights", "## 📊 Release Statistics", "## 🙌 Contributors", "## 🔗 Full Changelog"} {
		if n := strings.Count(got, heading); n != 1 {
			t.Errorf("heading %q appears %d times", heading, n)
		}
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("no triple blank lines:\n%q", got)
	}
}
