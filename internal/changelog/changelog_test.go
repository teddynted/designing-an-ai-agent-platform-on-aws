package changelog_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

func notes(v string, sections map[release.Category][]string) release.Notes {
	base := version.MustParse("0.1.0")
	head := version.MustParse(v)
	return release.Notes{
		Version:  head,
		Date:     release.MustDate("2026-07-10"),
		Summary:  "2 commits across 1 file, with 10 insertions and 2 deletions.",
		Sections: sections,
		Comparison: &git.Comparison{
			Head: head,
			Base: &base,
		},
	}
}

func TestEntryRendersKeepAChangelogOrder(t *testing.T) {
	// Supplied out of order on purpose: rendering must impose the spec's order.
	got := changelog.Entry(notes("0.2.0", map[release.Category][]string{
		release.Fixed:    {"Correct the parser (bbbbbbb)"},
		release.Added:    {"Add the roadmap (aaaaaaa)"},
		release.Security: {"Pin the action (ccccccc)"},
		release.Changed:  {"Rework routing (ddddddd)"},
	}))

	want := `## [0.2.0] - 2026-07-10

### Added

- Add the roadmap (aaaaaaa)

### Changed

- Rework routing (ddddddd)

### Fixed

- Correct the parser (bbbbbbb)

### Security

- Pin the action (ccccccc)
`
	if got != want {
		t.Errorf("Entry() =\n%q\nwant\n%q", got, want)
	}
}

func TestEntryOmitsEmptySections(t *testing.T) {
	got := changelog.Entry(notes("0.2.0", map[release.Category][]string{
		release.Added:   {"Only an addition"},
		release.Removed: {}, // present but empty
	}))
	if strings.Contains(got, "Removed") {
		t.Errorf("an empty section should not render:\n%s", got)
	}
	if !strings.Contains(got, "### Added") {
		t.Errorf("a populated section should render:\n%s", got)
	}
}

// A re-tag produces no entries. The changelog must say so rather than trail off
// into a bare heading.
func TestEntryOnAnEmptyRelease(t *testing.T) {
	got := changelog.Entry(notes("0.2.0", nil))
	if !strings.Contains(got, "No user-facing changes.") {
		t.Errorf("an empty release should say so:\n%s", got)
	}
	if !strings.Contains(got, "## [0.2.0] - 2026-07-10") {
		t.Errorf("the heading should still render:\n%s", got)
	}
}

func TestReleaseBodyHasSummaryAndCompareLink(t *testing.T) {
	got := changelog.ReleaseBody(
		notes("0.2.0", map[release.Category][]string{release.Added: {"Add a thing"}}),
		"teddynted/platform",
	)

	if !strings.HasPrefix(got, "2 commits across 1 file") {
		t.Errorf("the release body leads with the summary:\n%s", got)
	}
	if strings.Contains(got, "## [0.2.0]") {
		t.Errorf("the release body does not repeat the version heading:\n%s", got)
	}
	want := "**Full changelog:** [v0.1.0...v0.2.0](https://github.com/teddynted/platform/compare/v0.1.0...v0.2.0)"
	if !strings.Contains(got, want) {
		t.Errorf("release body should end with a compare link:\n%s", got)
	}
}

// The first release has no predecessor to compare against.
func TestReleaseBodyOmitsCompareLinkForTheFirstRelease(t *testing.T) {
	head := version.MustParse("0.1.0")
	initial := release.Notes{
		Version:    head,
		Date:       release.MustDate("2026-07-10"),
		Summary:    "Initial release. 1 commit across 2 files.",
		Sections:   map[release.Category][]string{release.Added: {"Everything"}},
		Comparison: &git.Comparison{Head: head},
	}
	got := changelog.ReleaseBody(initial, "teddynted/platform")
	if strings.Contains(got, "Full changelog") {
		t.Errorf("the first release has nothing to compare against:\n%s", got)
	}
}

func TestReleaseBodyWithoutARepositoryOmitsTheLink(t *testing.T) {
	got := changelog.ReleaseBody(notes("0.2.0", map[release.Category][]string{release.Added: {"x"}}), "")
	if strings.Contains(got, "Full changelog") {
		t.Errorf("no repository means no link:\n%s", got)
	}
}

func TestContains(t *testing.T) {
	doc := changelog.Render([]release.Notes{
		notes("0.2.0", map[release.Category][]string{release.Added: {"a"}}),
		notes("0.1.0", map[release.Category][]string{release.Added: {"b"}}),
	})
	for _, v := range []string{"0.2.0", "0.1.0"} {
		if !changelog.Contains(doc, version.MustParse(v)) {
			t.Errorf("Contains(%s) = false", v)
		}
	}
	if changelog.Contains(doc, version.MustParse("0.3.0")) {
		t.Error("Contains(0.3.0) = true")
	}
}

func TestRenderPutsNewestFirst(t *testing.T) {
	got := changelog.Render([]release.Notes{
		notes("0.2.0", map[release.Category][]string{release.Added: {"newer"}}),
		notes("0.1.0", map[release.Category][]string{release.Added: {"older"}}),
	})
	if !strings.HasPrefix(got, changelog.Header) {
		t.Error("Render should begin with the header")
	}
	if strings.Index(got, "## [0.2.0]") > strings.Index(got, "## [0.1.0]") {
		t.Errorf("newest release should come first:\n%s", got)
	}
}

func TestInsertIntoAnEmptyChangelog(t *testing.T) {
	got := changelog.Insert("", notes("0.1.0", map[release.Category][]string{release.Added: {"Everything"}}))
	if !strings.HasPrefix(got, changelog.Header) {
		t.Errorf("an empty changelog gets a header:\n%s", got)
	}
	if !strings.Contains(got, "## [0.1.0]") {
		t.Errorf("the entry should be present:\n%s", got)
	}
}

func TestInsertAboveTheNewestEntry(t *testing.T) {
	existing := changelog.Render([]release.Notes{
		notes("0.1.0", map[release.Category][]string{release.Added: {"older"}}),
	})
	got := changelog.Insert(existing, notes("0.2.0", map[release.Category][]string{release.Added: {"newer"}}))

	if strings.Index(got, "## [0.2.0]") > strings.Index(got, "## [0.1.0]") {
		t.Errorf("new entry should be inserted above the old:\n%s", got)
	}
	if !strings.HasPrefix(got, changelog.Header) {
		t.Errorf("the preamble should survive:\n%s", got)
	}
	if !strings.Contains(got, "older") {
		t.Errorf("the old entry should survive:\n%s", got)
	}
}

func TestInsertIntoAHeaderOnlyChangelog(t *testing.T) {
	got := changelog.Insert(changelog.Header, notes("0.1.0", map[release.Category][]string{release.Added: {"x"}}))
	if !strings.Contains(got, "## [0.1.0]") {
		t.Errorf("entry missing:\n%s", got)
	}
	if strings.Count(got, "# Changelog") != 1 {
		t.Errorf("the header should not be duplicated:\n%s", got)
	}
}

// A release pipeline is retried more often than anyone plans for.
func TestInsertIsIdempotent(t *testing.T) {
	entry := notes("0.2.0", map[release.Category][]string{release.Added: {"Add a thing"}})
	once := changelog.Insert(changelog.Header, entry)
	twice := changelog.Insert(once, entry)

	if once != twice {
		t.Errorf("inserting the same version twice changed the document:\n%s", twice)
	}
	if strings.Count(twice, "## [0.2.0]") != 1 {
		t.Errorf("the version appears more than once:\n%s", twice)
	}
}

// Build metadata does not affect precedence, so 0.2.0 and 0.2.0+build.7 are the
// same release and must not both appear.
func TestInsertTreatsBuildMetadataAsTheSameVersion(t *testing.T) {
	once := changelog.Insert(changelog.Header, notes("0.2.0", map[release.Category][]string{release.Added: {"x"}}))
	twice := changelog.Insert(once, notes("0.2.0+build.7", map[release.Category][]string{release.Added: {"x"}}))
	if strings.Count(twice, "## [0.2.0") != 1 {
		t.Errorf("build metadata does not make a new release:\n%s", twice)
	}
}

func TestFileReadMissingReturnsHeader(t *testing.T) {
	got, err := changelog.NewFile(t.TempDir()).Read()
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if got != changelog.Header {
		t.Errorf("a missing changelog should read as the bare header, got %q", got)
	}
}

func TestFileInsertReportsWhetherItChanged(t *testing.T) {
	root := t.TempDir()
	file := changelog.NewFile(root)
	entry := notes("0.2.0", map[release.Category][]string{release.Added: {"Add a thing"}})

	changed, err := file.Insert(entry)
	if err != nil {
		t.Fatalf("Insert returned error: %v", err)
	}
	if !changed {
		t.Error("the first insert changes the file")
	}

	changed, err = file.Insert(entry)
	if err != nil {
		t.Fatalf("Insert returned error: %v", err)
	}
	if changed {
		t.Error("re-inserting the same version should not change the file")
	}

	raw, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "## [0.2.0]") != 1 {
		t.Errorf("duplicate entry written:\n%s", raw)
	}
}
