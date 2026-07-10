// Command release cuts and publishes releases of this repository.
//
//	release patch            bump the patch version, tag, and update the changelog
//	release minor            bump the minor version
//	release major            bump the major version
//	release publish v0.2.0   publish the GitHub release for an existing tag
//	release notes            print the notes for the next release without writing
//	release current          print the current version
//
// The local flow tags and leaves the push to you; the GitHub release is created
// by CI when the tag lands. `--publish` overrides that for a manual release.
//
// Subcommands are dispatched by hand over the standard library's flag package.
// Cobra would add a dependency, a hundred lines of registration, and shell
// completion nobody asked for, to a tool with six verbs.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/github"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/workflow"
)

const usage = `release — cut and publish releases

Usage:
  release <command> [flags]

Commands:
  patch                Bump the patch version (a backwards-compatible fix)
  minor                Bump the minor version (a backwards-compatible feature)
  major                Bump the major version (a breaking change)
  publish <version>    Publish the GitHub release for an existing tag
  notes                Print the notes for the next release; write nothing
  current              Print the current version

Flags:
  -C <dir>             Run as if in this directory (default ".")
  -repository <o/n>    GitHub repository (default: $GITHUB_REPOSITORY)
  -remote <name>       Remote to push to (default "origin")
  -dry-run             Compute everything, write nothing
  -no-push             Commit and tag locally; do not push
  -publish             Publish the GitHub release as part of a bump
  -draft               Publish as a draft
  -no-fallback         Fail rather than fall back to local git when GitHub
                       cannot answer a comparison
  -v                   Verbose progress on stderr

Environment:
  GITHUB_TOKEN         Token with contents: write. Without it, this tool reads
                       from local git only and cannot publish.
  GITHUB_REPOSITORY    owner/name, as set by GitHub Actions.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

// config is the parsed command line.
type config struct {
	root       string
	repository string
	remote     string
	dryRun     bool
	noPush     bool
	publish    bool
	draft      bool
	noFallback bool
	verbose    bool
	args       []string
}

func parse(argv []string) (command string, cfg config, err error) {
	if len(argv) == 0 {
		return "", config{}, fmt.Errorf("no command given\n\n%s", usage)
	}
	command = argv[0]
	if command == "-h" || command == "--help" || command == "help" {
		fmt.Print(usage)
		os.Exit(0)
	}

	flags := flag.NewFlagSet("release "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	flags.StringVar(&cfg.root, "C", ".", "run as if in this directory")
	flags.StringVar(&cfg.repository, "repository", os.Getenv("GITHUB_REPOSITORY"), "GitHub repository, owner/name")
	flags.StringVar(&cfg.remote, "remote", workflow.DefaultRemote, "remote to push to")
	flags.BoolVar(&cfg.dryRun, "dry-run", false, "compute everything, write nothing")
	flags.BoolVar(&cfg.noPush, "no-push", false, "commit and tag locally; do not push")
	flags.BoolVar(&cfg.publish, "publish", false, "publish the GitHub release as part of a bump")
	flags.BoolVar(&cfg.draft, "draft", false, "publish as a draft")
	flags.BoolVar(&cfg.noFallback, "no-fallback", false, "fail rather than fall back to local git")
	flags.BoolVar(&cfg.verbose, "v", false, "verbose progress on stderr")

	if err := flags.Parse(argv[1:]); err != nil {
		return "", config{}, err
	}

	// The flag package stops at the first non-flag argument, so `publish v0.2.0
	// -v` would file -v as an argument. Re-parsing after each positional lets
	// flags and arguments interleave, which is what people type.
	rest := flags.Args()
	for len(rest) > 0 {
		cfg.args = append(cfg.args, rest[0])
		if err := flags.Parse(rest[1:]); err != nil {
			return "", config{}, err
		}
		rest = flags.Args()
	}
	return command, cfg, nil
}

func run(argv []string) error {
	command, cfg, err := parse(argv)
	if err != nil {
		return err
	}

	level := slog.LevelWarn
	if cfg.verbose {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	repo := git.NewRepo(cfg.root)

	// The host is optional. Without a token this tool still reads local git,
	// which is what makes `notes` and `--dry-run` work on a laptop.
	var host release.Host
	var publisher workflow.Publisher
	if token := os.Getenv("GITHUB_TOKEN"); token != "" && cfg.repository != "" {
		client, err := github.NewClient(cfg.repository, token)
		if err != nil {
			return err
		}
		releases := github.NewReleases(client)
		host, publisher = releases, releases
	}

	comparisons := release.NewComparisonService(repo, host, !cfg.noFallback, logger)
	notes := release.NewNotesService(comparisons, release.SystemClock{})
	runner := workflow.NewRunner(repo, comparisons, notes, publisher, release.SystemClock{}, logger)

	options := workflow.Options{
		Root:        cfg.root,
		Repository:  cfg.repository,
		Remote:      cfg.remote,
		DryRun:      cfg.dryRun,
		Draft:       cfg.draft,
		SkipPush:    cfg.noPush,
		SkipPublish: !cfg.publish,
	}

	switch command {
	case "major", "minor", "patch":
		part, err := version.ParsePart(command)
		if err != nil {
			return err
		}
		options.Part = part
		result, err := runner.Run(options)
		if err != nil {
			return err
		}
		workflow.Describe(os.Stdout, result)
		return nil

	case "publish":
		if len(cfg.args) != 1 {
			return fmt.Errorf("publish needs exactly one version, e.g. `release publish v0.2.0`")
		}
		target, err := version.Parse(cfg.args[0])
		if err != nil {
			return err
		}
		result, err := runner.Publish(target, options)
		if err != nil {
			return err
		}
		if result.DryRun {
			fmt.Println(result.Body)
			return nil
		}
		fmt.Printf("Published %s\n", target.Tag())
		return nil

	case "notes":
		return printNotes(cfg, notes)

	case "current":
		current, err := release.NewVersionFile(cfg.root).Read()
		if err != nil {
			return err
		}
		fmt.Println(current)
		return nil

	default:
		return fmt.Errorf("unknown command %q\n\n%s", command, usage)
	}
}

// printNotes renders the notes for the next release without writing anything.
// The level defaults to patch, because notes do not depend on the level except
// through the version they are headed with.
func printNotes(cfg config, notes *release.NotesService) error {
	current, err := release.NewVersionFile(cfg.root).Read()
	if err != nil {
		return err
	}
	next := current.BumpPatch()

	generated, err := notes.Generate(next, "HEAD")
	if err != nil {
		return err
	}
	fmt.Print(strings.TrimRight(changelog.ReleaseBody(generated, cfg.repository), "\n"), "\n")
	return nil
}
