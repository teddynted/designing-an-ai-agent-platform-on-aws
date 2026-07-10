package release

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

func TestErrorFormatsWhatWhyAndSolutions(t *testing.T) {
	err := &Error{
		Cause:     ErrTagExists,
		What:      `Tag "v0.1.0" already exists.`,
		Why:       "A patch bump from 0.0.9 lands on a version that is already tagged.",
		Solutions: []string{"choose another version", "delete the existing tag"},
	}

	got := err.Error()
	want := `Tag "v0.1.0" already exists.

A patch bump from 0.0.9 lands on a version that is already tagged.

Possible solutions:

• choose another version
• delete the existing tag`

	if got != want {
		t.Errorf("Error() =\n%q\nwant\n%q", got, want)
	}
}

func TestErrorWithoutWhyOrSolutions(t *testing.T) {
	err := &Error{What: "Something went wrong."}
	if got := err.Error(); got != "Something went wrong." {
		t.Errorf("Error() = %q", got)
	}
}

// The rich message must not cost callers the ability to classify a failure.
func TestErrorUnwrapsToSentinel(t *testing.T) {
	err := error(&Error{Cause: ErrDirtyWorkTree, What: "dirty"})
	if !errors.Is(err, ErrDirtyWorkTree) {
		t.Error("Error should unwrap to its sentinel")
	}
	if errors.Is(err, ErrTagExists) {
		t.Error("Error should not match an unrelated sentinel")
	}
}

func TestIndent(t *testing.T) {
	if got, want := indent([]string{"a", "b"}), "  a\n  b"; got != want {
		t.Errorf("indent() = %q, want %q", got, want)
	}
}

// Each failure names the offending thing and offers a way out. These are the
// messages a user sees when a release goes wrong, so they are worth pinning.
func TestPlanErrorsExplainThemselves(t *testing.T) {
	ctx := context.Background()

	t.Run("dirty work tree", func(t *testing.T) {
		fake := newFake()
		fake.status = []string{" M go.mod", "?? scratch.txt"}

		_, err := NewWithGit(testConfig(), fake).Plan(ctx, semver.BumpPatch, "")
		assertExplains(t, err, ErrDirtyWorkTree,
			[]string{"go.mod", "scratch.txt"},
			[]string{"git stash", "--allow-dirty"})
	})

	t.Run("disallowed branch", func(t *testing.T) {
		fake := newFake()
		fake.branch = "feature/x"

		_, err := NewWithGit(testConfig(), fake).Plan(ctx, semver.BumpPatch, "")
		assertExplains(t, err, ErrBranchNotAllowed,
			[]string{`"feature/x"`, "main, master"},
			[]string{"--branch feature/x", "--any-branch"})
	})

	t.Run("tag already exists", func(t *testing.T) {
		fake := newFake()
		fake.tags = []string{"v1.2.3", "v1.3.0-rc.0", "v1.3.0-snapshot.1"}

		_, err := NewWithGit(testConfig(), fake).Plan(ctx, semver.BumpMinor, "rc")
		assertExplains(t, err, ErrTagExists,
			[]string{`"v1.3.0-rc.0"`},
			[]string{"git tag -d v1.3.0-rc.0", "git push origin --delete v1.3.0-rc.0"})
	})

	t.Run("no releasable commits", func(t *testing.T) {
		fake := newFake()
		fake.commits = nil

		_, err := NewWithGit(testConfig(), fake).Plan(ctx, semver.BumpPatch, "")
		assertExplains(t, err, ErrNoChanges,
			[]string{"v1.2.3"},
			[]string{"--allow-empty"})
	})

	t.Run("no commits at all", func(t *testing.T) {
		fake := newFake()
		fake.tags, fake.commits = nil, nil

		_, err := NewWithGit(testConfig(), fake).Plan(ctx, semver.BumpPatch, "")
		assertExplains(t, err, ErrNoChanges,
			[]string{"no history"},
			[]string{"commit something first"})
	})

	t.Run("detached head", func(t *testing.T) {
		fake := newFake()
		fake.branchErr = git.ErrDetachedHead

		_, err := NewWithGit(testConfig(), fake).Plan(ctx, semver.BumpPatch, "")
		assertExplains(t, err, git.ErrDetachedHead,
			[]string{"detached"},
			[]string{"git switch main", "--any-branch"})
	})

	t.Run("not a repository", func(t *testing.T) {
		fake := newFake()
		fake.notARepo = true

		_, err := NewWithGit(testConfig(), fake).Plan(ctx, semver.BumpPatch, "")
		assertExplains(t, err, git.ErrNotRepository,
			[]string{"not inside a Git repository"},
			[]string{"--dir"})
	})

	t.Run("unknown tag", func(t *testing.T) {
		_, err := NewWithGit(testConfig(), newFake()).Snapshot(ctx, "v9.9.9")
		assertExplains(t, err, ErrNoSuchTag,
			[]string{`"v9.9.9"`},
			[]string{"git tag -l", "git fetch --tags"})
	})

	t.Run("push failed", func(t *testing.T) {
		fake := newFake()
		fake.pushTagErr = errors.New("permission denied")

		svc := NewWithGit(testConfig(), fake)
		plan, err := svc.Plan(ctx, semver.BumpPatch, "")
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		err = svc.Apply(ctx, plan, true)
		assertExplains(t, err, nil,
			[]string{"permission denied", "still exists"},
			[]string{"git tag -d v1.2.4", "git push origin v1.2.4"})
	})
}

// assertExplains checks that an error is classifiable, states the detail, and
// offers the remedies.
func assertExplains(t *testing.T, err error, sentinel error, detail, solutions []string) {
	t.Helper()

	if err == nil {
		t.Fatal("expected an error")
	}
	if sentinel != nil && !errors.Is(err, sentinel) {
		t.Errorf("error %v does not wrap %v", err, sentinel)
	}

	var rich *Error
	if !errors.As(err, &rich) {
		t.Fatalf("error %v is not a *release.Error", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(rich.What), ".") {
		t.Errorf("What should be a sentence ending in a full stop, got %q", rich.What)
	}
	if len(rich.Solutions) == 0 {
		t.Error("an actionable error should offer at least one solution")
	}

	message := err.Error()
	for _, want := range detail {
		if !strings.Contains(message, want) {
			t.Errorf("message should mention %q:\n%s", want, message)
		}
	}
	for _, want := range solutions {
		if !strings.Contains(message, want) {
			t.Errorf("solutions should include %q:\n%s", want, message)
		}
	}
}
