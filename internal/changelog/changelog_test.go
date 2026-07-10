package changelog

import (
	"strings"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

func TestParseConventionalSubjects(t *testing.T) {
	tests := []struct {
		subject  string
		wantType string
		scope    string
		want     string
		breaking bool
	}{
		{"feat: add release command", "feat", "", "add release command", false},
		{"fix(semver): reject leading zeros", "fix", "semver", "reject leading zeros", false},
		{"feat(api)!: drop v1 endpoints", "feat", "api", "drop v1 endpoints", true},
		{"FEAT: upper case type", "feat", "", "upper case type", false},
		{"refactor(git)!:\tuse a tab separator", "refactor", "git", "use a tab separator", true},

		// Not Conventional Commits: kept, but unclassified.
		{"Merge branch 'main'", "", "", "Merge branch 'main'", false},
		{"update readme", "", "", "update readme", false},
		{"feat:missing space", "", "", "feat:missing space", false},
		{"feat(): empty scope", "", "", "feat(): empty scope", false},
		{"feat:", "", "", "feat:", false},
	}
	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			e := Parse(Commit{Subject: tt.subject})
			if e.Type != tt.wantType || e.Scope != tt.scope || e.Subject != tt.want || e.Breaking != tt.breaking {
				t.Errorf("Parse(%q) = type=%q scope=%q subject=%q breaking=%v; want type=%q scope=%q subject=%q breaking=%v",
					tt.subject, e.Type, e.Scope, e.Subject, e.Breaking, tt.wantType, tt.scope, tt.want, tt.breaking)
			}
		})
	}
}

func TestParseBreakingFooter(t *testing.T) {
	for _, prefix := range []string{"BREAKING CHANGE:", "BREAKING-CHANGE:"} {
		body := "Some context.\n\n" + prefix + " the --force flag\nis gone entirely.\n\nRefs: #12"
		e := Parse(Commit{Subject: "fix: tidy flags", Body: body})
		if !e.Breaking {
			t.Fatalf("%s footer should mark the entry breaking", prefix)
		}
		if want := "the --force flag is gone entirely."; e.BreakingNote != want {
			t.Errorf("BreakingNote = %q, want %q", e.BreakingNote, want)
		}
	}

	if e := Parse(Commit{Subject: "fix: unrelated", Body: "BREAKING CHANGE: on the first line"}); !e.Breaking {
		t.Error("a footer on the first body line should still count")
	}
	if e := Parse(Commit{Subject: "fix: unrelated", Body: "no footer here"}); e.Breaking {
		t.Error("a body without a footer must not be breaking")
	}
	// The footer must start a line: prose that merely mentions it is not a footer.
	if e := Parse(Commit{Subject: "fix: unrelated", Body: "this is not a BREAKING CHANGE: really"}); e.Breaking {
		t.Error("a mid-line mention must not be treated as a footer")
	}
}

func fixture() Release {
	return Release{
		Tag:         "v1.4.0",
		Version:     semver.MustParse("1.4.0"),
		PreviousTag: "v1.3.0",
		Date:        time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
		Repo:        Repository{Host: "github.com", Owner: "teddynted", Name: "repo"},
		Commits: []Commit{
			{SHA: "aaaaaaabbbbbbb", Subject: "feat(cli): add --dry-run"},
			{SHA: "cccccccddddddd", Subject: "feat!: require Go 1.25", Body: "BREAKING CHANGE: Go 1.24 is no longer supported"},
			{SHA: "eeeeeeefffffff", Subject: "fix: handle empty tag list"},
			{SHA: "1111111222222", Subject: "chore: bump linter"},
			{SHA: "3333333444444", Subject: "wip on something"},
		},
	}
}

func TestRenderNotes(t *testing.T) {
	got := RenderNotes(fixture(), DefaultSections())

	for _, want := range []string{
		"### Breaking Changes\n",
		"- require Go 1.25 ([ccccccc](https://github.com/teddynted/repo/commit/cccccccddddddd))\n",
		"  Go 1.24 is no longer supported\n",
		"### Features\n",
		"- **cli:** add --dry-run ([aaaaaaa](https://github.com/teddynted/repo/commit/aaaaaaabbbbbbb))\n",
		"### Bug Fixes\n",
		"- handle empty tag list ",
		// An unrecognised type falls through to the catch-all section.
		"### Other Changes\n",
		"- wip on something ",
		"**Full Changelog**: https://github.com/teddynted/repo/compare/v1.3.0...v1.4.0\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderNotes() missing %q\n---\n%s", want, got)
		}
	}

	// Hidden sections stay out of the notes.
	if strings.Contains(got, "bump linter") {
		t.Errorf("RenderNotes() should hide chore commits\n---\n%s", got)
	}

	// The breaking change is also listed under its own type.
	if n := strings.Count(got, "require Go 1.25"); n != 2 {
		t.Errorf("breaking feat should appear under both headings, appeared %d times", n)
	}
}

func TestRenderNotesFirstRelease(t *testing.T) {
	rel := fixture()
	rel.PreviousTag = ""
	got := RenderNotes(rel, DefaultSections())
	if !strings.Contains(got, "**Commit history**: https://github.com/teddynted/repo/commits/v1.4.0") {
		t.Errorf("a first release should link to the commit history\n---\n%s", got)
	}
}

func TestRenderNotesWithoutRepository(t *testing.T) {
	rel := fixture()
	rel.Repo = Repository{}
	got := RenderNotes(rel, DefaultSections())
	if strings.Contains(got, "https://") {
		t.Errorf("an unknown repository must not produce links\n---\n%s", got)
	}
	if !strings.Contains(got, "- **cli:** add --dry-run (aaaaaaa)") {
		t.Errorf("the short SHA should still be shown\n---\n%s", got)
	}
}

func TestRenderNotesNoVisibleChanges(t *testing.T) {
	rel := fixture()
	rel.Commits = []Commit{{SHA: "1111111", Subject: "chore: bump linter"}}
	if got := RenderNotes(rel, DefaultSections()); !strings.Contains(got, "_No user-facing changes._") {
		t.Errorf("RenderNotes() = %q, want the empty-release placeholder", got)
	}
}

func TestRenderEntry(t *testing.T) {
	got := RenderEntry(fixture(), DefaultSections())
	want := "## [1.4.0](https://github.com/teddynted/repo/compare/v1.3.0...v1.4.0) - 2026-07-10\n"
	if !strings.HasPrefix(got, want) {
		t.Errorf("RenderEntry() should start with %q\n---\n%s", want, got)
	}
	if strings.Contains(got, "Full Changelog") {
		t.Error("a CHANGELOG entry should not repeat the compare link in the footer")
	}
}

func TestInsertIntoEmptyFile(t *testing.T) {
	out, changed := Insert(nil, "1.0.0", "## [1.0.0] - 2026-07-10\n\n### Features\n\n- first\n")
	if !changed {
		t.Fatal("inserting into an empty file should change it")
	}
	got := string(out)
	if !strings.HasPrefix(got, "# Changelog") {
		t.Errorf("a new file should get the standard header:\n%s", got)
	}
	if !strings.Contains(got, "## [1.0.0]") {
		t.Errorf("the entry is missing:\n%s", got)
	}
}

func TestInsertPrependsNewestFirst(t *testing.T) {
	existing := Header + "\n## [1.0.0] - 2026-01-01\n\n### Features\n\n- first\n"
	out, changed := Insert([]byte(existing), "1.1.0", "## [1.1.0] - 2026-07-10\n\n### Features\n\n- second\n")
	if !changed {
		t.Fatal("a new version should change the file")
	}
	got := string(out)

	newest, oldest := strings.Index(got, "## [1.1.0]"), strings.Index(got, "## [1.0.0]")
	if newest == -1 || oldest == -1 {
		t.Fatalf("both versions should be present:\n%s", got)
	}
	if newest > oldest {
		t.Errorf("1.1.0 should precede 1.0.0:\n%s", got)
	}
	if !strings.HasPrefix(got, "# Changelog") {
		t.Errorf("the preamble should be preserved:\n%s", got)
	}
}

// Re-running the release workflow for a tag must not duplicate its entry.
func TestInsertIsIdempotent(t *testing.T) {
	entry := "## [1.1.0] - 2026-07-10\n\n### Features\n\n- second\n"
	first, _ := Insert([]byte(Header), "1.1.0", entry)
	second, changed := Insert(first, "1.1.0", entry)
	if changed {
		t.Error("inserting the same version twice should report no change")
	}
	if string(first) != string(second) {
		t.Error("inserting the same version twice should leave the file untouched")
	}
}

func TestInsertPreambleOnly(t *testing.T) {
	out, changed := Insert([]byte("# Changelog\n\nSome preamble.\n"), "1.0.0", "## [1.0.0] - 2026-07-10\n")
	if !changed {
		t.Fatal("should have changed")
	}
	got := string(out)
	if !strings.HasPrefix(got, "# Changelog\n\nSome preamble.\n\n## [1.0.0]") {
		t.Errorf("the entry should follow the preamble:\n%q", got)
	}
}
