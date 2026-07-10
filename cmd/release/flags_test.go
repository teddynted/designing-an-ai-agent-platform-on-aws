package main

import (
	"flag"
	"io"
	"slices"
	"testing"
)

// parseFlags runs a fresh flag set, as a command would.
func parseFlags(t *testing.T, args []string) *releaseFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	opts := &releaseFlags{}
	opts.register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return opts
}

func TestDefaultConfig(t *testing.T) {
	cfg := parseFlags(t, nil).config()

	if cfg.Remote != "origin" || cfg.TagPrefix != "v" {
		t.Errorf("cfg = %+v", cfg)
	}
	if !slices.Equal(cfg.Branches, []string{"main", "master"}) {
		t.Errorf("Branches = %v, want [main master]", cfg.Branches)
	}
	// Fetching tags is on by default, so a stale clone cannot reuse a version.
	if !cfg.FetchTags {
		t.Error("FetchTags should default to true")
	}
	if cfg.Sign || cfg.AllowDirty || cfg.AllowEmpty {
		t.Errorf("the safety checks should be on by default: %+v", cfg)
	}
}

func TestBranchFlagsOverrideDefaults(t *testing.T) {
	cfg := parseFlags(t, []string{"--branch", "main", "--branch", "release/*"}).config()
	if !slices.Equal(cfg.Branches, []string{"main", "release/*"}) {
		t.Errorf("Branches = %v", cfg.Branches)
	}
}

// --any-branch must win over --branch, not merge with it.
func TestAnyBranchClearsTheBranchList(t *testing.T) {
	cfg := parseFlags(t, []string{"--branch", "main", "--any-branch"}).config()
	if len(cfg.Branches) != 0 {
		t.Errorf("Branches = %v, want none", cfg.Branches)
	}
}

func TestNoFetchDisablesFetching(t *testing.T) {
	if cfg := parseFlags(t, []string{"--no-fetch"}).config(); cfg.FetchTags {
		t.Error("--no-fetch should disable FetchTags")
	}
}

func TestEscapeHatchFlags(t *testing.T) {
	cfg := parseFlags(t, []string{"--allow-dirty", "--allow-empty", "--sign", "--tag-prefix", "release-"}).config()
	if !cfg.AllowDirty || !cfg.AllowEmpty || !cfg.Sign {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.TagPrefix != "release-" {
		t.Errorf("TagPrefix = %q", cfg.TagPrefix)
	}
}

func TestStringSlice(t *testing.T) {
	var s stringSlice
	s.Set("a")
	s.Set("b")
	if got := s.String(); got != "a,b" {
		t.Errorf("String() = %q, want a,b", got)
	}
}
