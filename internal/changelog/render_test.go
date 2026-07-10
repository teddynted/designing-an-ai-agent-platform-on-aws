package changelog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

func fixture() Release {
	return Release{
		Tag:         "v1.4.0",
		Version:     semver.MustParse("1.4.0"),
		PreviousTag: "v1.3.0",
		Date:        time.Date(2026, 7, 10, 12, 30, 0, 0, time.UTC),
		Repo:        testRepo,
		Bump:        "minor",
		Commits: []Commit{
			{SHA: "aaaaaaabbbbbbb", Subject: "feat(cli): add --dry-run"},
			{SHA: "cccccccddddddd", Subject: "feat!: require Go 1.25", Body: "BREAKING CHANGE: Go 1.24 is no longer supported"},
			{SHA: "eeeeeeefffffff", Subject: "fix: handle empty tag list."},
			{SHA: "1111111222222", Subject: "chore: bump the linter"},
			{SHA: "3333333444444", Subject: "wip on something"},
		},
	}
}

func mustRenderNotes(t *testing.T, rel Release, opts Options) string {
	t.Helper()
	out, err := RenderNotes(rel, opts)
	if err != nil {
		t.Fatalf("RenderNotes: %v", err)
	}
	return out
}

func TestRenderNotes(t *testing.T) {
	got := mustRenderNotes(t, fixture(), Options{})

	for _, want := range []string{
		"## What's Changed\n",
		"### ⚠️ Breaking Changes\n",
		"- Require Go 1.25 ([ccccccc](https://github.com/teddynted/repo/commit/cccccccddddddd))\n",
		"  Go 1.24 is no longer supported\n",
		"### 🚀 Features\n",
		"- **cli:** Add --dry-run ([aaaaaaa](https://github.com/teddynted/repo/commit/aaaaaaabbbbbbb))\n",
		"### 🐛 Bug Fixes\n",
		// The subject is capitalised and its trailing full stop removed.
		"- Handle empty tag list (",
		"### 🧹 Chores\n",
		// An unrecognised subject falls through to the catch-all.
		"### Other Changes\n",
		"- Wip on something (",
		"Compare changes:\nhttps://github.com/teddynted/repo/compare/v1.3.0...v1.4.0\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderNotes() missing %q\n---\n%s", want, got)
		}
	}

	// The old footer wording is gone.
	if strings.Contains(got, "Full Changelog") {
		t.Errorf("the footer should read \"Compare changes:\"\n---\n%s", got)
	}
	// The breaking change is listed under both its callout and its category.
	if n := strings.Count(got, "Require Go 1.25"); n != 2 {
		t.Errorf("breaking feat should appear twice, appeared %d times\n---\n%s", n, got)
	}
	// The note appears only once, under the callout.
	if n := strings.Count(got, "Go 1.24 is no longer supported"); n != 1 {
		t.Errorf("the breaking note should appear once, appeared %d times", n)
	}
}

// Section order follows the categories, and headings carry their icons.
func TestRenderNotesSectionOrder(t *testing.T) {
	got := mustRenderNotes(t, fixture(), Options{})
	want := []string{"⚠️ Breaking Changes", "🚀 Features", "🐛 Bug Fixes", "🧹 Chores", "Other Changes"}

	at := -1
	for _, heading := range want {
		i := strings.Index(got, "### "+heading)
		if i == -1 {
			t.Fatalf("missing heading %q\n---\n%s", heading, got)
		}
		if i < at {
			t.Errorf("heading %q is out of order\n---\n%s", heading, got)
		}
		at = i
	}
}

func TestRenderNotesFirstRelease(t *testing.T) {
	rel := fixture()
	rel.PreviousTag = ""

	got := mustRenderNotes(t, rel, Options{})
	if !strings.Contains(got, "Initial release.") {
		t.Errorf("a first release should say so\n---\n%s", got)
	}
	if strings.Contains(got, "Compare changes:") || strings.Contains(got, "/compare/") {
		t.Errorf("a first release has nothing to compare against\n---\n%s", got)
	}
}

func TestRenderNotesWithoutRepository(t *testing.T) {
	rel := fixture()
	rel.Repo = Repository{}

	got := mustRenderNotes(t, rel, Options{})
	if strings.Contains(got, "https://") {
		t.Errorf("an unknown repository must not produce links\n---\n%s", got)
	}
	if !strings.Contains(got, "- **cli:** Add --dry-run (aaaaaaa)") {
		t.Errorf("the short SHA should still be shown\n---\n%s", got)
	}
	// No repository means no compare URL, so the footer is omitted entirely.
	if strings.Contains(got, "Compare changes:") {
		t.Errorf("there is no URL to compare against\n---\n%s", got)
	}
}

func TestRenderNotesEmptyRelease(t *testing.T) {
	rel := fixture()
	rel.Commits = nil

	got := mustRenderNotes(t, rel, Options{})
	if !strings.Contains(got, "_No user-facing changes._") {
		t.Errorf("RenderNotes() = %q, want the empty-release placeholder", got)
	}
	if !strings.Contains(got, "Compare changes:") {
		t.Errorf("an empty release still links to the comparison\n---\n%s", got)
	}
}

// Output ends in exactly one newline, so a caller can append without guessing.
func TestRenderNotesTrailingNewline(t *testing.T) {
	got := mustRenderNotes(t, fixture(), Options{})
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Errorf("output should end in exactly one newline, got %q", got[len(got)-4:])
	}
}

func TestRenderNotesNoBlankLineRuns(t *testing.T) {
	if got := mustRenderNotes(t, fixture(), Options{}); strings.Contains(got, "\n\n\n") {
		t.Errorf("output should not contain runs of blank lines\n---\n%q", got)
	}
}

func TestRenderEntry(t *testing.T) {
	got, err := RenderEntry(fixture(), Options{})
	if err != nil {
		t.Fatalf("RenderEntry: %v", err)
	}

	want := "## [1.4.0](https://github.com/teddynted/repo/compare/v1.3.0...v1.4.0) - 2026-07-10\n"
	if !strings.HasPrefix(got, want) {
		t.Errorf("RenderEntry() should start with %q\n---\n%s", want, got)
	}
	if !strings.Contains(got, "### 🚀 Features\n") {
		t.Errorf("the entry should carry the same headings as the notes\n---\n%s", got)
	}
	// The compare link is in the heading, so the entry needs no footer.
	for _, unwanted := range []string{"Compare changes:", "What's Changed", "Initial release."} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a CHANGELOG entry should not contain %q\n---\n%s", unwanted, got)
		}
	}
}

func TestRenderEntryFirstReleaseLinksToHistory(t *testing.T) {
	rel := fixture()
	rel.PreviousTag = ""

	got, err := RenderEntry(rel, Options{})
	if err != nil {
		t.Fatalf("RenderEntry: %v", err)
	}
	if !strings.HasPrefix(got, "## [1.4.0](https://github.com/teddynted/repo/commits/v1.4.0) - 2026-07-10") {
		t.Errorf("a first entry should link to the commit history\n---\n%s", got)
	}
}

func TestRenderEntryWithoutRepository(t *testing.T) {
	rel := fixture()
	rel.Repo = Repository{}

	got, err := RenderEntry(rel, Options{})
	if err != nil {
		t.Fatalf("RenderEntry: %v", err)
	}
	if !strings.HasPrefix(got, "## [1.4.0] - 2026-07-10") {
		t.Errorf("an unlinked heading should still carry the version and date\n---\n%s", got)
	}
}

func TestNewData(t *testing.T) {
	data := NewData(fixture(), DefaultCategories())

	if data.Date != "2026-07-10" {
		t.Errorf("Date = %q, want an ISO-8601 date", data.Date)
	}
	if data.Version != "1.4.0" || data.Tag != "v1.4.0" {
		t.Errorf("Version/Tag = %q/%q", data.Version, data.Tag)
	}
	if data.IsFirstRelease {
		t.Error("IsFirstRelease should be false when there is a previous tag")
	}
	if data.Bump != "minor" {
		t.Errorf("Bump = %q", data.Bump)
	}
	if data.Stats.Commits != 5 || data.Stats.Breaking != 1 {
		t.Errorf("Stats = %+v", data.Stats)
	}
	if data.CompareURL == "" || data.HistoryURL != data.CompareURL {
		t.Errorf("CompareURL = %q, HistoryURL = %q", data.CompareURL, data.HistoryURL)
	}
}

func TestCustomTemplate(t *testing.T) {
	tmpl := template.Must(template.New("t").Parse(
		"{{.Tag}} {{.Bump}} {{.Stats.Commits}}\n{{range .Groups}}{{.Title}}={{len .Items}} {{end}}"))

	got := mustRenderNotes(t, fixture(), Options{Template: tmpl})
	if !strings.HasPrefix(got, "v1.4.0 minor 5\n") {
		t.Errorf("custom template output = %q", got)
	}
	if !strings.Contains(got, "Features=2") {
		t.Errorf("the breaking feat should be counted under Features too: %q", got)
	}
}

func TestCustomCategoriesChangeRendering(t *testing.T) {
	categories := []Category{
		{Key: "feat", Title: "New Stuff", Icon: "✨", Label: "New", Types: []string{"feat"}},
	}
	got := mustRenderNotes(t, fixture(), Options{Categories: categories})

	if !strings.Contains(got, "### ✨ New Stuff") {
		t.Errorf("custom categories should drive the headings\n---\n%s", got)
	}
	if strings.Contains(got, "Bug Fixes") {
		t.Errorf("a category that was not supplied should not appear\n---\n%s", got)
	}
}

func TestParseTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.tmpl")
	if err := os.WriteFile(path, []byte("Release {{.Tag}}"), 0o644); err != nil {
		t.Fatal(err)
	}

	tmpl, err := ParseTemplate(path)
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	if got := mustRenderNotes(t, fixture(), Options{Template: tmpl}); !strings.HasPrefix(got, "Release v1.4.0") {
		t.Errorf("rendered = %q", got)
	}
}

func TestParseTemplateErrors(t *testing.T) {
	if _, err := ParseTemplate(filepath.Join(t.TempDir(), "missing.tmpl")); err == nil {
		t.Error("a missing template file should be reported")
	}

	path := filepath.Join(t.TempDir(), "bad.tmpl")
	if err := os.WriteFile(path, []byte("{{.Unclosed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTemplate(path); err == nil {
		t.Error("a malformed template should be reported at parse time")
	}
}

// A template that references a field which does not exist must fail loudly
// rather than render a half-empty document.
func TestTemplateExecutionErrorIsReported(t *testing.T) {
	tmpl := template.Must(template.New("t").Parse("{{.NoSuchField}}"))
	if _, err := RenderNotes(fixture(), Options{Template: tmpl}); err == nil {
		t.Error("RenderNotes should surface a template execution error")
	}
}

// The built-in templates must always parse; a typo in them is a bug, not a
// user error.
func TestBuiltInTemplatesParse(t *testing.T) {
	for name, text := range map[string]string{
		"notes": DefaultNotesTemplate,
		"entry": DefaultEntryTemplate,
	} {
		if _, err := template.New(name).Parse(text); err != nil {
			t.Errorf("the built-in %s template does not parse: %v", name, err)
		}
	}
}
