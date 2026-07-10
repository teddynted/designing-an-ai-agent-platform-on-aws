package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// stringSlice collects a flag that may be repeated.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// repoFlags are the flags shared by every command: they describe where the
// repository is, how its tags are named, how its notes are rendered, and how
// much the tool says while it works.
type repoFlags struct {
	dir       string
	remote    string
	tagPrefix string
	template  string

	noColor bool
	ascii   bool
	verbose bool
	debug   bool

	verifyAuth bool
}

func (f *repoFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.dir, "dir", "", "repository directory (default: the working directory)")
	fs.StringVar(&f.remote, "remote", "origin", "git remote to read tags from and push to")
	fs.StringVar(&f.tagPrefix, "tag-prefix", "v", "prefix prepended to the version to form the tag name")
	fs.StringVar(&f.template, "template", "", "render notes with this text/template file instead of the built-in layout")

	fs.BoolVar(&f.noColor, "no-color", false, "disable coloured output")
	fs.BoolVar(&f.ascii, "ascii", false, "use ASCII markers instead of Unicode icons")
	fs.BoolVar(&f.verbose, "verbose", false, "narrate each phase as it runs")
	fs.BoolVar(&f.debug, "debug", false, "print internal diagnostic detail")

	fs.BoolVar(&f.verifyAuth, "verify-auth", false, "check GITHUB_TOKEN against the GitHub API (makes a network call)")
}

func (f *repoFlags) config() release.Config {
	cfg := release.DefaultConfig()
	cfg.Dir = f.dir
	cfg.Remote = f.remote
	cfg.TagPrefix = f.tagPrefix
	return cfg
}

// level maps the flags onto a verbosity. --debug implies --verbose.
func (f *repoFlags) level() verbosity {
	switch {
	case f.debug:
		return levelDebug
	case f.verbose:
		return levelVerbose
	default:
		return levelNormal
	}
}

// printerOptions builds the printer configuration for a stream, resolving the
// terminal width and whether colour and Unicode are appropriate.
func (f *repoFlags) printerOptions(stream *os.File) options {
	return options{
		color: useColor(f.noColor, stream),
		ascii: f.ascii,
		width: detectWidth(stream),
		level: f.level(),
	}
}

// categories returns the changelog categories in force. It is the single place
// a future --categories flag would hook into.
func (f *repoFlags) categories() []changelog.Category { return changelog.DefaultCategories() }

// renderOptions builds the changelog rendering options, loading a custom
// template when one was named.
func (f *repoFlags) renderOptions() (changelog.Options, error) {
	opts := changelog.Options{Categories: f.categories()}
	if f.template == "" {
		return opts, nil
	}

	tmpl, err := changelog.ParseTemplate(f.template)
	if err != nil {
		return changelog.Options{}, err
	}
	opts.Template = tmpl
	return opts, nil
}

// renderNotes renders a release's notes. Every command that prints notes goes
// through here, so --template behaves identically everywhere.
func (f *repoFlags) renderNotes(rel changelog.Release) (string, error) {
	opts, err := f.renderOptions()
	if err != nil {
		return "", err
	}
	return changelog.RenderNotes(rel, opts)
}

// renderEntry renders a release's CHANGELOG.md entry.
func (f *repoFlags) renderEntry(rel changelog.Release) (string, error) {
	opts, err := f.renderOptions()
	if err != nil {
		return "", err
	}
	return changelog.RenderEntry(rel, opts)
}

// releaseFlags adds the flags that govern cutting a release.
type releaseFlags struct {
	repoFlags

	branches   stringSlice
	anyBranch  bool
	prerelease string

	sign       bool
	noFetch    bool
	allowDirty bool
	allowEmpty bool

	dryRun bool
	noPush bool
	yes    bool
}

func (f *releaseFlags) register(fs *flag.FlagSet) {
	f.repoFlags.register(fs)

	fs.Var(&f.branches, "branch", "branch permitted to publish releases; repeatable, supports globs (default: main, master)")
	fs.BoolVar(&f.anyBranch, "any-branch", false, "allow releasing from any branch")
	fs.StringVar(&f.prerelease, "pre", "", "cut a pre-release in this series, e.g. rc or beta")

	fs.BoolVar(&f.sign, "sign", false, "create a GPG-signed tag instead of an annotated one")
	fs.BoolVar(&f.noFetch, "no-fetch", false, "skip fetching tags from the remote before calculating the version")
	fs.BoolVar(&f.allowDirty, "allow-dirty", false, "release even though the working tree has uncommitted changes")
	fs.BoolVar(&f.allowEmpty, "allow-empty", false, "release even though there are no new commits")

	fs.BoolVar(&f.dryRun, "dry-run", false, "print what would happen without creating a tag")
	fs.BoolVar(&f.noPush, "no-push", false, "create the tag locally but do not push it")
	fs.BoolVar(&f.yes, "yes", false, "do not ask for confirmation")
}

func (f *releaseFlags) config() release.Config {
	cfg := f.repoFlags.config()
	cfg.Sign = f.sign
	cfg.FetchTags = !f.noFetch
	cfg.AllowDirty = f.allowDirty
	cfg.AllowEmpty = f.allowEmpty

	switch {
	case f.anyBranch:
		cfg.Branches = nil
	case len(f.branches) > 0:
		cfg.Branches = f.branches
	}
	return cfg
}

// commandLine reconstructs the command that would perform this release for
// real, so a dry run can tell the user exactly what to type next.
//
// Only the flags that change the outcome are included. Presentation flags such
// as --no-color, and --dry-run itself, are left out: repeating them would be
// noise, and repeating --dry-run would be wrong.
func (f *releaseFlags) commandLine(bump semver.Bump) string {
	parts := []string{"release", bump.String()}

	if f.prerelease != "" {
		parts = append(parts, "--pre", quoteArg(f.prerelease))
	}
	if f.tagPrefix != "v" {
		parts = append(parts, "--tag-prefix", quoteArg(f.tagPrefix))
	}
	if f.remote != "origin" {
		parts = append(parts, "--remote", quoteArg(f.remote))
	}
	if f.dir != "" {
		parts = append(parts, "--dir", quoteArg(f.dir))
	}
	if f.template != "" {
		parts = append(parts, "--template", quoteArg(f.template))
	}
	if f.anyBranch {
		parts = append(parts, "--any-branch")
	}
	for _, branch := range f.branches {
		parts = append(parts, "--branch", quoteArg(branch))
	}

	// A map would order these differently on every run, and the command is
	// meant to be copied.
	for _, boolFlag := range []struct {
		name    string
		enabled bool
	}{
		{"--sign", f.sign},
		{"--no-fetch", f.noFetch},
		{"--allow-dirty", f.allowDirty},
		{"--allow-empty", f.allowEmpty},
		{"--no-push", f.noPush},
	} {
		if boolFlag.enabled {
			parts = append(parts, boolFlag.name)
		}
	}
	return strings.Join(parts, " ")
}

// quoteArg makes a value safe to paste into a shell. An empty value especially:
// `--tag-prefix` followed by nothing would silently swallow the next argument.
func quoteArg(value string) string {
	if value == "" || strings.ContainsAny(value, " \t\"'$`\\") {
		return strconv.Quote(value)
	}
	return value
}

// verifyAuthentication asks GitHub who the token belongs to, and returns the
// result as a health check so it can join the others in the report.
//
// It is only called when --verify-auth is passed, because cutting a tag needs
// no GitHub API call and should keep working offline.
func verifyAuthentication(ctx context.Context) release.Check {
	const name = "GitHub authentication"

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return release.Check{
			Name:   name,
			Level:  release.LevelWarn,
			Detail: "GITHUB_TOKEN is not set, so it cannot be verified",
		}
	}

	login, err := newGitHubClient(token, "", "").Viewer(ctx)
	if err != nil {
		return release.Check{
			Name:   name,
			Level:  release.LevelFail,
			Detail: fmt.Sprintf("GITHUB_TOKEN was rejected: %v", err),
		}
	}
	return release.Check{
		Name:   name,
		Level:  release.LevelOK,
		Detail: "authenticated as " + login,
	}
}
