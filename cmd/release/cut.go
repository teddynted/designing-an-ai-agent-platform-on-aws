package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// bumpCommand implements `release major|minor|patch`.
//
// It reads as the report does: validate, plan, describe, confirm, apply. Each
// step appends to the report and the stopwatch; none of them format anything
// themselves.
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

	p := newPrinter(os.Stdout, os.Stderr, opts.printerOptions(os.Stderr))
	svc := release.New(opts.config())
	watch := newStopwatch()

	p.debugf("remote=%s tag-prefix=%q dir=%q dry-run=%v", opts.remote, opts.tagPrefix, opts.dir, opts.dryRun)

	if opts.dryRun {
		p.dryRunBanner()
	}

	// Validation. Health describes every problem at once; Plan then enforces
	// the ones that block a release, and explains how to fix them.
	p.verbosef("inspecting the repository")
	health, err := svc.Health(ctx)
	if err != nil {
		return err
	}
	if opts.verifyAuth {
		p.verbosef("verifying GITHUB_TOKEN against the GitHub API")
		health = replaceCheck(health, verifyAuthentication(ctx))
	}
	printHealth(p, health)
	watch.lap("Validation")

	p.verbosef("calculating the next version")
	plan, err := svc.Plan(ctx, bump, opts.prerelease)
	if err != nil {
		return err
	}
	watch.lap("Version calculation")
	p.debugf("previous=%q next=%s commits=%d", plan.PreviousTag, plan.Tag, len(plan.Commits))

	printPlan(p, plan, opts.remote)
	printVersion(p, plan)
	printActions(p, plannedActions(plan, &opts), opts.dryRun)

	rel := plan.Release()
	printStatistics(p, changelog.NewData(rel, opts.categories()).Stats, plan.Diff)
	printContributors(p, plan.Contributors())

	p.verbosef("rendering the release notes")
	notes, err := opts.renderNotes(rel)
	if err != nil {
		return err
	}
	watch.lap("Release notes")

	// Show the notes whenever a person is watching: before a dry run ends, and
	// before the prompt that publishes them for real.
	interactive := !opts.yes && isTerminal(os.Stdin) && isTerminal(os.Stderr)
	if opts.dryRun || interactive {
		printNotes(p, notes)
	}

	printConfidence(p, health)

	if opts.dryRun {
		printTiming(p, watch)
		printDryRunSummary(p, plan, &opts, bump)
		return nil
	}

	if interactive {
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

	p.section("Releasing")
	p.verbosef("creating the tag and pushing it to %s", opts.remote)
	if err := svc.Apply(ctx, plan, !opts.noPush); err != nil {
		return err
	}
	watch.lap("Git operations")

	p.success("Created annotated tag %s", plan.Tag)
	if !opts.noPush {
		p.success("Pushed %s to %s", plan.Tag, opts.remote)
	}

	printTiming(p, watch)
	printReleaseSummary(p, plan, &opts)
	return nil
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

// printDryRunSummary closes a dry run by saying plainly that nothing happened,
// and giving the exact command that would make it happen.
func printDryRunSummary(p *printer, plan *release.Plan, opts *releaseFlags, bump semver.Bump) {
	p.section("Summary")
	p.success("Dry run completed successfully")
	p.blank()
	p.note("No tag was created. Nothing was pushed. Nothing was published.")
	p.blank()
	p.plain("  Run:")
	p.blank()
	p.plain("      %s", p.paint(ansiBold, opts.commandLine(bump)))
	p.blank()
	p.plain("  to publish %s.", plan.Tag)
}

// printReleaseSummary closes a real release with what happened and what happens
// next.
func printReleaseSummary(p *printer, plan *release.Plan, opts *releaseFlags) {
	p.section("Summary")

	if opts.noPush {
		p.warn("%s exists locally but was not pushed", plan.Tag)
		p.blank()
		p.note("Push it with: git push %s %s", opts.remote, plan.Tag)
		return
	}

	p.success("Released %s", plan.Tag)
	p.blank()
	p.note("GitHub Actions will now generate the changelog and publish the release.")
	if plan.Repo.Known() {
		p.note("Watch it at https://%s/%s/%s/actions", plan.Repo.Host, plan.Repo.Owner, plan.Repo.Name)
	}
}

// checkCommand implements `release check`: the health report on its own, for use
// as a pre-release sanity check or a CI guard.
func checkCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: go run ./cmd/release check [flags]\n\nReport on the repository's readiness to publish a release.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	var opts releaseFlags
	opts.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	p := newPrinter(os.Stdout, os.Stderr, opts.printerOptions(os.Stderr))
	svc := release.New(opts.config())

	health, err := svc.Health(ctx)
	if err != nil {
		return err
	}
	if opts.verifyAuth {
		health = replaceCheck(health, verifyAuthentication(ctx))
	}
	printHealth(p, health)

	latest, err := svc.LatestTag(ctx)
	if err != nil {
		return err
	}

	p.section("Version Information")
	if latest == "" {
		p.table([]row{{label: "Current Version", value: "none, the next release will be the first"}})
	} else {
		p.table([]row{{label: "Current Version", value: latest}})
	}

	printConfidence(p, health)

	// `check` reports; it does not decide. A failing check still exits non-zero
	// so that CI can gate on it.
	if health.Failures() > 0 {
		return fmt.Errorf("%d of %d checks failed", health.Failures(), len(health.Checks))
	}
	return nil
}
