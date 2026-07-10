package roadmap_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/roadmap"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

const sample = `releases:
  - version: 0.2.0
    title: Release management
    status: planned
  - version: 0.1.0
    title: Initial architecture
    status: released
    date: 2026-07-09
    milestone: 1
    summary: Design and documentation only.
    highlights:
      - Three-plane decomposition
      - Model Gateway seam
`

func TestParseSortsAscendingAndReadsEveryField(t *testing.T) {
	registry, err := roadmap.Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(registry.Releases) != 2 {
		t.Fatalf("parsed %d releases, want 2", len(registry.Releases))
	}

	// Given newest-first in the file, the registry orders ascending.
	first := registry.Releases[0]
	if first.Version.String() != "0.1.0" {
		t.Errorf("releases[0] = %s, want 0.1.0", first.Version)
	}
	if first.Title != "Initial architecture" || !first.IsReleased() {
		t.Errorf("releases[0] = %+v", first)
	}
	if first.Milestone != 1 || !first.HasMilestone() {
		t.Errorf("milestone = %d", first.Milestone)
	}
	if got := first.Date.Format(release.DateFormat); got != "2026-07-09" {
		t.Errorf("date = %s", got)
	}
	if len(first.Highlights) != 2 {
		t.Errorf("highlights = %v", first.Highlights)
	}

	second := registry.Releases[1]
	if second.Status != release.Planned || second.HasMilestone() {
		t.Errorf("releases[1] = %+v", second)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"bad version", "releases:\n  - version: 1.2\n    title: T\n    status: planned\n", "SemVer"},
		{"bad status", "releases:\n  - version: 1.0.0\n    title: T\n    status: shipped\n", "unknown status"},
		{"bad date", "releases:\n  - version: 1.0.0\n    title: T\n    status: released\n    date: 10-07-2026\n", "YYYY-MM-DD"},
		{"no title", "releases:\n  - version: 1.0.0\n    status: planned\n", "no title"},
		{"duplicate", "releases:\n  - version: 1.0.0\n    title: A\n    status: planned\n  - version: 1.0.0\n    title: B\n    status: planned\n", "appears twice"},
		// A released version with no date cannot be ordered on the roadmap.
		{"released without a date", "releases:\n  - version: 1.0.0\n    title: T\n    status: released\n", "no date"},
		{"not yaml", "releases: [unclosed\n", "valid YAML"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := roadmap.Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	registry, err := roadmap.Parse([]byte("releases: []\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(registry.Releases) != 0 {
		t.Errorf("expected no releases, got %v", registry.Releases)
	}
	if registry.Latest() != nil || registry.Next() != nil {
		t.Error("an empty registry has no latest and no next")
	}
}

func TestRoundTrip(t *testing.T) {
	original, err := roadmap.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	data, err := original.Marshal()
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	reparsed, err := roadmap.Parse(data)
	if err != nil {
		t.Fatalf("re-parsing our own output failed: %v\n%s", err, data)
	}
	if len(reparsed.Releases) != len(original.Releases) {
		t.Fatalf("round trip lost releases")
	}
	for i := range original.Releases {
		a, b := original.Releases[i], reparsed.Releases[i]
		if !a.Version.Equal(b.Version) || a.Title != b.Title || a.Status != b.Status || !a.Date.Equal(b.Date) {
			t.Errorf("release %d changed across a round trip:\n%+v\n%+v", i, a, b)
		}
		if a.Milestone != b.Milestone {
			t.Errorf("milestone %d changed across a round trip", i)
		}
	}
}

// A planned release has no date, and the field must not be emitted as an empty
// string that then fails to parse.
func TestMarshalOmitsAbsentDate(t *testing.T) {
	registry := &roadmap.Registry{Releases: []release.Release{
		{Version: version.MustParse("0.3.0"), Title: "Next", Status: release.Planned},
	}}
	data, err := registry.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "date:") {
		t.Errorf("a planned release should carry no date field:\n%s", data)
	}
	if _, err := roadmap.Parse(data); err != nil {
		t.Errorf("our own output should re-parse: %v", err)
	}
}

func TestLatestAndNext(t *testing.T) {
	registry, err := roadmap.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	latest := registry.Latest()
	if latest == nil || latest.Version.String() != "0.1.0" {
		t.Errorf("Latest() = %v, want 0.1.0", latest)
	}
	next := registry.Next()
	if next == nil || next.Version.String() != "0.2.0" {
		t.Errorf("Next() = %v, want 0.2.0", next)
	}
	if planned := registry.Planned(); len(planned) != 1 || planned[0].Version.String() != "0.2.0" {
		t.Errorf("Planned() = %v", planned)
	}
}

func TestFind(t *testing.T) {
	registry, _ := roadmap.Parse([]byte(sample))
	if got := registry.Find(version.MustParse("0.2.0")); got == nil || got.Title != "Release management" {
		t.Errorf("Find(0.2.0) = %v", got)
	}
	if got := registry.Find(version.MustParse("9.9.9")); got != nil {
		t.Errorf("Find(9.9.9) = %v, want nil", got)
	}
}

func TestUpsertReplacesInPlace(t *testing.T) {
	registry, _ := roadmap.Parse([]byte(sample))
	err := registry.Upsert(release.Release{
		Version: version.MustParse("0.2.0"),
		Title:   "Renamed",
		Status:  release.InProgress,
	})
	if err != nil {
		t.Fatalf("Upsert returned error: %v", err)
	}
	if len(registry.Releases) != 2 {
		t.Errorf("Upsert added a duplicate: %v", registry.Releases)
	}
	if got := registry.Find(version.MustParse("0.2.0")); got.Title != "Renamed" {
		t.Errorf("Upsert did not replace: %+v", got)
	}
}

func TestUpsertAddsAndSorts(t *testing.T) {
	registry, _ := roadmap.Parse([]byte(sample))
	if err := registry.Upsert(release.Release{
		Version: version.MustParse("0.1.5"),
		Title:   "Between",
		Status:  release.Planned,
	}); err != nil {
		t.Fatal(err)
	}
	if len(registry.Releases) != 3 {
		t.Fatalf("expected 3 releases, got %d", len(registry.Releases))
	}
	if registry.Releases[1].Version.String() != "0.1.5" {
		t.Errorf("Upsert did not sort: %v", registry.Releases)
	}
}

func TestUpsertRejectsInvalid(t *testing.T) {
	registry := &roadmap.Registry{}
	err := registry.Upsert(release.Release{
		Version: version.MustParse("1.0.0"),
		Title:   "T",
		Status:  release.Released, // released with no date
	})
	if err == nil {
		t.Error("Upsert should reject a released version with no date")
	}
}

func TestMarkReleased(t *testing.T) {
	registry, _ := roadmap.Parse([]byte(sample))
	when := release.MustDate("2026-07-10")

	if err := registry.MarkReleased(version.MustParse("0.2.0"), "Release management", when); err != nil {
		t.Fatalf("MarkReleased returned error: %v", err)
	}
	got := registry.Find(version.MustParse("0.2.0"))
	if !got.IsReleased() || !got.Date.Equal(when) {
		t.Errorf("release not marked: %+v", got)
	}
	// The title from the roadmap survives; MarkReleased does not overwrite it.
	if got.Title != "Release management" {
		t.Errorf("title = %q", got.Title)
	}
}

// A release cut without a roadmap entry is a real thing that happens; failing
// the pipeline over bookkeeping would be worse than recording it.
func TestMarkReleasedAddsAnAbsentVersion(t *testing.T) {
	registry, _ := roadmap.Parse([]byte(sample))
	when := release.MustDate("2026-07-11")

	if err := registry.MarkReleased(version.MustParse("0.9.0"), "Unplanned", when); err != nil {
		t.Fatalf("MarkReleased returned error: %v", err)
	}
	got := registry.Find(version.MustParse("0.9.0"))
	if got == nil || !got.IsReleased() || got.Title != "Unplanned" {
		t.Errorf("absent version not added: %v", got)
	}
}

// This repository's own roadmap must parse. A typo in RELEASES.yaml would
// otherwise only surface during a release, which is the worst moment to find it.
func TestRepositoryRoadmapParses(t *testing.T) {
	registry, err := roadmap.NewFile(filepath.Join("..", "..")).Load()
	if err != nil {
		t.Fatalf("the repository's RELEASES.yaml does not parse: %v", err)
	}
	if len(registry.Releases) == 0 {
		t.Fatal("the repository's RELEASES.yaml lists no releases")
	}
	for _, rel := range registry.Releases {
		if err := rel.Validate(); err != nil {
			t.Errorf("%s: %v", rel.Tag(), err)
		}
	}
	// v0.1.0 shipped, and it delivers milestone 1.
	shipped := registry.Find(version.MustParse("0.1.0"))
	if shipped == nil || !shipped.IsReleased() || shipped.Milestone != 1 {
		t.Errorf("v0.1.0 should be a released milestone 1: %+v", shipped)
	}
}

func TestFileLoadMissingIsEmptyNotAnError(t *testing.T) {
	file := roadmap.NewFile(t.TempDir())
	if file.Exists() {
		t.Fatal("RELEASES.yaml should not exist")
	}
	registry, err := file.Load()
	if err != nil {
		t.Fatalf("a missing roadmap is not an error: %v", err)
	}
	if len(registry.Releases) != 0 {
		t.Errorf("expected an empty registry, got %v", registry.Releases)
	}
}

func TestFileSaveAndLoad(t *testing.T) {
	root := t.TempDir()
	file := roadmap.NewFile(root)

	original, err := roadmap.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Save(original); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if !file.Exists() {
		t.Error("RELEASES.yaml should exist after Save")
	}

	loaded, err := file.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(loaded.Releases) != 2 {
		t.Errorf("loaded %d releases, want 2", len(loaded.Releases))
	}
	if loaded.Latest().Version.String() != "0.1.0" {
		t.Errorf("Latest() = %v", loaded.Latest())
	}
}
