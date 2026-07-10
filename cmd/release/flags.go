package main

import (
	"flag"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
)

// stringSlice collects a flag that may be repeated.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// repoFlags are the flags shared by every command: they describe where the
// repository is and how its tags are named.
type repoFlags struct {
	dir       string
	remote    string
	tagPrefix string
	noColor   bool
}

func (f *repoFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.dir, "dir", "", "repository directory (default: the working directory)")
	fs.StringVar(&f.remote, "remote", "origin", "git remote to read tags from and push to")
	fs.StringVar(&f.tagPrefix, "tag-prefix", "v", "prefix prepended to the version to form the tag name")
	fs.BoolVar(&f.noColor, "no-color", false, "disable coloured output")
}

func (f *repoFlags) config() release.Config {
	cfg := release.DefaultConfig()
	cfg.Dir = f.dir
	cfg.Remote = f.remote
	cfg.TagPrefix = f.tagPrefix
	return cfg
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
