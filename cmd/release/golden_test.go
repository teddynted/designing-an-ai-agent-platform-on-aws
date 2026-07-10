package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// update rewrites the golden files instead of comparing against them:
//
//	go test ./cmd/release -update
//
// Review the resulting diff. A golden file exists to make an accidental change
// to the report visible in code review, so regenerating it without reading it
// defeats the point.
var update = flag.Bool("update", false, "rewrite the golden files")

// Fixed dates, so the golden files do not change with the clock.
var (
	goldenDate         = time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	goldenPreviousDate = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
)

// assertGolden compares output with testdata/<name>.golden.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the golden file: %v\nrun `go test ./cmd/release -update` to create it", err)
	}
	if got != string(want) {
		t.Errorf("output does not match %s\n\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// goldenPrinter renders at a fixed width with no colour, so the golden files
// are stable wherever the tests run.
func goldenPrinter() (*printer, *strings.Builder, *strings.Builder) {
	var out, errw strings.Builder
	return newPrinter(&out, &errw, options{width: 76}), &out, &errw
}

// goldenPlan is a release with something in every section: two authors, a
// breaking change, and a measurable diff.
func goldenPlan() *release.Plan {
	return &release.Plan{
		Bump:         semver.BumpMinor,
		Branch:       "main",
		HeadSHA:      "486bcb2e40fe1b3596d5733bd5ac6da6941b141a",
		Current:      semver.MustParse("1.2.3"),
		Next:         semver.MustParse("1.3.0"),
		PreviousTag:  "v1.2.3",
		Tag:          "v1.3.0",
		Date:         goldenDate,
		PreviousDate: goldenPreviousDate,
		Diff:         git.DiffStat{Files: 30, Insertions: 2881, Deletions: 651},
		Repo:         changelog.Repository{Host: "github.com", Owner: "teddynted", Name: "repo"},
		Commits: []changelog.Commit{
			{SHA: "aaaaaaabbbbbbb", Subject: "feat(cli): add --dry-run", AuthorName: "Teddy Kekana", AuthorEmail: "teddy@example.com"},
			{SHA: "cccccccddddddd", Subject: "feat!: require Go 1.25", Body: "BREAKING CHANGE: Go 1.24 is no longer supported", AuthorName: "Teddy Kekana", AuthorEmail: "teddy@example.com"},
			{SHA: "eeeeeeefffffff", Subject: "fix: handle an empty tag list.", AuthorName: "Ada Lovelace", AuthorEmail: "ada@example.com"},
			{SHA: "1111111222222", Subject: "docs: explain the flags", AuthorName: "Ada Lovelace", AuthorEmail: "ada@example.com"},
			{SHA: "3333333444444", Subject: "chore: bump the linter", AuthorName: "Teddy Kekana", AuthorEmail: "teddy@example.com"},
		},
	}
}

// TestGoldenDryRunReport pins the whole report, section by section, so that an
// accidental change to spacing, ordering, or wording shows up as a diff.
//
// The timing section is excluded on purpose: its values are measured, so they
// differ on every run.
func TestGoldenDryRunReport(t *testing.T) {
	plan := goldenPlan()
	health := testHealth()

	opts := testFlags()
	opts.dryRun = true

	p, _, errw := goldenPrinter()
	p.dryRunBanner()
	printHealth(p, health)
	printPlan(p, plan, opts.remote)
	printVersion(p, plan)
	printActions(p, plannedActions(plan, opts), true)
	printStatistics(p, changelog.NewData(plan.Release(), changelog.DefaultCategories()).Stats, plan.Diff)
	printContributors(p, plan.Contributors())
	printConfidence(p, health)
	printDryRunSummary(p, plan, opts, semver.BumpMinor)

	assertGolden(t, "dry_run_report", errw.String())
}

// TestGoldenDryRunWithWarnings pins the degraded path: warnings lower the
// rating, and every one of them is explained beneath it.
func TestGoldenDryRunWithWarnings(t *testing.T) {
	health := testHealth()
	health.Checks[2] = release.Check{Name: "Working tree", Level: release.LevelWarn, Detail: "3 changes uncommitted, allowed by --allow-dirty"}
	health.Checks[5] = release.Check{Name: "GitHub authentication", Level: release.LevelWarn, Detail: "GITHUB_TOKEN is not set; the release workflow will publish"}

	p, _, errw := goldenPrinter()
	printHealth(p, health)
	printConfidence(p, health)

	assertGolden(t, "warnings_report", errw.String())
}

// TestGoldenReleaseNotes pins the published Markdown: the summary, the
// categories, the contributors, and the footer.
func TestGoldenReleaseNotes(t *testing.T) {
	notes, err := changelog.RenderNotes(goldenPlan().Release(), changelog.Options{})
	if err != nil {
		t.Fatalf("RenderNotes: %v", err)
	}
	assertGolden(t, "release_notes", notes)
}

// TestGoldenFirstReleaseNotes pins the first-release footer, which has no
// comparison to link to.
func TestGoldenFirstReleaseNotes(t *testing.T) {
	plan := goldenPlan()
	plan.PreviousTag = ""

	notes, err := changelog.RenderNotes(plan.Release(), changelog.Options{})
	if err != nil {
		t.Fatalf("RenderNotes: %v", err)
	}
	assertGolden(t, "first_release_notes", notes)
}

// TestGoldenChangelogEntry pins what lands in CHANGELOG.md.
func TestGoldenChangelogEntry(t *testing.T) {
	entry, err := changelog.RenderEntry(goldenPlan().Release(), changelog.Options{})
	if err != nil {
		t.Fatalf("RenderEntry: %v", err)
	}
	assertGolden(t, "changelog_entry", entry)
}

// TestGoldenASCIIReport pins the fallback rendering, so a terminal without
// UTF-8 is never forgotten.
func TestGoldenASCIIReport(t *testing.T) {
	var out, errw strings.Builder
	p := newPrinter(&out, &errw, options{width: 76, ascii: true})

	printHealth(p, testHealth())
	printConfidence(p, testHealth())

	assertGolden(t, "ascii_report", errw.String())
}
