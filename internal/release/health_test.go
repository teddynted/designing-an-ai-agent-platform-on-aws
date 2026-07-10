package release

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
)

// checkNamed finds a check by name.
func checkNamed(t *testing.T, health Health, name string) Check {
	t.Helper()
	for _, c := range health.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, health.Checks)
	return Check{}
}

// healthOf runs Health with GITHUB_TOKEN unset unless the test sets it.
func healthOf(t *testing.T, fake *fakeGit, cfg Config) Health {
	t.Helper()
	health, err := NewWithGit(cfg, fake).Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	return health
}

func TestHealthAllClear(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "token")

	health := healthOf(t, newFake(), testConfig())
	if !health.OK() {
		t.Errorf("a clean repository should be all clear, got %+v", health.Checks)
	}
	if health.Branch != "main" {
		t.Errorf("Branch = %q", health.Branch)
	}
	if len(health.Concerns()) != 0 {
		t.Errorf("Concerns() = %v, want none", health.Concerns())
	}

	for _, name := range []string{
		"Git repository", "Release branch", "Working tree",
		"Untracked files", "Branch synchronised", "GitHub authentication",
	} {
		if c := checkNamed(t, health, name); c.Level != LevelOK {
			t.Errorf("check %q = %v (%s), want ok", name, c.Level, c.Detail)
		}
	}
}

// Modified and untracked files call for different remedies, so they are
// reported separately.
func TestHealthSeparatesModifiedFromUntracked(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "token")
	fake := newFake()
	fake.status = []string{" M go.mod", "?? scratch.txt", "?? dist/"}

	health := healthOf(t, fake, testConfig())

	tree := checkNamed(t, health, "Working tree")
	if tree.Level != LevelFail || !strings.Contains(tree.Detail, "1 change") {
		t.Errorf("Working tree = %v (%s)", tree.Level, tree.Detail)
	}
	untracked := checkNamed(t, health, "Untracked files")
	if untracked.Level != LevelFail || !strings.Contains(untracked.Detail, "2 files") {
		t.Errorf("Untracked files = %v (%s)", untracked.Level, untracked.Detail)
	}
	if health.Failures() != 2 {
		t.Errorf("Failures() = %d, want 2", health.Failures())
	}
}

// --allow-dirty downgrades the failures to warnings: the user has said they
// know, so the release proceeds, but the report still says so.
func TestHealthAllowDirtyDowngradesToWarning(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "token")
	fake := newFake()
	fake.status = []string{" M go.mod", "?? scratch.txt"}

	cfg := testConfig()
	cfg.AllowDirty = true
	health := healthOf(t, fake, cfg)

	if health.Failures() != 0 {
		t.Errorf("--allow-dirty should produce no failures, got %+v", health.Checks)
	}
	if health.Warnings() != 2 {
		t.Errorf("Warnings() = %d, want 2", health.Warnings())
	}
	if got := checkNamed(t, health, "Working tree").Detail; !strings.Contains(got, "--allow-dirty") {
		t.Errorf("the warning should name the flag that allowed it: %q", got)
	}
}

func TestHealthDisallowedBranch(t *testing.T) {
	fake := newFake()
	fake.branch = "feature/x"

	health := healthOf(t, fake, testConfig())
	branch := checkNamed(t, health, "Release branch")

	if branch.Level != LevelFail {
		t.Errorf("Release branch = %v, want a failure", branch.Level)
	}
	if !strings.Contains(branch.Detail, "main, master") {
		t.Errorf("the detail should name the allowed branches: %q", branch.Detail)
	}
}

// A detached HEAD blocks a normal release, but is merely a warning when the
// branch restriction has been lifted — which is how a pull request dry run works.
func TestHealthDetachedHead(t *testing.T) {
	fake := newFake()
	fake.branchErr = git.ErrDetachedHead

	if got := checkNamed(t, healthOf(t, fake, testConfig()), "Release branch"); got.Level != LevelFail {
		t.Errorf("a detached HEAD should fail a restricted release, got %v", got.Level)
	}

	cfg := testConfig()
	cfg.Branches = nil
	if got := checkNamed(t, healthOf(t, fake, cfg), "Release branch"); got.Level != LevelWarn {
		t.Errorf("--any-branch should downgrade a detached HEAD to a warning, got %v", got.Level)
	}
}

// Being behind the upstream means the tag would miss commits, which is nearly
// always a mistake. Being ahead is just unpushed work.
func TestHealthSynchronisation(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "token")

	t.Run("behind", func(t *testing.T) {
		fake := newFake()
		fake.behind = 3

		got := checkNamed(t, healthOf(t, fake, testConfig()), "Branch synchronised")
		if got.Level != LevelWarn || !strings.Contains(got.Detail, "3 commits behind") {
			t.Errorf("behind = %v (%s)", got.Level, got.Detail)
		}
		if !strings.Contains(got.Detail, "miss those commits") {
			t.Errorf("the detail should explain the consequence: %q", got.Detail)
		}
	})

	t.Run("ahead", func(t *testing.T) {
		fake := newFake()
		fake.ahead = 2

		got := checkNamed(t, healthOf(t, fake, testConfig()), "Branch synchronised")
		if got.Level != LevelWarn || !strings.Contains(got.Detail, "2 commits ahead") {
			t.Errorf("ahead = %v (%s)", got.Level, got.Detail)
		}
	})

	t.Run("no upstream", func(t *testing.T) {
		fake := newFake()
		fake.upstreamErr = git.ErrNoUpstream

		got := checkNamed(t, healthOf(t, fake, testConfig()), "Branch synchronised")
		if got.Level != LevelWarn || !strings.Contains(got.Detail, "tracks no remote") {
			t.Errorf("no upstream = %v (%s)", got.Level, got.Detail)
		}
	})

	// A git failure here must not fail the release: the check is advisory.
	t.Run("git failure is a warning", func(t *testing.T) {
		fake := newFake()
		fake.aheadBehindErr = errors.New("rev-list exploded")

		got := checkNamed(t, healthOf(t, fake, testConfig()), "Branch synchronised")
		if got.Level != LevelWarn {
			t.Errorf("an unreadable upstream should warn, not fail: %v", got.Level)
		}
	})
}

// Cutting a tag never calls the GitHub API, so a missing token is a warning.
func TestHealthAuthentication(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	got := checkNamed(t, healthOf(t, newFake(), testConfig()), "GitHub authentication")
	if got.Level != LevelWarn {
		t.Errorf("a missing token should warn, not fail: %v", got.Level)
	}
	if !strings.Contains(got.Detail, "workflow will publish") {
		t.Errorf("the detail should explain why it is only a warning: %q", got.Detail)
	}

	t.Setenv("GITHUB_TOKEN", "ghp_x")
	if got := checkNamed(t, healthOf(t, newFake(), testConfig()), "GitHub authentication"); got.Level != LevelOK {
		t.Errorf("a present token should pass: %v", got.Level)
	}
}

func TestHealthOutsideARepository(t *testing.T) {
	fake := newFake()
	fake.notARepo = true

	if _, err := NewWithGit(testConfig(), fake).Health(context.Background()); !errors.Is(err, git.ErrNotRepository) {
		t.Errorf("Health outside a repository = %v, want ErrNotRepository", err)
	}
}

func TestHealthConcerns(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	fake := newFake()
	fake.behind = 1

	concerns := healthOf(t, fake, testConfig()).Concerns()
	if len(concerns) != 2 {
		t.Fatalf("Concerns() = %v, want the sync warning and the token warning", concerns)
	}
}

func TestPartitionStatus(t *testing.T) {
	modified, untracked := partitionStatus([]string{" M a.go", "?? b.txt", "A  c.go", "?? d/"})
	if len(modified) != 2 || len(untracked) != 2 {
		t.Errorf("partitionStatus = %v / %v", modified, untracked)
	}
}

func TestLevelString(t *testing.T) {
	for level, want := range map[Level]string{LevelOK: "ok", LevelWarn: "warning", LevelFail: "failure"} {
		if got := level.String(); got != want {
			t.Errorf("Level(%d).String() = %q, want %q", level, got, want)
		}
	}
}
