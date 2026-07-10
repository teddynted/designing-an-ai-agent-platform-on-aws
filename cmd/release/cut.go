package main

import (
	"context"
	"flag"
	"fmt"
	"os"
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

	p.step("Validating the repository")
	plan, err := svc.Plan(ctx, bump, opts.prerelease)
	if err != nil {
		return err
	}
	p.ok("Preflight checks passed")
	p.blank()

	printPlan(p, plan)
	p.blank()

	if opts.dryRun {
		p.warn("Dry run: no tag was created")
		p.blank()
		p.heading("Release notes preview")
		fmt.Fprintln(p.out, changelog.RenderNotes(plan.Release(time.Now().UTC()), changelog.DefaultSections()))
		return nil
	}

	// Prompting only makes sense when a human is watching. In CI, or when the
	// output is piped, --yes is implied.
	if !opts.yes && isTerminal(os.Stdin) && isTerminal(os.Stderr) {
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
		p.blank()
	}

	if err := svc.Apply(ctx, plan, !opts.noPush); err != nil {
		return err
	}
	p.ok("Created annotated tag %s", plan.Tag)

	if opts.noPush {
		p.warn("The tag was not pushed")
		p.info("Push it with: git push %s %s", opts.remote, plan.Tag)
		return nil
	}

	p.ok("Pushed %s to %s", plan.Tag, opts.remote)
	p.blank()
	p.info("GitHub Actions will now generate the changelog and publish the release.")
	if plan.Repo.Owner != "" {
		p.info("Watch it at https://%s/%s/%s/actions", plan.Repo.Host, plan.Repo.Owner, plan.Repo.Name)
	}
	return nil
}

// printPlan renders the summary block a user reads before confirming.
func printPlan(p *printer, plan *release.Plan) {
	p.heading("Release plan")

	if plan.Repo.Owner != "" {
		p.field("Repository", "%s/%s", plan.Repo.Owner, plan.Repo.Name)
	}
	p.field("Branch", "%s", plan.Branch)

	if plan.IsFirstRelease() {
		p.field("Current", "none, this is the first release")
	} else {
		p.field("Current", "%s", plan.PreviousTag)
	}

	next := fmt.Sprintf("%s  (%s)", plan.Tag, plan.Bump)
	if plan.Next.IsPrerelease() {
		next += "  pre-release"
	}
	p.field("Next", "%s", p.paint(ansiBold, next))

	p.field("Commits", "%s", pluralise(len(plan.Commits), "commit", "commits"))
}

func pluralise(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
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
	p.ok("Inside a git repository")
	p.ok("On a release branch: %s", branch)
	if opts.allowDirty {
		p.warn("Working tree check skipped by --allow-dirty")
	} else {
		p.ok("Working tree is clean")
	}

	latest, err := svc.LatestTag(ctx)
	if err != nil {
		return err
	}
	if latest == "" {
		p.ok("No release tags yet; the next release will be the first")
	} else {
		p.ok("Latest release tag: %s", latest)
	}
	return nil
}
