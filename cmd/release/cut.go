package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// bumpCommand implements `release major|minor|patch`: validate, calculate,
// confirm, tag, push.
func bumpCommand(ctx context.Context, bump semver.Bump, args []string) error {
	fs := flag.NewFlagSet(bump.String(), flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: go run ./cmd/release %s [flags]\n\nTag the next %s release.\n\nFlags:\n", bump, bump)
		fs.PrintDefaults()
	}

	var opts releaseFlags
	opts.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}

	p := newPrinter(os.Stdout, os.Stderr, useColor(opts.noColor, os.Stderr))
	svc := release.New(opts.config())

	if opts.dryRun {
		p.dryRunBanner()
		p.blank()
	}

	p.info("Validating the repository")
	plan, err := svc.Plan(ctx, bump, opts.prerelease)
	if err != nil {
		return err
	}
	p.success("Preflight checks passed")
	p.blank()

	stats := changelog.NewData(plan.Release(), opts.categories()).Stats

	printPlan(p, plan)
	p.blank()
	printActions(p, plannedActions(plan, &opts), opts.dryRun)
	p.blank()
	printStatistics(p, stats)

	if opts.dryRun {
		p.blank()
		p.heading("Release notes preview")

		notes, err := opts.renderNotes(plan.Release())
		if err != nil {
			return err
		}
		fmt.Fprint(p.out, notes)

		p.blank()
		p.warn("Dry run: nothing was created, pushed, or published")
		return nil
	}

	// Prompting only makes sense when a human is watching. In CI, or when the
	// output is piped, --yes is implied.
	if !opts.yes && isTerminal(os.Stdin) && isTerminal(os.Stderr) {
		p.blank()
		question := fmt.Sprintf("Create and push %s?", plan.Tag)
		if opts.noPush {
			question = fmt.Sprintf("Create %s locally?", plan.Tag)
		}
		agreed, err := confirm(os.Stdin, os.Stderr, question)
		if err != nil {
			return err
		}
		if !agreed {
			return errAborted
		}
	}
	p.blank()

	if err := svc.Apply(ctx, plan, !opts.noPush); err != nil {
		return err
	}
	p.success("Created annotated tag %s", plan.Tag)

	if opts.noPush {
		p.warn("The tag was not pushed")
		p.note("Push it with: git push %s %s", opts.remote, plan.Tag)
		return nil
	}

	p.success("Pushed %s to %s", plan.Tag, opts.remote)
	p.blank()
	p.info("GitHub Actions will now generate the changelog and publish the release")
	if plan.Repo.Known() {
		p.note("Watch it at https://%s/%s/%s/actions", plan.Repo.Host, plan.Repo.Owner, plan.Repo.Name)
	}
	return nil
}

// printPlan renders the summary block a user reads before confirming.
func printPlan(p *printer, plan *release.Plan) {
	p.heading("Release plan")

	rows := make([]row, 0, 6)
	if plan.Repo.Known() {
		rows = append(rows, row{label: "Repository", value: plan.Repo.Owner + "/" + plan.Repo.Name})
	}
	rows = append(rows, row{label: "Branch", value: plan.Branch})

	current := plan.PreviousTag
	if plan.IsFirstRelease() {
		current = "none, this is the first release"
	}
	rows = append(rows,
		row{label: "Current", value: current},
		row{label: "Next", value: fmt.Sprintf("%s (%s)", plan.Tag, plan.Bump), bold: true},
		row{label: "Commits", value: strconv.Itoa(len(plan.Commits))},
		row{label: "Release Date", value: plan.Date.Format(time.DateOnly)},
	)
	p.table(rows)
}

// plannedActions lists what the release will do, in order, phrased as lower-case
// infinitives so that both the confirmed and the "would" forms read naturally.
//
// The list reflects the flags: nothing is promised that will not happen.
func plannedActions(plan *release.Plan, opts *releaseFlags) []string {
	actions := []string{fmt.Sprintf("create Git tag %s", plan.Tag)}

	// Without a pushed tag nothing downstream is triggered, so the list stops
	// here rather than promising a release that will never appear.
	if opts.noPush {
		return actions
	}
	return append(actions,
		fmt.Sprintf("push tag to %s", opts.remote),
		"generate release notes",
		"create GitHub Release",
	)
}

func printActions(p *printer, actions []string, dryRun bool) {
	p.heading("Planned Actions")
	for _, action := range actions {
		if dryRun {
			p.bullet("Would %s", action)
		} else {
			p.success("%s", capitalise(action))
		}
	}
}

// printStatistics summarises the release by category. Categories with no commits
// are omitted rather than shown as zero.
func printStatistics(p *printer, stats changelog.Stats) {
	p.heading("Release Statistics")

	rows := make([]row, 0, len(stats.Counts)+3)
	if stats.Bump != "" {
		rows = append(rows, row{label: "Version Bump", value: capitalise(stats.Bump)})
	}
	rows = append(rows, row{label: "Commits", value: strconv.Itoa(stats.Commits)})
	if stats.Breaking > 0 {
		rows = append(rows, row{label: "Breaking", value: strconv.Itoa(stats.Breaking), bold: true})
	}
	for _, c := range stats.Counts {
		rows = append(rows, row{label: c.Label, value: strconv.Itoa(c.N)})
	}
	p.table(rows)
}

// capitalise upper-cases the first letter of an ASCII phrase.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// checkCommand implements `release check`: the preflight validations on their
// own, for use as a pre-release sanity check or a CI guard.
func checkCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: go run ./cmd/release check [flags]\n\nRun the release preflight validations without tagging anything.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	var opts releaseFlags
	opts.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	p := newPrinter(os.Stdout, os.Stderr, useColor(opts.noColor, os.Stderr))
	svc := release.New(opts.config())

	branch, err := svc.Check(ctx)
	if err != nil {
		return err
	}
	p.success("Inside a git repository")
	p.success("On a release branch: %s", branch)
	if opts.allowDirty {
		p.warn("Working tree check skipped by --allow-dirty")
	} else {
		p.success("Working tree is clean")
	}

	latest, err := svc.LatestTag(ctx)
	if err != nil {
		return err
	}
	if latest == "" {
		p.success("No release tags yet; the next release will be the first")
	} else {
		p.success("Latest release tag: %s", latest)
	}
	return nil
}
