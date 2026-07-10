package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
)

func TestResolveRepository(t *testing.T) {
	remote := changelog.Repository{Host: "github.com", Owner: "teddynted", Name: "repo"}

	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("GITHUB_REPOSITORY", "env/owner")
		owner, name, err := resolveRepository("flag/name", remote)
		if err != nil || owner != "flag" || name != "name" {
			t.Errorf("resolveRepository = %q, %q, %v", owner, name, err)
		}
	})

	t.Run("environment beats the remote", func(t *testing.T) {
		t.Setenv("GITHUB_REPOSITORY", "envowner/envrepo")
		owner, name, err := resolveRepository("", remote)
		if err != nil || owner != "envowner" || name != "envrepo" {
			t.Errorf("resolveRepository = %q, %q, %v", owner, name, err)
		}
	})

	t.Run("falls back to the remote", func(t *testing.T) {
		t.Setenv("GITHUB_REPOSITORY", "")
		owner, name, err := resolveRepository("", remote)
		if err != nil || owner != "teddynted" || name != "repo" {
			t.Errorf("resolveRepository = %q, %q, %v", owner, name, err)
		}
	})

	t.Run("rejects a malformed value", func(t *testing.T) {
		t.Setenv("GITHUB_REPOSITORY", "")
		if _, _, err := resolveRepository("noslash", remote); err == nil {
			t.Error("resolveRepository should reject a value that is not owner/name")
		}
	})

	t.Run("errors when nothing is known", func(t *testing.T) {
		t.Setenv("GITHUB_REPOSITORY", "")
		if _, _, err := resolveRepository("", changelog.Repository{}); err == nil {
			t.Error("resolveRepository should fail when the repository cannot be determined")
		}
	})
}

func TestExpandAssets(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"release_linux_amd64", "release_darwin_arm64", "checksums.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The same file matched by two patterns must be uploaded once, and the
	// directory must not be treated as an asset.
	paths, err := expandAssets([]string{filepath.Join(dir, "*"), filepath.Join(dir, "checksums.txt")})
	if err != nil {
		t.Fatalf("expandAssets: %v", err)
	}

	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	slices.Sort(names)

	want := []string{"checksums.txt", "release_darwin_arm64", "release_linux_amd64"}
	if !slices.Equal(names, want) {
		t.Errorf("expandAssets = %v, want %v", names, want)
	}
}

func TestExpandAssetsNoMatch(t *testing.T) {
	paths, err := expandAssets([]string{filepath.Join(t.TempDir(), "nothing-here-*")})
	if err != nil {
		t.Fatalf("a pattern that matches nothing is not an error: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("expandAssets = %v, want none", paths)
	}
}

func TestExpandAssetsInvalidPattern(t *testing.T) {
	if _, err := expandAssets([]string{"["}); err == nil {
		t.Error("a malformed glob should be reported")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("firstNonEmpty = %q, want third", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty = %q, want empty", got)
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
