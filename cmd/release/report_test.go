package main

import (
	"strings"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

func testPlan() *release.Plan {
	return &release.Plan{
		Bump:         semver.BumpMinor,
		Branch:       "main",
		HeadSHA:      "486bcb2e40fe1b3596d5733bd5ac6da6941b141a",
		Current:      semver.MustParse("1.2.3"),
		Next:         semver.MustParse("1.3.0"),
		PreviousTag:  "v1.2.3",
		Tag:          "v1.3.0",
		Date:         time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
		PreviousDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Diff:         git.DiffStat{Files: 30, Insertions: 2881, Deletions: 651},
		Repo:         changelog.Repository{Host: "github.com", Owner: "teddynted", Name: "repo"},
		Commits: []changelog.Commit{
			{SHA: "aaaaaaa", Subject: "feat: add a thing", AuthorName: "Teddy Kekana", AuthorEmail: "teddy@example.com"},
			{SHA: "bbbbbbb", Subject: "fix: correct a thing", AuthorName: "Ada Lovelace", AuthorEmail: "ada@example.com"},
		},
	}
}

func testHealth() release.Health {
	return release.Health{
		Branch: "main",
		Checks: []release.Check{
			{Name: "Git repository", Level: release.LevelOK, Detail: "inside a Git work tree"},
			{Name: "Release branch", Level: release.LevelOK, Detail: "on main"},
			{Name: "Working tree", Level: release.LevelOK, Detail: "clean"},
			{Name: "Untracked files", Level: release.LevelOK, Detail: "none"},
			{Name: "Branch synchronised", Level: release.LevelOK, Detail: "up to date with origin/main"},
			{Name: "GitHub authentication", Level: release.LevelOK, Detail: "GITHUB_TOKEN is set"},
		},
	}
}

func TestPrintHealth(t *testing.T) {
	p, _, errw := newTestPrinter()
	printHealth(p, testHealth())

	got := errw.String()
	if !strings.Contains(got, "Validation") {
		t.Errorf("missing the section heading:\n%s", got)
	}
	th := unicodeTheme()
	if !strings.Contains(got, th.success+" Git repository "+th.dash+" inside a Git work tree") {
		t.Errorf("check line is malformed:\n%s", got)
	}
}

func TestPrintHealthMarksEachLevel(t *testing.T) {
	health := release.Health{Checks: []release.Check{
		{Name: "A", Level: release.LevelOK, Detail: "fine"},
		{Name: "B", Level: release.LevelWarn, Detail: "hmm"},
		{Name: "C", Level: release.LevelFail, Detail: "no"},
	}}

	p, _, errw := newTestPrinter()
	printHealth(p, health)

	th := unicodeTheme()
	got := errw.String()
	for glyph, name := range map[string]string{th.success: "A", th.warning: "B", th.failure: "C"} {
		if !strings.Contains(got, glyph+" "+name) {
			t.Errorf("check %s should carry glyph %q:\n%s", name, glyph, got)
		}
	}
}

func TestReplaceCheck(t *testing.T) {
	health := testHealth()
	position := len(health.Checks) - 1

	updated := replaceCheck(health, release.Check{
		Name: "GitHub authentication", Level: release.LevelFail, Detail: "rejected",
	})
	if updated.Checks[position].Detail != "rejected" {
		t.Errorf("the check was not replaced in place: %+v", updated.Checks)
	}
	if len(updated.Checks) != len(health.Checks) {
		t.Errorf("replacing should not change the number of checks")
	}
}

// A check that does not exist yet is appended rather than dropped.
func TestReplaceCheckAppendsUnknown(t *testing.T) {
	health := release.Health{}
	updated := replaceCheck(health, release.Check{Name: "New", Level: release.LevelOK})
	if len(updated.Checks) != 1 {
		t.Errorf("an unknown check should be appended: %+v", updated.Checks)
	}
}

func TestPrintVersion(t *testing.T) {
	p, _, errw := newTestPrinter()
	printVersion(p, testPlan())

	got := errw.String()
	for _, want := range []string{
		"Version Information",
		"Current Version",
		"v1.2.3",
		"Next Version",
		"v1.3.0",
		"Increment Type",
		"Minor",
		"Previous Release",
		"2026-07-01",
		"Days Since",
		"9 days ago",
		"Release Date",
		"2026-07-10",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("version information missing %q:\n%s", want, got)
		}
	}
}

func TestPrintVersionFirstRelease(t *testing.T) {
	plan := testPlan()
	plan.PreviousTag = ""
	plan.PreviousDate = time.Time{}

	p, _, errw := newTestPrinter()
	printVersion(p, plan)

	got := errw.String()
	if !strings.Contains(got, "this is the first release") {
		t.Errorf("a first release should say so:\n%s", got)
	}
	// There is no previous release, so neither row is invented.
	if strings.Contains(got, "Previous Release") || strings.Contains(got, "Days Since") {
		t.Errorf("a first release has no previous date:\n%s", got)
	}
}

func TestPrintVersionShowsPrerelease(t *testing.T) {
	plan := testPlan()
	plan.Next = semver.MustParse("1.3.0-rc.0")
	plan.Tag = "v1.3.0-rc.0"

	p, _, errw := newTestPrinter()
	printVersion(p, plan)
	if !strings.Contains(errw.String(), "rc.0") {
		t.Errorf("a pre-release should be labelled:\n%s", errw.String())
	}
}

func TestElapsedDays(t *testing.T) {
	for days, want := range map[int]string{0: "today", 1: "yesterday", 2: "2 days ago", 30: "30 days ago"} {
		if got := elapsedDays(days); got != want {
			t.Errorf("elapsedDays(%d) = %q, want %q", days, got, want)
		}
	}
}

func TestPrintPlan(t *testing.T) {
	p, _, errw := newTestPrinter()
	printPlan(p, testPlan(), "origin")

	got := errw.String()
	for _, want := range []string{"Release Plan", "teddynted/repo", "main", "origin", "486bcb2"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan missing %q:\n%s", want, got)
		}
	}
	// The full SHA is noise; the short one is what people quote.
	if strings.Contains(got, "486bcb2e40fe1b") {
		t.Errorf("the commit should be abbreviated:\n%s", got)
	}
}

// A repository with no usable remote still gets a plan, just without the row.
func TestPrintPlanWithoutRepository(t *testing.T) {
	plan := testPlan()
	plan.Repo = changelog.Repository{}

	p, _, errw := newTestPrinter()
	printPlan(p, plan, "origin")

	got := errw.String()
	if strings.Contains(got, "Repository") {
		t.Errorf("an unknown repository should be omitted, not shown blank:\n%s", got)
	}
	if !strings.Contains(got, "Branch") {
		t.Errorf("the rest of the plan should still print:\n%s", got)
	}
}

func TestPlannedActions(t *testing.T) {
	opts := testFlags()

	want := []string{
		"create Git tag v1.3.0",
		"push tag to origin",
		"generate release notes",
		"create GitHub Release",
	}
	got := plannedActions(testPlan(), opts)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("plannedActions() = %v\nwant %v", got, want)
	}
}

// Without a push, nothing downstream runs, so nothing downstream is promised.
func TestPlannedActionsWithoutPush(t *testing.T) {
	opts := testFlags()
	opts.noPush = true

	if got := plannedActions(testPlan(), opts); len(got) != 1 {
		t.Errorf("plannedActions() with --no-push = %v", got)
	}
}

func TestPrintActionsConfirmed(t *testing.T) {
	p, _, errw := newTestPrinter()
	printActions(p, []string{"create Git tag v1.3.0"}, false)

	got := errw.String()
	if !strings.Contains(got, unicodeTheme().success+" Create Git tag v1.3.0") {
		t.Errorf("a real run should tick each action:\n%s", got)
	}
	if strings.Contains(got, "Would") {
		t.Errorf("a real run should not say \"Would\":\n%s", got)
	}
}

func TestPrintActionsDryRun(t *testing.T) {
	p, _, errw := newTestPrinter()
	printActions(p, []string{"create Git tag v1.3.0"}, true)

	got := errw.String()
	if !strings.Contains(got, unicodeTheme().bullet+" Would create Git tag v1.3.0") {
		t.Errorf("a dry run should use the conditional:\n%s", got)
	}
	if strings.Contains(got, unicodeTheme().success) {
		t.Errorf("a dry run must not tick anything as done:\n%s", got)
	}
}

func statsOf(plan *release.Plan) changelog.Stats {
	return changelog.NewData(plan.Release(), changelog.DefaultCategories()).Stats
}

func TestPrintStatistics(t *testing.T) {
	plan := testPlan()

	p, _, errw := newTestPrinter()
	printStatistics(p, statsOf(plan), plan.Diff)

	got := errw.String()
	for _, want := range []string{
		"Release Statistics",
		"Total Commits",
		"Features",
		"Fixes",
		"Files Changed",
		"30",
		"Lines Added",
		"+2,881",
		"Lines Removed",
		"-651",
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

// A repository git could not measure reports no diff at all, rather than zeros.
func TestPrintStatisticsOmitsAnUnknownDiff(t *testing.T) {
	plan := testPlan()
	plan.Diff = git.DiffStat{}

	p, _, errw := newTestPrinter()
	printStatistics(p, statsOf(plan), plan.Diff)

	if strings.Contains(errw.String(), "Files Changed") {
		t.Errorf("an unmeasured diff should be omitted:\n%s", errw.String())
	}
}

func TestPrintStatisticsHighlightsBreakingChanges(t *testing.T) {
	plan := testPlan()
	plan.Commits = []changelog.Commit{{SHA: "a", Subject: "feat!: drop v1"}}

	p, _, errw := newTestPrinter()
	printStatistics(p, statsOf(plan), git.DiffStat{})
	if !strings.Contains(errw.String(), "Breaking Changes") {
		t.Errorf("a breaking release should say so:\n%s", errw.String())
	}
}

func TestSigned(t *testing.T) {
	if got := signed("+", 2881); got != "+2,881" {
		t.Errorf("signed(+, 2881) = %q", got)
	}
	// "-0" reads as a mistake.
	if got := signed("-", 0); got != "0" {
		t.Errorf("signed(-, 0) = %q, want 0", got)
	}
}

func TestHumanInt(t *testing.T) {
	for in, want := range map[int]string{
		0: "0", 1: "1", 999: "999", 1000: "1,000",
		2881: "2,881", 12345: "12,345", 1234567: "1,234,567",
		-1234: "-1,234",
	} {
		if got := humanInt(in); got != want {
			t.Errorf("humanInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPrintContributors(t *testing.T) {
	p, _, errw := newTestPrinter()
	printContributors(p, testPlan().Contributors())

	got := errw.String()
	if !strings.Contains(got, "Contributors") {
		t.Errorf("missing the section heading:\n%s", got)
	}
	for _, want := range []string{"Teddy Kekana (1 commit)", "Ada Lovelace (1 commit)"} {
		if !strings.Contains(got, want) {
			t.Errorf("contributors missing %q:\n%s", want, got)
		}
	}
}

// Author information is not always available. That must not print an empty
// section.
func TestPrintContributorsOmitsAnEmptySection(t *testing.T) {
	p, _, errw := newTestPrinter()
	printContributors(p, nil)
	if errw.String() != "" {
		t.Errorf("an authorless release should print no section, got %q", errw.String())
	}
}

func TestConfidence(t *testing.T) {
	tests := []struct {
		name      string
		health    release.Health
		wantStars int
		wantLabel string
	}{
		{"all clear", testHealth(), 5, "Ready to release"},
		{
			name: "one warning",
			health: release.Health{Checks: []release.Check{
				{Level: release.LevelOK}, {Level: release.LevelWarn},
			}},
			wantStars: 4, wantLabel: "Ready with warnings",
		},
		{
			name: "many warnings never fall below two stars",
			health: release.Health{Checks: []release.Check{
				{Level: release.LevelWarn}, {Level: release.LevelWarn},
				{Level: release.LevelWarn}, {Level: release.LevelWarn},
				{Level: release.LevelWarn}, {Level: release.LevelWarn},
			}},
			wantStars: 2, wantLabel: "Ready with warnings",
		},
		{
			name: "a failure is not a low rating, it is a blocked release",
			health: release.Health{Checks: []release.Check{
				{Level: release.LevelFail},
			}},
			wantStars: 1, wantLabel: "Not ready to release",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stars, label := confidence(tt.health)
			if stars != tt.wantStars || label != tt.wantLabel {
				t.Errorf("confidence() = %d, %q; want %d, %q", stars, label, tt.wantStars, tt.wantLabel)
			}
		})
	}
}

// A rating without its reasons is decoration.
func TestPrintConfidenceExplainsItsWarnings(t *testing.T) {
	health := testHealth()
	health.Checks[2] = release.Check{Name: "Working tree", Level: release.LevelWarn, Detail: "3 changes uncommitted"}

	p, _, errw := newTestPrinter()
	printConfidence(p, health)

	got := errw.String()
	if !strings.Contains(got, "Ready with warnings") {
		t.Errorf("missing the label:\n%s", got)
	}
	if !strings.Contains(got, "3 changes uncommitted") {
		t.Errorf("the warning should be explained:\n%s", got)
	}
	if !strings.Contains(got, "★★★★☆") {
		t.Errorf("one warning should cost one star:\n%s", got)
	}
}

func TestPrintConfidenceAllClear(t *testing.T) {
	p, _, errw := newTestPrinter()
	printConfidence(p, testHealth())

	got := errw.String()
	if !strings.Contains(got, "★★★★★") || !strings.Contains(got, "Ready to release") {
		t.Errorf("a clean repository should be five stars:\n%s", got)
	}
}

func TestStopwatchRecordsPhases(t *testing.T) {
	watch := newStopwatch()
	watch.lap("Validation")
	watch.lap("Version calculation")

	if len(watch.phases) != 2 {
		t.Fatalf("phases = %+v", watch.phases)
	}
	if watch.phases[0].name != "Validation" || watch.phases[1].name != "Version calculation" {
		t.Errorf("phases recorded out of order: %+v", watch.phases)
	}
	if watch.total() <= 0 {
		t.Error("total() should be positive")
	}
}

func TestPrintTiming(t *testing.T) {
	watch := newStopwatch()
	watch.lap("Validation")

	p, _, errw := newTestPrinter()
	printTiming(p, watch)

	got := errw.String()
	if !strings.Contains(got, "Timing") || !strings.Contains(got, "Validation") || !strings.Contains(got, "Total") {
		t.Errorf("timing missing rows:\n%s", got)
	}
	// The report measures; it never estimates.
	if strings.Contains(strings.ToLower(got), "estimat") {
		t.Errorf("timing should report measurements, not estimates:\n%s", got)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		100 * time.Microsecond:  "<1ms",
		137 * time.Millisecond:  "137ms",
		999 * time.Millisecond:  "999ms",
		3670 * time.Millisecond: "3.67s",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestCapitalise(t *testing.T) {
	for in, want := range map[string]string{
		"create Git tag v1.0.0": "Create Git tag v1.0.0",
		"minor":                 "Minor",
		"Already":               "Already",
		"":                      "",
	} {
		if got := capitalise(in); got != want {
			t.Errorf("capitalise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("486bcb2e40fe1b3596d5733bd5ac6da6941b141a"); got != "486bcb2" {
		t.Errorf("shortSHA() = %q", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("a short hash should be left alone, got %q", got)
	}
}

// testFlags mirrors what the flag package produces for a bare invocation, so
// tests never assert against defaults that no real run would have.
func testFlags() *releaseFlags {
	opts := &releaseFlags{}
	opts.remote = "origin"
	opts.tagPrefix = "v"
	return opts
}

func TestCommandLineDefaults(t *testing.T) {
	if got := testFlags().commandLine(semver.BumpMinor); got != "release minor" {
		t.Errorf("commandLine() = %q, want a bare command", got)
	}
}

// Only the flags that change the outcome are repeated; --dry-run never is.
func TestCommandLineRepeatsMeaningfulFlags(t *testing.T) {
	opts := testFlags()
	opts.dryRun = true
	opts.noColor = true
	opts.verbose = true
	opts.prerelease = "rc"
	opts.sign = true
	opts.allowDirty = true
	opts.branches = stringSlice{"main", "release/*"}

	got := opts.commandLine(semver.BumpMajor)
	for _, want := range []string{"release major", "--pre rc", "--sign", "--allow-dirty", "--branch main", "--branch release/*"} {
		if !strings.Contains(got, want) {
			t.Errorf("commandLine() = %q, should contain %q", got, want)
		}
	}
	for _, unwanted := range []string{"--dry-run", "--no-color", "--verbose"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("commandLine() = %q, should not repeat %q", got, unwanted)
		}
	}
}

// The command is meant to be pasted into a shell, so it must be stable and
// syntactically valid however odd the values are.
func TestCommandLineIsStableAndQuoted(t *testing.T) {
	opts := testFlags()
	opts.sign, opts.noFetch, opts.allowDirty, opts.allowEmpty, opts.noPush = true, true, true, true, true

	first := opts.commandLine(semver.BumpPatch)
	for range 20 {
		if got := opts.commandLine(semver.BumpPatch); got != first {
			t.Fatalf("commandLine() is not deterministic:\n%q\n%q", first, got)
		}
	}
}

func TestQuoteArg(t *testing.T) {
	for in, want := range map[string]string{
		"origin":           "origin",
		"release/*":        "release/*",
		"":                 `""`,
		"/path with space": `"/path with space"`,
		`say "hi"`:         `"say \"hi\""`,
	} {
		if got := quoteArg(in); got != want {
			t.Errorf("quoteArg(%q) = %q, want %q", in, got, want)
		}
	}
}
