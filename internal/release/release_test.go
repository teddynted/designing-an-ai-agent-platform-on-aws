package release

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// fakeGit is an in-memory repository. Zero values mean "clean tree, no tags".
type fakeGit struct {
	notARepo  bool
	branch    string
	branchErr error
	status    []string
	tags      []string
	commits   map[string][]git.Commit // keyed by "from..to"
	remote    string
	remoteErr error
	date      time.Time

	createTagErr error
	pushTagErr   error
	fetchErr     error

	created    []string
	pushed     []string
	fetched    int
	tagMessage string
	signed     bool
}

func (f *fakeGit) EnsureRepository(context.Context) error {
	if f.notARepo {
		return git.ErrNotRepository
	}
	return nil
}

func (f *fakeGit) CurrentBranch(context.Context) (string, error) {
	if f.branchErr != nil {
		return "", f.branchErr
	}
	return f.branch, nil
}

func (f *fakeGit) Status(context.Context) ([]string, error) { return f.status, nil }

func (f *fakeGit) FetchTags(_ context.Context, _ string) error {
	f.fetched++
	return f.fetchErr
}

func (f *fakeGit) Tags(_ context.Context, _ string) ([]string, error) { return f.tags, nil }

func (f *fakeGit) TagExists(_ context.Context, name string) (bool, error) {
	return slices.Contains(f.tags, name), nil
}

func (f *fakeGit) CreateTag(_ context.Context, name, message string, sign bool) error {
	if f.createTagErr != nil {
		return f.createTagErr
	}
	f.created = append(f.created, name)
	f.tagMessage = message
	f.signed = sign
	f.tags = append(f.tags, name)
	return nil
}

func (f *fakeGit) PushTag(_ context.Context, remote, name string) error {
	if f.pushTagErr != nil {
		return f.pushTagErr
	}
	f.pushed = append(f.pushed, remote+"/"+name)
	return nil
}

func (f *fakeGit) Commits(_ context.Context, from, to string) ([]git.Commit, error) {
	return f.commits[from+".."+to], nil
}

func (f *fakeGit) HeadSHA(context.Context) (string, error) { return "headsha0000000", nil }

func (f *fakeGit) CommitDate(context.Context, string) (time.Time, error) { return f.date, nil }

func (f *fakeGit) RemoteURL(context.Context, string) (string, error) {
	if f.remoteErr != nil {
		return "", f.remoteErr
	}
	return f.remote, nil
}

// newFake returns a repository on main with one feature commit since v1.2.3.
func newFake() *fakeGit {
	return &fakeGit{
		branch: "main",
		tags:   []string{"v1.0.0", "v1.2.3", "v1.1.0"},
		remote: "git@github.com:teddynted/repo.git",
		date:   time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		commits: map[string][]git.Commit{
			"v1.2.3..HEAD":   {{SHA: "aaaaaaabbbb", Subject: "feat: add a thing"}},
			"v1.1.0..v1.2.3": {{SHA: "cccccccdddd", Subject: "fix: fix a thing"}},
		},
	}
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.FetchTags = false
	return cfg
}

func TestPlanCalculatesNextVersion(t *testing.T) {
	for _, tt := range []struct {
		bump semver.Bump
		want string
	}{
		{semver.BumpPatch, "v1.2.4"},
		{semver.BumpMinor, "v1.3.0"},
		{semver.BumpMajor, "v2.0.0"},
	} {
		t.Run(tt.bump.String(), func(t *testing.T) {
			svc := NewWithGit(testConfig(), newFake())
			plan, err := svc.Plan(context.Background(), tt.bump, "")
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.Tag != tt.want {
				t.Errorf("Tag = %q, want %q", plan.Tag, tt.want)
			}
			// The predecessor is the highest tag by precedence, not the last
			// one git happened to list.
			if plan.PreviousTag != "v1.2.3" {
				t.Errorf("PreviousTag = %q, want v1.2.3", plan.PreviousTag)
			}
			if plan.Current.String() != "1.2.3" {
				t.Errorf("Current = %s, want 1.2.3", plan.Current)
			}
			if plan.Repo.Owner != "teddynted" || plan.Repo.Name != "repo" {
				t.Errorf("Repo = %+v", plan.Repo)
			}
			if len(plan.Commits) != 1 || plan.Commits[0].Subject != "feat: add a thing" {
				t.Errorf("Commits = %+v", plan.Commits)
			}
		})
	}
}

func TestPlanPrerelease(t *testing.T) {
	svc := NewWithGit(testConfig(), newFake())
	plan, err := svc.Plan(context.Background(), semver.BumpMinor, "rc")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Tag != "v1.3.0-rc.0" {
		t.Errorf("Tag = %q, want v1.3.0-rc.0", plan.Tag)
	}
}

func TestPlanFirstRelease(t *testing.T) {
	fake := newFake()
	fake.tags = nil
	fake.commits = map[string][]git.Commit{
		"..HEAD": {{SHA: "aaaaaaabbbb", Subject: "feat: initial commit"}},
	}

	svc := NewWithGit(testConfig(), fake)
	plan, err := svc.Plan(context.Background(), semver.BumpMinor, "")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Tag != "v0.1.0" {
		t.Errorf("Tag = %q, want v0.1.0", plan.Tag)
	}
	if !plan.IsFirstRelease() {
		t.Error("IsFirstRelease() = false, want true")
	}
}

// Tags that do not parse, or that belong to another prefix, must be ignored
// rather than break every future release.
func TestPlanIgnoresUnrelatedTags(t *testing.T) {
	fake := newFake()
	fake.tags = []string{"v1.2.3", "vnot-a-version", "release-9.9.9", "v2.0.0-not..valid"}

	svc := NewWithGit(testConfig(), fake)
	plan, err := svc.Plan(context.Background(), semver.BumpPatch, "")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Tag != "v1.2.4" {
		t.Errorf("Tag = %q, want v1.2.4", plan.Tag)
	}
}

// A pre-release tag outranks the release before it, so the next stable release
// is calculated from the pre-release.
func TestPlanFromPrereleaseTag(t *testing.T) {
	fake := newFake()
	fake.tags = []string{"v1.2.3", "v1.3.0-rc.1"}
	fake.commits = map[string][]git.Commit{
		"v1.3.0-rc.1..HEAD": {{SHA: "aaaaaaabbbb", Subject: "fix: last minute"}},
	}

	svc := NewWithGit(testConfig(), fake)
	plan, err := svc.Plan(context.Background(), semver.BumpMinor, "")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Tag != "v1.3.0" {
		t.Errorf("Tag = %q, want v1.3.0 (the candidate graduating)", plan.Tag)
	}
}

func TestPlanRejectsDirtyWorkTree(t *testing.T) {
	fake := newFake()
	fake.status = []string{" M go.mod"}

	svc := NewWithGit(testConfig(), fake)
	_, err := svc.Plan(context.Background(), semver.BumpPatch, "")
	if !errors.Is(err, ErrDirtyWorkTree) {
		t.Fatalf("Plan on a dirty tree = %v, want ErrDirtyWorkTree", err)
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("the error should name the offending paths: %v", err)
	}

	cfg := testConfig()
	cfg.AllowDirty = true
	if _, err := NewWithGit(cfg, fake).Plan(context.Background(), semver.BumpPatch, ""); err != nil {
		t.Errorf("AllowDirty should permit a dirty tree, got %v", err)
	}
}

func TestPlanRejectsDisallowedBranch(t *testing.T) {
	fake := newFake()
	fake.branch = "feature/x"

	svc := NewWithGit(testConfig(), fake)
	_, err := svc.Plan(context.Background(), semver.BumpPatch, "")
	if !errors.Is(err, ErrBranchNotAllowed) {
		t.Fatalf("Plan on a feature branch = %v, want ErrBranchNotAllowed", err)
	}
}

func TestPlanBranchPatterns(t *testing.T) {
	fake := newFake()
	fake.branch = "release/1.2"

	cfg := testConfig()
	cfg.Branches = []string{"main", "release/*"}
	if _, err := NewWithGit(cfg, fake).Plan(context.Background(), semver.BumpPatch, ""); err != nil {
		t.Errorf("release/1.2 should match release/*, got %v", err)
	}

	cfg.Branches = nil
	if _, err := NewWithGit(cfg, fake).Plan(context.Background(), semver.BumpPatch, ""); err != nil {
		t.Errorf("an empty branch list should allow any branch, got %v", err)
	}
}

func TestPlanRejectsDetachedHead(t *testing.T) {
	fake := newFake()
	fake.branchErr = git.ErrDetachedHead

	svc := NewWithGit(testConfig(), fake)
	if _, err := svc.Plan(context.Background(), semver.BumpPatch, ""); !errors.Is(err, git.ErrDetachedHead) {
		t.Fatalf("Plan on a detached HEAD = %v, want ErrDetachedHead", err)
	}
}

// A pull request builds a detached merge commit. A dry run there is legitimate,
// so an unrestricted branch list must tolerate a detached HEAD.
func TestPlanAllowsDetachedHeadWhenAnyBranchIsPermitted(t *testing.T) {
	fake := newFake()
	fake.branchErr = git.ErrDetachedHead

	cfg := testConfig()
	cfg.Branches = nil

	plan, err := NewWithGit(cfg, fake).Plan(context.Background(), semver.BumpPatch, "")
	if err != nil {
		t.Fatalf("Plan on a detached HEAD with no branch restriction: %v", err)
	}
	if plan.Branch != detachedHead {
		t.Errorf("Branch = %q, want %q", plan.Branch, detachedHead)
	}
}

// A stable bump can never collide, because the next version always outranks the
// highest existing tag. A pre-release can: starting the "rc" series when the
// highest tag is in a series that sorts above it lands on a version that may
// already have been tagged.
func TestPlanRejectsExistingTag(t *testing.T) {
	fake := newFake()
	fake.tags = []string{"v1.2.3", "v1.3.0-rc.0", "v1.3.0-snapshot.1"}

	svc := NewWithGit(testConfig(), fake)
	_, err := svc.Plan(context.Background(), semver.BumpMinor, "rc")
	if !errors.Is(err, ErrTagExists) {
		t.Fatalf("Plan = %v, want ErrTagExists", err)
	}
	if !strings.Contains(err.Error(), "v1.3.0-rc.0") {
		t.Errorf("the error should name the colliding tag: %v", err)
	}
}

func TestPlanRejectsEmptyRelease(t *testing.T) {
	fake := newFake()
	fake.commits = nil

	svc := NewWithGit(testConfig(), fake)
	if _, err := svc.Plan(context.Background(), semver.BumpPatch, ""); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("Plan with no new commits = %v, want ErrNoChanges", err)
	}

	cfg := testConfig()
	cfg.AllowEmpty = true
	if _, err := NewWithGit(cfg, fake).Plan(context.Background(), semver.BumpPatch, ""); err != nil {
		t.Errorf("AllowEmpty should permit a release with no commits, got %v", err)
	}
}

func TestPlanFetchesTagsWhenConfigured(t *testing.T) {
	fake := newFake()
	cfg := DefaultConfig() // FetchTags is on by default
	if _, err := NewWithGit(cfg, fake).Plan(context.Background(), semver.BumpPatch, ""); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if fake.fetched != 1 {
		t.Errorf("FetchTags called %d times, want 1", fake.fetched)
	}
}

func TestPlanWithoutRemoteStillWorks(t *testing.T) {
	fake := newFake()
	fake.remoteErr = errors.New("no such remote")

	svc := NewWithGit(testConfig(), fake)
	plan, err := svc.Plan(context.Background(), semver.BumpPatch, "")
	if err != nil {
		t.Fatalf("a missing remote should not fail planning: %v", err)
	}
	if (plan.Repo != changelog.Repository{}) {
		t.Errorf("Repo = %+v, want zero so that rendering falls back to plain text", plan.Repo)
	}
}

func TestApplyCreatesAndPushesTag(t *testing.T) {
	fake := newFake()
	svc := NewWithGit(testConfig(), fake)
	ctx := context.Background()

	plan, err := svc.Plan(ctx, semver.BumpMinor, "")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := svc.Apply(ctx, plan, true); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !slices.Equal(fake.created, []string{"v1.3.0"}) {
		t.Errorf("created = %v, want [v1.3.0]", fake.created)
	}
	if !slices.Equal(fake.pushed, []string{"origin/v1.3.0"}) {
		t.Errorf("pushed = %v, want [origin/v1.3.0]", fake.pushed)
	}
	if fake.signed {
		t.Error("the tag should be annotated, not signed, by default")
	}
	if !strings.HasPrefix(fake.tagMessage, "Release v1.3.0\n\n") {
		t.Errorf("tag message = %q, want a Release subject line", fake.tagMessage)
	}
	if !strings.Contains(fake.tagMessage, "add a thing") {
		t.Errorf("tag message should embed the notes:\n%s", fake.tagMessage)
	}
}

func TestApplyWithoutPush(t *testing.T) {
	fake := newFake()
	svc := NewWithGit(testConfig(), fake)
	ctx := context.Background()

	plan, _ := svc.Plan(ctx, semver.BumpPatch, "")
	if err := svc.Apply(ctx, plan, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fake.created) != 1 {
		t.Errorf("the tag should still be created locally, created = %v", fake.created)
	}
	if len(fake.pushed) != 0 {
		t.Errorf("nothing should be pushed, pushed = %v", fake.pushed)
	}
}

// A failed push leaves a local tag behind; the error must say how to clean up.
func TestApplyPushFailureExplainsRecovery(t *testing.T) {
	fake := newFake()
	fake.pushTagErr = errors.New("permission denied")

	svc := NewWithGit(testConfig(), fake)
	ctx := context.Background()
	plan, _ := svc.Plan(ctx, semver.BumpPatch, "")

	err := svc.Apply(ctx, plan, true)
	if err == nil {
		t.Fatal("Apply should fail when the push fails")
	}
	if !strings.Contains(err.Error(), "git tag -d v1.2.4") {
		t.Errorf("the error should explain how to recover: %v", err)
	}
}

func TestApplySigns(t *testing.T) {
	fake := newFake()
	cfg := testConfig()
	cfg.Sign = true

	svc := NewWithGit(cfg, fake)
	ctx := context.Background()
	plan, _ := svc.Plan(ctx, semver.BumpPatch, "")
	if err := svc.Apply(ctx, plan, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !fake.signed {
		t.Error("Sign should produce a signed tag")
	}
}

func TestSnapshot(t *testing.T) {
	fake := newFake()
	svc := NewWithGit(testConfig(), fake)

	rel, err := svc.Snapshot(context.Background(), "v1.2.3")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if rel.PreviousTag != "v1.1.0" {
		t.Errorf("PreviousTag = %q, want v1.1.0", rel.PreviousTag)
	}
	if rel.Version.String() != "1.2.3" {
		t.Errorf("Version = %s", rel.Version)
	}
	if len(rel.Commits) != 1 || rel.Commits[0].Subject != "fix: fix a thing" {
		t.Errorf("Commits = %+v", rel.Commits)
	}
	if !rel.Date.Equal(fake.date) {
		t.Errorf("Date = %s, want %s", rel.Date, fake.date)
	}
	if rel.Repo.Owner != "teddynted" {
		t.Errorf("Repo = %+v", rel.Repo)
	}
}

func TestSnapshotOfFirstTag(t *testing.T) {
	fake := newFake()
	fake.tags = []string{"v1.0.0"}
	fake.commits = map[string][]git.Commit{
		"..v1.0.0": {{SHA: "aaaaaaabbbb", Subject: "feat: initial"}},
	}

	svc := NewWithGit(testConfig(), fake)
	rel, err := svc.Snapshot(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if rel.PreviousTag != "" {
		t.Errorf("PreviousTag = %q, want empty for a first release", rel.PreviousTag)
	}
	if len(rel.Commits) != 1 {
		t.Errorf("a first release should cover the whole history, got %d commits", len(rel.Commits))
	}
}

func TestSnapshotUnknownTag(t *testing.T) {
	svc := NewWithGit(testConfig(), newFake())
	if _, err := svc.Snapshot(context.Background(), "v9.9.9"); !errors.Is(err, ErrNoSuchTag) {
		t.Fatalf("Snapshot of a missing tag = %v, want ErrNoSuchTag", err)
	}
}

func TestSnapshotInvalidTag(t *testing.T) {
	fake := newFake()
	fake.tags = append(fake.tags, "vnonsense")

	svc := NewWithGit(testConfig(), fake)
	if _, err := svc.Snapshot(context.Background(), "vnonsense"); !errors.Is(err, semver.ErrInvalid) {
		t.Fatalf("Snapshot of a non-version tag = %v, want a semver error", err)
	}
}

func TestLatestTag(t *testing.T) {
	svc := NewWithGit(testConfig(), newFake())
	tag, err := svc.LatestTag(context.Background())
	if err != nil {
		t.Fatalf("LatestTag: %v", err)
	}
	if tag != "v1.2.3" {
		t.Errorf("LatestTag = %q, want v1.2.3", tag)
	}

	fake := newFake()
	fake.tags = nil
	if tag, err := NewWithGit(testConfig(), fake).LatestTag(context.Background()); err != nil || tag != "" {
		t.Errorf("LatestTag with no tags = %q, %v; want \"\", nil", tag, err)
	}
}

func TestCheckRejectsNonRepository(t *testing.T) {
	fake := newFake()
	fake.notARepo = true

	svc := NewWithGit(testConfig(), fake)
	if _, err := svc.Check(context.Background()); !errors.Is(err, git.ErrNotRepository) {
		t.Fatalf("Check outside a repository = %v, want ErrNotRepository", err)
	}
}

func TestPredecessorOfUsesPrecedenceNotOrder(t *testing.T) {
	vs := versionSet{
		{tag: "v1.0.0", version: semver.MustParse("1.0.0")},
		{tag: "v1.3.0-rc.1", version: semver.MustParse("1.3.0-rc.1")},
		{tag: "v1.3.0", version: semver.MustParse("1.3.0")},
	}
	prev, ok := vs.predecessorOf(semver.MustParse("1.3.0"))
	if !ok || prev.tag != "v1.3.0-rc.1" {
		t.Errorf("predecessorOf(1.3.0) = %q, %v; want v1.3.0-rc.1", prev.tag, ok)
	}
	if _, ok := vs.predecessorOf(semver.MustParse("1.0.0")); ok {
		t.Error("the oldest tag should have no predecessor")
	}
}

func TestTrimPrefix(t *testing.T) {
	if got, ok := trimPrefix("v1.2.3", "v"); !ok || got != "1.2.3" {
		t.Errorf("trimPrefix(v1.2.3, v) = %q, %v", got, ok)
	}
	if _, ok := trimPrefix("release-1.2.3", "v"); ok {
		t.Error("a tag with another prefix should be rejected")
	}
	if got, ok := trimPrefix("1.2.3", ""); !ok || got != "1.2.3" {
		t.Errorf("an empty prefix should pass the tag through, got %q, %v", got, ok)
	}
}
