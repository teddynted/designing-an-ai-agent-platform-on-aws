package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
)

// This file renders the release report. It knows how things look; it decides
// nothing about what a release is. Every function takes data already computed
// by internal/release or internal/changelog and turns it into lines.

// levelStatus maps a health level onto the glyph the printer draws.
func levelStatus(l release.Level) status {
	switch l {
	case release.LevelOK:
		return statusOK
	case release.LevelWarn:
		return statusWarn
	default:
		return statusFail
	}
}

// printHealth renders the Validation section: one line per check, each carrying
// its own verdict, so a reader sees the whole picture rather than the first
// problem.
func printHealth(p *printer, health release.Health) {
	p.section("Validation")
	for _, c := range health.Checks {
		p.status(levelStatus(c.Level), "%s %s %s", c.Name, p.theme.dash, c.Detail)
	}
}

// replaceCheck substitutes a check of the same name, keeping its position in
// the report. Matching on the name rather than the position means the order of
// the checks can change without breaking this.
func replaceCheck(health release.Health, check release.Check) release.Health {
	for i, existing := range health.Checks {
		if existing.Name == check.Name {
			health.Checks[i] = check
			return health
		}
	}
	health.Checks = append(health.Checks, check)
	return health
}

// printPlan renders the Release Plan: where the release is being cut from.
func printPlan(p *printer, plan *release.Plan, remote string) {
	p.section("Release Plan")

	rows := make([]row, 0, 4)
	if plan.Repo.Known() {
		rows = append(rows, row{label: "Repository", value: plan.Repo.Owner + "/" + plan.Repo.Name})
	}
	rows = append(rows,
		row{label: "Branch", value: plan.Branch},
		row{label: "Remote", value: remote},
		row{label: "Commit", value: shortSHA(plan.HeadSHA)},
	)
	p.table(rows)
}

// printVersion renders the Version Information section: what the version is
// moving from, to, and how long it has been.
func printVersion(p *printer, plan *release.Plan) {
	p.section("Version Information")

	current := plan.PreviousTag
	if plan.IsFirstRelease() {
		current = "none, this is the first release"
	}

	rows := []row{
		{label: "Current Version", value: current},
		{label: "Next Version", value: plan.Tag, bold: true},
		{label: "Increment Type", value: capitalise(plan.Bump.String())},
	}
	if plan.Next.IsPrerelease() {
		rows = append(rows, row{label: "Pre-release", value: plan.Next.Prerelease})
	}
	if !plan.PreviousDate.IsZero() {
		rows = append(rows, row{label: "Previous Release", value: plan.PreviousDate.Format(time.DateOnly)})
	}
	if days, ok := plan.DaysSincePrevious(); ok {
		rows = append(rows, row{label: "Days Since", value: elapsedDays(days)})
	}
	rows = append(rows, row{label: "Release Date", value: plan.Date.Format(time.DateOnly)})
	p.table(rows)
}

// printActions renders the Planned Actions section. During a dry run the verbs
// are conditional, so the two modes can never be confused at a glance.
func printActions(p *printer, actions []string, dryRun bool) {
	p.section("Planned Actions")
	for _, action := range actions {
		if dryRun {
			p.bullet("Would %s", action)
		} else {
			p.success("%s", capitalise(action))
		}
	}
}

// printStatistics renders the Release Statistics section. Categories with no
// commits are omitted rather than shown as zero, and the diff is omitted when
// git could not measure it.
func printStatistics(p *printer, stats changelog.Stats, diff git.DiffStat) {
	p.section("Release Statistics")

	rows := make([]row, 0, len(stats.Counts)+6)
	rows = append(rows, row{label: "Total Commits", value: strconv.Itoa(stats.Commits)})
	for _, c := range stats.Counts {
		rows = append(rows, row{label: c.Label, value: strconv.Itoa(c.N)})
	}
	if stats.Breaking > 0 {
		rows = append(rows, row{label: "Breaking Changes", value: strconv.Itoa(stats.Breaking), bold: true})
	}
	if diff != (git.DiffStat{}) {
		rows = append(rows,
			row{label: "Files Changed", value: humanInt(diff.Files)},
			row{label: "Lines Added", value: signed("+", diff.Insertions)},
			row{label: "Lines Removed", value: signed("-", diff.Deletions)},
		)
	}
	p.table(rows)
}

// signed prefixes a line count with its sign, except zero, which has none:
// "-0" reads as a mistake.
func signed(sign string, n int) string {
	if n == 0 {
		return "0"
	}
	return sign + humanInt(n)
}

// printContributors renders the Contributors section, or nothing at all when no
// commit carries author information.
func printContributors(p *printer, contributors []changelog.Contributor) {
	if len(contributors) == 0 {
		return
	}
	p.section("Contributors")
	for _, c := range contributors {
		p.bullet("%s (%s)", c.Name, pluralise(c.Commits, "commit", "commits"))
	}
}

// printNotes renders the Release Notes Preview. The notes themselves go to
// stdout, so they can be redirected while the report stays on stderr.
func printNotes(p *printer, notes string) {
	p.section("Release Notes Preview")
	fmt.Fprint(p.out, notes)
}

// confidence turns the health report into a rating.
//
// The stars are derived, never asserted: five means every check passed, and
// each warning costs one, down to a floor of two — because a release that can
// proceed at all is never worthless. A failing check is not a low rating, it is
// a blocked release, and the caller will have errored before reaching here.
func confidence(health release.Health) (stars int, label string) {
	switch {
	case health.Failures() > 0:
		return 1, "Not ready to release"
	case health.Warnings() == 0:
		return 5, "Ready to release"
	default:
		return max(2, 5-health.Warnings()), "Ready with warnings"
	}
}

// printConfidence renders the Release Confidence section, and explains every
// warning behind the rating. A rating without its reasons is decoration.
func printConfidence(p *printer, health release.Health) {
	stars, label := confidence(health)
	p.section("Release Confidence")

	marker := statusOK
	switch {
	case health.Failures() > 0:
		marker = statusFail
	case health.Warnings() > 0:
		marker = statusWarn
	}
	p.status(marker, "%s  %s", p.stars(stars), label)

	if concerns := health.Concerns(); len(concerns) > 0 {
		p.blank()
		for _, concern := range concerns {
			p.note("%s %s", p.theme.bullet, concern)
		}
	}
}

// phase is one timed step of the release.
type phase struct {
	name     string
	duration time.Duration
}

// stopwatch records how long each phase actually took.
//
// It reports measurements, never estimates. Predicting how long a push to a
// remote will take is guesswork, and a confidently wrong number is worse than
// no number at all.
type stopwatch struct {
	start  time.Time
	last   time.Time
	phases []phase
}

func newStopwatch() *stopwatch {
	now := time.Now()
	return &stopwatch{start: now, last: now}
}

// lap closes the current phase and opens the next.
func (s *stopwatch) lap(name string) {
	now := time.Now()
	s.phases = append(s.phases, phase{name: name, duration: now.Sub(s.last)})
	s.last = now
}

// total is the elapsed time since the stopwatch started.
func (s *stopwatch) total() time.Duration { return time.Since(s.start) }

// printTiming renders the Timing section with what was actually measured.
func printTiming(p *printer, watch *stopwatch) {
	p.section("Timing")

	rows := make([]row, 0, len(watch.phases)+1)
	for _, ph := range watch.phases {
		rows = append(rows, row{label: ph.name, value: formatDuration(ph.duration)})
	}
	rows = append(rows, row{label: "Total", value: formatDuration(watch.total()), bold: true})
	p.table(rows)
}

// formatDuration renders a duration at a precision a person can read: whole
// milliseconds below a second, then two significant figures of seconds.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

// humanInt groups thousands, so a large diff can be read at a glance.
func humanInt(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + humanInt(-n)
	}
	if len(s) <= 3 {
		return s
	}

	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// elapsedDays renders the age of the previous release. Zero days is "today",
// because "0 days" reads as a missing value rather than a same-day release.
func elapsedDays(days int) string {
	switch days {
	case 0:
		return "today"
	case 1:
		return "yesterday"
	default:
		return pluralise(days, "day", "days") + " ago"
	}
}

// pluralise renders "1 commit" or "12 commits".
func pluralise(n int, singular, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// capitalise upper-cases the first letter of an ASCII phrase.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// shortSHA abbreviates a commit hash for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
