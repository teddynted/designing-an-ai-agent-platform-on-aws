package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

func testPlan() *release.Plan {
	return &release.Plan{
		Bump:        semver.BumpMinor,
		Branch:      "main",
		Current:     semver.MustParse("1.2.3"),
		Next:        semver.MustParse("1.3.0"),
		PreviousTag: "v1.2.3",
		Tag:         "v1.3.0",
		Date:        time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
		Repo:        changelog.Repository{Host: "github.com", Owner: "teddynted", Name: "repo"},
		Commits: []changelog.Commit{
			{SHA: "aaaaaaa", Subject: "feat: add a thing"},
			{SHA: "bbbbbbb", Subject: "fix: correct a thing"},
		},
	}
}

func TestPlannedActions(t *testing.T) {
	opts := &releaseFlags{}
	opts.remote = "origin"

	want := []string{
		"create Git tag v1.3.0",
		"push tag to origin",
		"generate release notes",
		"create GitHub Release",
	}
	if got := plannedActions(testPlan(), opts); !slices.Equal(got, want) {
		t.Errorf("plannedActions() = %v\nwant %v", got, want)
	}
}

// Without a push, nothing downstream runs, so nothing downstream is promised.
func TestPlannedActionsWithoutPush(t *testing.T) {
	opts := &releaseFlags{noPush: true}
	opts.remote = "origin"

	got := plannedActions(testPlan(), opts)
	if !slices.Equal(got, []string{"create Git tag v1.3.0"}) {
		t.Errorf("plannedActions() with --no-push = %v", got)
	}
}

func TestPlannedActionsNamesTheRemote(t *testing.T) {
	opts := &releaseFlags{}
	opts.remote = "upstream"

	if got := plannedActions(testPlan(), opts); !slices.Contains(got, "push tag to upstream") {
		t.Errorf("plannedActions() should name the configured remote: %v", got)
	}
}

func TestPrintActionsConfirmed(t *testing.T) {
	p, _, errw := newTestPrinter()
	printActions(p, []string{"create Git tag v1.3.0", "push tag to origin"}, false)

	got := errw.String()
	if !strings.Contains(got, glyphSuccess+" Create Git tag v1.3.0") {
		t.Errorf("a real run should tick each action:\n%s", got)
	}
	if strings.Contains(got, "Would") {
		t.Errorf("a real run should not say \"Would\":\n%s", got)
	}
}

func TestPrintActionsDryRun(t *testing.T) {
	p, _, errw := newTestPrinter()
	printActions(p, []string{"create Git tag v1.3.0", "push tag to origin"}, true)

	got := errw.String()
	for _, want := range []string{
		glyphBullet + " Would create Git tag v1.3.0",
		glyphBullet + " Would push tag to origin",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, glyphSuccess) {
		t.Errorf("a dry run must not tick anything as done:\n%s", got)
	}
}

func TestPrintPlan(t *testing.T) {
	p, _, errw := newTestPrinter()
	printPlan(p, testPlan())

	got := errw.String()
	for _, want := range []string{
		"Release plan",
		"Repository",
		"teddynted/repo",
		"Branch",
		"main",
		"Current",
		"v1.2.3",
		"Next",
		"v1.3.0 (minor)",
		"Commits",
		"Release Date",
		"2026-07-10",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan missing %q:\n%s", want, got)
		}
	}
	// The awkward "1 commit"/"12 commits" wording is gone: it is just a number.
	if strings.Contains(got, "commits") || strings.Contains(got, "2 commit") {
		t.Errorf("the commit count should be a bare number:\n%s", got)
	}
}

func TestPrintPlanFirstRelease(t *testing.T) {
	plan := testPlan()
	plan.PreviousTag = ""

	p, _, errw := newTestPrinter()
	printPlan(p, plan)
	if !strings.Contains(errw.String(), "this is the first release") {
		t.Errorf("a first release should say so:\n%s", errw.String())
	}
}

// A repository with no usable remote still gets a plan, just without the row.
func TestPrintPlanWithoutRepository(t *testing.T) {
	plan := testPlan()
	plan.Repo = changelog.Repository{}

	p, _, errw := newTestPrinter()
	printPlan(p, plan)

	got := errw.String()
	if strings.Contains(got, "Repository") {
		t.Errorf("an unknown repository should be omitted, not shown blank:\n%s", got)
	}
	if !strings.Contains(got, "Branch") {
		t.Errorf("the rest of the plan should still print:\n%s", got)
	}
}

func TestPrintStatistics(t *testing.T) {
	stats := changelog.NewData(changelog.Release{
		Bump: "minor",
		Commits: []changelog.Commit{
			{SHA: "a", Subject: "feat: one"},
			{SHA: "b", Subject: "feat: two"},
			{SHA: "c", Subject: "fix: three"},
			{SHA: "d", Subject: "docs: four"},
			{SHA: "e", Subject: "mystery"},
		},
	}, changelog.DefaultCategories()).Stats

	p, _, errw := newTestPrinter()
	printStatistics(p, stats)

	got := errw.String()
	for _, want := range []string{
		"Release Statistics",
		"Version Bump",
		"Minor",
		"Commits",
		"Features",
		"Fixes",
		"Documentation",
		"Other",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("statistics missing %q:\n%s", want, got)
		}
	}
	// Categories with no commits are omitted, not printed as zero.
	if strings.Contains(got, "Chores") {
		t.Errorf("an empty category should be omitted:\n%s", got)
	}
}

func TestPrintStatisticsBreakingIsHighlighted(t *testing.T) {
	stats := changelog.NewData(changelog.Release{
		Bump:    "major",
		Commits: []changelog.Commit{{SHA: "a", Subject: "feat!: drop v1"}},
	}, changelog.DefaultCategories()).Stats

	p, _, errw := newTestPrinter()
	printStatistics(p, stats)
	if !strings.Contains(errw.String(), "Breaking") {
		t.Errorf("a breaking release should say so:\n%s", errw.String())
	}
}

func TestPrintStatisticsOmitsUnknownBump(t *testing.T) {
	// A snapshot of an existing first tag has no bump to report.
	stats := changelog.NewData(changelog.Release{
		Commits: []changelog.Commit{{SHA: "a", Subject: "feat: one"}},
	}, changelog.DefaultCategories()).Stats

	p, _, errw := newTestPrinter()
	printStatistics(p, stats)
	if strings.Contains(errw.String(), "Version Bump") {
		t.Errorf("an unknown bump should be omitted:\n%s", errw.String())
	}
}
