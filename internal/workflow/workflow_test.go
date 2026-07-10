package workflow_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/releasenotes"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/workflow"
)

// releaseCommitSHA is what the fake's Commit returns, so tests can assert that
// the tag was created on the release commit and not on HEAD.
const releaseCommitSHA = "c0mm17deadbeef"

type fakeRepo struct {
	tags       []git.Tag
	comparison git.Comparison
	branch     string

	// calls records the write-side operations in the order they were issued.
	// The ordering is the bug this whole fix is about, so it is asserted, not
	// assumed.
	calls []string

	created     []string
	tagRefs     []string
	commitMsgs  []string
	commitPaths [][]string
	pushes      [][]string

	createErr error
	commitErr error
	pushErr   error
	branchErr error
}

func (f *fakeRepo) ListTags() ([]git.Tag, error) { return f.tags, nil }

func (f *fakeRepo) LatestTag() (*git.Tag, error) {
	if len(f.tags) == 0 {
		return nil, nil
	}
	return &f.tags[len(f.tags)-1], nil
}

func (f *fakeRepo) CurrentTag() (*git.Tag, error) { return nil, nil }

func (f *fakeRepo) PreviousTag(target *version.Version) (*git.Tag, error) {
	for i := len(f.tags) - 1; i >= 0; i-- {
		if target == nil || f.tags[i].Version.Less(*target) {
			return &f.tags[i], nil
		}
	}
	return nil, nil
}

func (f *fakeRepo) TagExists(name string) (bool, error) {
	for _, tag := range f.tags {
		if tag.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRepo) CurrentBranch() (string, error) {
	if f.branchErr != nil {
		return "", f.branchErr
	}
	if f.branch == "" {
		return "main", nil
	}
	return f.branch, nil
}

func (f *fakeRepo) Commit(message string, paths ...string) (string, error) {
	f.calls = append(f.calls, "commit")
	if f.commitErr != nil {
		return "", f.commitErr
	}
	f.commitMsgs = append(f.commitMsgs, message)
	f.commitPaths = append(f.commitPaths, paths)
	return releaseCommitSHA, nil
}

func (f *fakeRepo) CreateTag(name, message, ref string) (git.Tag, error) {
	f.calls = append(f.calls, "tag")
	if f.createErr != nil {
		return git.Tag{}, f.createErr
	}
	f.created = append(f.created, name)
	f.tagRefs = append(f.tagRefs, ref)
	return git.ParseTag(name)
}

func (f *fakeRepo) Push(remote string, refs ...string) error {
	f.calls = append(f.calls, "push")
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushes = append(f.pushes, append([]string{remote}, refs...))
	return nil
}

func (f *fakeRepo) CommitsBetween(base, head string) ([]git.Commit, error) {
	return f.comparison.Commits, nil
}

func (f *fakeRepo) Compare(base, head string, headVersion *version.Version) (git.Comparison, error) {
	return f.comparison, nil
}

// The release notes builder reads history too.
func (f *fakeRepo) RevList(base, head string) ([]string, error)      { return nil, nil }
func (f *fakeRepo) FilesChanged(base, head string) ([]string, error) { return nil, nil }

type fakePublisher struct {
	published []release.CreateOptions
	err       error
}

func (f *fakePublisher) UpsertRelease(options release.CreateOptions) (release.Release, error) {
	if f.err != nil {
		return release.Release{}, f.err
	}
	f.published = append(f.published, options)
	return release.Release{Version: version.MustParse("0.2.0"), Title: options.Title}, nil
}

// harness builds a repository root with a VERSION file and a wired runner.
type harness struct {
	root      string
	repo      *fakeRepo
	publisher *fakePublisher
	runner    *workflow.Runner
}

func newHarness(t *testing.T, currentVersion string, commits []git.Commit) *harness {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte(currentVersion+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	base := version.MustParse(currentVersion)
	repo := &fakeRepo{
		tags: []git.Tag{mustTag("v" + currentVersion)},
		comparison: git.Comparison{
			Head:    version.MustParse("0.0.0"), // overwritten by headVersion
			Base:    &base,
			Commits: commits,
			Files:   []git.FileChange{{Path: "a.go", Kind: git.Added, Insertions: 10}},
		},
	}
	publisher := &fakePublisher{}

	comparisons := release.NewComparisonService(repo, nil, true, nil)
	notes := release.NewNotesService(comparisons, release.FixedClock{When: release.MustDate("2026-07-10")})

	return &harness{
		root:      root,
		repo:      repo,
		publisher: publisher,
		runner:    workflow.NewRunner(repo, comparisons, notes, releasenotes.NewBuilder(repo, nil, nil), publisher, release.FixedClock{When: release.MustDate("2026-07-10")}, nil),
	}
}

func mustTag(name string) git.Tag {
	tag, err := git.ParseTag(name)
	if err != nil {
		panic(err)
	}
	return tag
}

func defaultCommits() []git.Commit {
	return []git.Commit{
		{SHA: "aaaaaaa0", Subject: "feat: add the roadmap registry"},
		{SHA: "bbbbbbb0", Subject: "fix: correct the numstat parser"},
	}
}

func (h *harness) options(part version.Part) workflow.Options {
	return workflow.Options{
		Part:        part,
		Root:        h.root,
		Repository:  "teddynted/platform",
		SkipPublish: true,
	}
}

// writeRoadmap gives the harness repository a RELEASES.yaml.
func writeRoadmap(t *testing.T, root string) {
	t.Helper()
	yaml := "releases:\n  - version: 0.2.0\n    title: Release management\n    status: planned\n"
	if err := os.WriteFile(filepath.Join(root, "RELEASES.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.root, name))
	if err != nil {
		t.Fatalf("could not read %s: %v", name, err)
	}
	return string(raw)
}

func TestRunBumpsTagsAndWritesTheChangelog(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())

	result, err := h.runner.Run(h.options(version.Minor))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.Previous.String() != "0.1.0" || result.Next.String() != "0.2.0" {
		t.Errorf("%s -> %s", result.Previous, result.Next)
	}
	if got := strings.TrimSpace(h.read(t, "VERSION")); got != "0.2.0" {
		t.Errorf("VERSION = %q", got)
	}

	changelog := h.read(t, "CHANGELOG.md")
	if !strings.Contains(changelog, "## [0.2.0] - 2026-07-10") {
		t.Errorf("changelog missing the entry:\n%s", changelog)
	}
	if !strings.Contains(changelog, "Add the roadmap registry") {
		t.Errorf("changelog missing an entry:\n%s", changelog)
	}

	if !result.Tagged || len(h.repo.created) != 1 || h.repo.created[0] != "v0.2.0" {
		t.Errorf("tags created: %v", h.repo.created)
	}

	// SkipPublish is set: local runs push and let CI publish.
	if result.Published || len(h.publisher.published) != 0 {
		t.Error("a local run should not publish")
	}
}

// The bug this ordering exists to prevent: a tag created before the release
// commit points at a tree with no CHANGELOG entry and a stale VERSION, and CI's
// tag-matches-VERSION check fails on every release.
func TestRunCommitsBeforeTaggingAndTagsTheReleaseCommit(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())

	result, err := h.runner.Run(h.options(version.Minor))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	want := []string{"commit", "tag", "push"}
	if len(h.repo.calls) != len(want) {
		t.Fatalf("write operations = %v, want %v", h.repo.calls, want)
	}
	for i := range want {
		if h.repo.calls[i] != want[i] {
			t.Fatalf("write operations = %v, want %v", h.repo.calls, want)
		}
	}

	// The tag must be created on the release commit, never on HEAD.
	if len(h.repo.tagRefs) != 1 || h.repo.tagRefs[0] != releaseCommitSHA {
		t.Errorf("tag ref = %v, want the release commit %q", h.repo.tagRefs, releaseCommitSHA)
	}
	if h.repo.tagRefs[0] == "HEAD" {
		t.Error("tagging HEAD is the bug: HEAD predates the release commit")
	}

	if !result.Committed || result.CommitSHA != releaseCommitSHA {
		t.Errorf("result = %+v", result)
	}
}

// A release commit contains the release artefacts and nothing else, ever.
func TestReleaseCommitCarriesOnlyTheArtefacts(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	writeRoadmap(t, h.root)

	if _, err := h.runner.Run(h.options(version.Minor)); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(h.repo.commitPaths) != 1 {
		t.Fatalf("expected exactly one commit, got %d", len(h.repo.commitPaths))
	}

	want := []string{"VERSION", "CHANGELOG.md", "RELEASES.yaml"}
	got := h.repo.commitPaths[0]
	if len(got) != len(want) {
		t.Fatalf("commit pathspec = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commit pathspec = %v, want %v", got, want)
		}
	}
}

// Without a roadmap the pathspec must not name a file that does not exist —
// `git commit -- RELEASES.yaml` would refuse the whole commit.
func TestReleaseCommitOmitsAnAbsentRoadmap(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())

	if _, err := h.runner.Run(h.options(version.Minor)); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	for _, path := range h.repo.commitPaths[0] {
		if path == "RELEASES.yaml" {
			t.Errorf("pathspec names an absent roadmap: %v", h.repo.commitPaths[0])
		}
	}
}

// The release commit's own subject must never appear in the next release's
// notes, so it uses the prefix the classifier drops.
func TestReleaseCommitSubjectIsDroppedByTheClassifier(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())

	if _, err := h.runner.Run(h.options(version.Minor)); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	subject := h.repo.commitMsgs[0]
	if subject != "Release v0.2.0" {
		t.Errorf("commit subject = %q", subject)
	}
	if _, keep := release.Classify(git.Commit{SHA: "abc1234", Subject: subject}); keep {
		t.Errorf("the release commit subject %q must be dropped from release notes", subject)
	}
}

func TestRunPushesCommitAndTagAtomically(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.branch = "main"

	result, err := h.runner.Run(h.options(version.Minor))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !result.Pushed {
		t.Fatal("a release should push by default")
	}
	if len(h.repo.pushes) != 1 {
		t.Fatalf("expected one push, got %d", len(h.repo.pushes))
	}

	// remote, branch, tag — in one invocation, so they land together.
	want := []string{"origin", "main", "v0.2.0"}
	got := h.repo.pushes[0]
	if len(got) != len(want) {
		t.Fatalf("push = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("push = %v, want %v", got, want)
		}
	}
}

func TestRunHonoursACustomRemote(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	options := h.options(version.Minor)
	options.Remote = "upstream"

	if _, err := h.runner.Run(options); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.repo.pushes[0][0] != "upstream" {
		t.Errorf("pushed to %q, want upstream", h.repo.pushes[0][0])
	}
}

func TestRunSkipPushCommitsAndTagsLocally(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	options := h.options(version.Minor)
	options.SkipPush = true

	result, err := h.runner.Run(options)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !result.Committed || !result.Tagged {
		t.Error("--no-push still commits and tags")
	}
	if result.Pushed || len(h.repo.pushes) != 0 {
		t.Error("--no-push must not push")
	}
}

// A detached HEAD has no branch to push. Discover that before committing and
// tagging, not after.
func TestRunRefusesDetachedHEADBeforeWritingAnything(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.branch = "HEAD"

	_, err := h.runner.Run(h.options(version.Minor))
	if err == nil {
		t.Fatal("Run on a detached HEAD should refuse to push")
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Errorf("error = %v", err)
	}
	if len(h.repo.calls) != 0 {
		t.Errorf("nothing should have been written: %v", h.repo.calls)
	}
	if got := strings.TrimSpace(h.read(t, "VERSION")); got != "0.1.0" {
		t.Errorf("VERSION was written despite the refusal: %q", got)
	}
}

// ...but a purely local release on a detached HEAD is fine.
func TestRunAllowsDetachedHEADWhenNotPushing(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.branch = "HEAD"
	options := h.options(version.Minor)
	options.SkipPush = true

	if _, err := h.runner.Run(options); err != nil {
		t.Fatalf("--no-push on a detached HEAD should succeed: %v", err)
	}
}

// A failed commit must leave no tag and no push behind.
func TestRunDoesNotTagWhenTheCommitFails(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.commitErr = errors.New("nothing to commit, working tree clean")

	_, err := h.runner.Run(h.options(version.Minor))
	if err == nil {
		t.Fatal("Run should surface a commit failure")
	}
	if len(h.repo.created) != 0 {
		t.Error("a failed commit must not be tagged")
	}
	if len(h.repo.pushes) != 0 {
		t.Error("a failed commit must not be pushed")
	}
}

// A push failure leaves the commit and tag local, and says exactly how to retry.
func TestRunReportsAPushFailureWithTheRetryCommand(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.pushErr = errors.New("failed to push some refs")

	result, err := h.runner.Run(h.options(version.Minor))
	if err == nil {
		t.Fatal("expected a push failure")
	}
	if !result.Committed || !result.Tagged || result.Pushed {
		t.Errorf("result = %+v", result)
	}
	if !strings.Contains(err.Error(), "git push --atomic origin main v0.2.0") {
		t.Errorf("error should name the retry command: %v", err)
	}
	if len(h.publisher.published) != 0 {
		t.Error("a failed push must not publish")
	}
}

func TestRunBumpLevels(t *testing.T) {
	for _, tc := range []struct {
		part version.Part
		want string
	}{
		{version.Patch, "0.1.1"},
		{version.Minor, "0.2.0"},
		{version.Major, "1.0.0"},
	} {
		t.Run(string(tc.part), func(t *testing.T) {
			h := newHarness(t, "0.1.0", defaultCommits())
			result, err := h.runner.Run(h.options(tc.part))
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if result.Next.String() != tc.want {
				t.Errorf("Next = %s, want %s", result.Next, tc.want)
			}
		})
	}
}

// Nothing is written until everything that can be checked has been checked.
func TestDryRunWritesNothing(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	options := h.options(version.Minor)
	options.DryRun = true

	result, err := h.runner.Run(options)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !result.DryRun || result.Next.String() != "0.2.0" {
		t.Errorf("result = %+v", result)
	}
	if got := strings.TrimSpace(h.read(t, "VERSION")); got != "0.1.0" {
		t.Errorf("a dry run wrote VERSION: %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.root, "CHANGELOG.md")); !os.IsNotExist(err) {
		t.Error("a dry run wrote CHANGELOG.md")
	}
	if len(h.repo.created) != 0 {
		t.Errorf("a dry run created tags: %v", h.repo.created)
	}
	if result.Tagged || result.Published {
		t.Error("a dry run reported writes it did not make")
	}
	// The notes are still computed, which is the point of a dry run.
	if result.Notes.IsEmpty() {
		t.Error("a dry run should still generate notes")
	}
}

// A tag that already exists means a half-finished release or a mistaken bump.
// Both want a human, and neither wants VERSION rewritten first.
func TestRunRefusesAnExistingTagBeforeWritingAnything(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.tags = append(h.repo.tags, mustTag("v0.2.0"))

	_, err := h.runner.Run(h.options(version.Minor))
	if err == nil {
		t.Fatal("Run should refuse to re-create an existing tag")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v", err)
	}
	if got := strings.TrimSpace(h.read(t, "VERSION")); got != "0.1.0" {
		t.Errorf("VERSION was written despite the refusal: %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.root, "CHANGELOG.md")); !os.IsNotExist(err) {
		t.Error("CHANGELOG.md was written despite the refusal")
	}
}

func TestRunFailsOnAMissingVersionFile(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	os.Remove(filepath.Join(h.root, "VERSION"))

	if _, err := h.runner.Run(h.options(version.Minor)); err == nil {
		t.Error("Run without a VERSION file should fail")
	}
}

// The changelog is written before the tag, so the tag points at a tree whose
// changelog already describes it. If tagging fails we must not have published.
func TestRunDoesNotPublishWhenTaggingFails(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.createErr = errors.New("fatal: gpg failed to sign")

	options := h.options(version.Minor)
	options.SkipPublish = false

	if _, err := h.runner.Run(options); err == nil {
		t.Fatal("Run should surface a tagging failure")
	}
	if len(h.publisher.published) != 0 {
		t.Error("a failed tag must not publish a release")
	}
}

func TestRunPublishesWhenAsked(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	options := h.options(version.Minor)
	options.SkipPublish = false

	result, err := h.runner.Run(options)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !result.Published || len(h.publisher.published) != 1 {
		t.Fatalf("expected one published release, got %d", len(h.publisher.published))
	}

	published := h.publisher.published[0]
	if published.Tag != "v0.2.0" {
		t.Errorf("published tag = %q", published.Tag)
	}
	if !strings.Contains(published.Body, "feat: add the roadmap registry") {
		t.Errorf("release body missing entries:\n%s", published.Body)
	}
	// The body carries a compare link; the changelog entry does not.
	if !strings.Contains(published.Body, "compare/v0.1.0...v0.2.0") {
		t.Errorf("release body missing the compare link:\n%s", published.Body)
	}
}

// A publish failure after a successful local tag must say so precisely: the tag
// exists, the release does not, and the fix is to re-run publish.
func TestRunReportsAPublishFailureWithoutLosingTheTag(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.publisher.err = errors.New("403 rate limited")

	options := h.options(version.Minor)
	options.SkipPublish = false

	result, err := h.runner.Run(options)
	if err == nil {
		t.Fatal("expected a publish failure")
	}
	if !strings.Contains(err.Error(), "pushed v0.2.0") {
		t.Errorf("error should say the release was pushed: %v", err)
	}
	if !result.Tagged || !result.Pushed {
		t.Error("the release was committed, tagged, and pushed; the result should say so")
	}
}

func TestRunUpdatesTheRoadmapWhenPresent(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	roadmapYAML := "releases:\n  - version: 0.2.0\n    title: Release management\n    status: planned\n"
	if err := os.WriteFile(filepath.Join(h.root, "RELEASES.yaml"), []byte(roadmapYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := h.runner.Run(h.options(version.Minor)); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	updated := h.read(t, "RELEASES.yaml")
	if !strings.Contains(updated, "status: released") {
		t.Errorf("roadmap not marked released:\n%s", updated)
	}
	if !strings.Contains(updated, "date: \"2026-07-10\"") && !strings.Contains(updated, "date: 2026-07-10") {
		t.Errorf("roadmap missing the release date:\n%s", updated)
	}
	// The roadmap's title survives and reaches the release.
	if !strings.Contains(updated, "Release management") {
		t.Errorf("roadmap title lost:\n%s", updated)
	}
}

// A project without a roadmap still releases.
func TestRunWithoutARoadmap(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	if _, err := h.runner.Run(h.options(version.Minor)); err != nil {
		t.Fatalf("a missing roadmap should not fail the release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "RELEASES.yaml")); !os.IsNotExist(err) {
		t.Error("a roadmap should not be conjured into existence")
	}
}

func TestRunTitleComesFromTheRoadmap(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	roadmapYAML := "releases:\n  - version: 0.2.0\n    title: Release management\n    status: planned\n"
	os.WriteFile(filepath.Join(h.root, "RELEASES.yaml"), []byte(roadmapYAML), 0o644)

	options := h.options(version.Minor)
	options.SkipPublish = false
	if _, err := h.runner.Run(options); err != nil {
		t.Fatal(err)
	}
	if got, want := h.publisher.published[0].Title, "v0.2.0 — Release management"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
}

func TestRunTitleFallsBackToTheTag(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	options := h.options(version.Minor)
	options.SkipPublish = false
	if _, err := h.runner.Run(options); err != nil {
		t.Fatal(err)
	}
	if got, want := h.publisher.published[0].Title, "v0.2.0"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
}

// An empty release is not an error — a re-tag, for instance.
func TestRunOnAReleaseWithNoUserFacingChanges(t *testing.T) {
	h := newHarness(t, "0.1.0", []git.Commit{{SHA: "aaaaaaa0", Subject: "chore: tidy the makefile"}})

	result, err := h.runner.Run(h.options(version.Patch))
	if err != nil {
		t.Fatalf("an empty release should not fail: %v", err)
	}
	if !result.Notes.IsEmpty() {
		t.Error("notes should be empty")
	}
	if !strings.Contains(h.read(t, "CHANGELOG.md"), "No user-facing changes.") {
		t.Error("the changelog should say the release is empty")
	}
}

func TestPublishRequiresAnExistingTag(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	_, err := h.runner.Publish(version.MustParse("0.2.0"), h.options(version.Minor))
	if err == nil {
		t.Fatal("publishing an unpushed tag should fail")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v", err)
	}
}

func TestPublishUpsertsAnExistingTag(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.tags = append(h.repo.tags, mustTag("v0.2.0"))

	result, err := h.runner.Publish(version.MustParse("0.2.0"), h.options(version.Minor))
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if !result.Published || len(h.publisher.published) != 1 {
		t.Fatalf("expected one published release")
	}
	if h.publisher.published[0].Tag != "v0.2.0" {
		t.Errorf("published %q", h.publisher.published[0].Tag)
	}
	// Publish does not touch the working tree.
	if got := strings.TrimSpace(h.read(t, "VERSION")); got != "0.1.0" {
		t.Errorf("Publish wrote VERSION: %q", got)
	}
}

// tokenlessRunner has no publisher, as though GITHUB_TOKEN were unset.
func tokenlessRunner(t *testing.T) (*workflow.Runner, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepo{
		tags:       []git.Tag{mustTag("v0.1.0"), mustTag("v0.2.0")},
		comparison: git.Comparison{Commits: defaultCommits()},
	}
	comparisons := release.NewComparisonService(repo, nil, true, nil)
	notes := release.NewNotesService(comparisons, release.FixedClock{When: release.MustDate("2026-07-10")})
	return workflow.NewRunner(repo, comparisons, notes, releasenotes.NewBuilder(repo, nil, nil), nil, nil, nil), root
}

func TestPublishWithoutAPublisher(t *testing.T) {
	runner, root := tokenlessRunner(t)

	_, err := runner.Publish(version.MustParse("0.2.0"), workflow.Options{Root: root})
	if err == nil {
		t.Fatal("publishing without a token should fail")
	}
	if !strings.Contains(err.Error(), "no GitHub token") {
		t.Errorf("error should name the cause: %v", err)
	}
}

// Rendering the body needs no credentials, so a dry run must not demand them.
func TestPublishDryRunNeedsNoToken(t *testing.T) {
	runner, root := tokenlessRunner(t)

	result, err := runner.Publish(version.MustParse("0.2.0"), workflow.Options{Root: root, DryRun: true})
	if err != nil {
		t.Fatalf("a dry-run publish should not need a token: %v", err)
	}
	if result.Published {
		t.Error("a dry run should not publish")
	}
	if !strings.Contains(result.Body, "feat: add the roadmap registry") {
		t.Errorf("a dry run should still render the body:\n%s", result.Body)
	}
}

func TestPublishDryRun(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.tags = append(h.repo.tags, mustTag("v0.2.0"))

	options := h.options(version.Minor)
	options.DryRun = true

	result, err := h.runner.Publish(version.MustParse("0.2.0"), options)
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if result.Published || len(h.publisher.published) != 0 {
		t.Error("a dry run should not publish")
	}
	if result.Body == "" {
		t.Error("a dry run should still render the body")
	}
}

func TestDescribe(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	result, err := h.runner.Run(h.options(version.Minor))
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	workflow.Describe(&out, result)
	text := out.String()

	for _, want := range []string{
		"0.1.0 -> 0.2.0", "Added", "Fixed",
		"Committed the release", "Created tag v0.2.0", "Pushed main and v0.2.0",
		"GitHub Actions will publish the release.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Describe() missing %q:\n%s", want, text)
		}
	}
}

// When the release stays local, Describe must hand the push back to the reader.
func TestDescribeSkipPushPrintsTheRetryCommand(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	options := h.options(version.Minor)
	options.SkipPush = true
	result, err := h.runner.Run(options)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	workflow.Describe(&out, result)
	text := out.String()

	if !strings.Contains(text, "git push --atomic origin main v0.2.0") {
		t.Errorf("Describe() should print the push command:\n%s", text)
	}
	if strings.Contains(text, "Pushed") {
		t.Errorf("nothing was pushed:\n%s", text)
	}
}

// A detached HEAD has no branch, so `git push origin HEAD <tag>` would be bad
// advice. Say something true instead.
func TestDescribeDetachedHEADDoesNotSuggestPushingABranchNamedHEAD(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	h.repo.branch = "HEAD"
	options := h.options(version.Minor)
	options.SkipPush = true
	result, err := h.runner.Run(options)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	workflow.Describe(&out, result)
	text := out.String()

	if strings.Contains(text, "origin HEAD") {
		t.Errorf("Describe() suggested pushing a branch named HEAD:\n%s", text)
	}
	if !strings.Contains(text, "HEAD is detached") {
		t.Errorf("Describe() should explain the detached state:\n%s", text)
	}
}

func TestDescribeDryRunSaysSo(t *testing.T) {
	h := newHarness(t, "0.1.0", defaultCommits())
	options := h.options(version.Minor)
	options.DryRun = true
	result, _ := h.runner.Run(options)

	var out strings.Builder
	workflow.Describe(&out, result)
	text := out.String()

	if !strings.Contains(text, "Dry run — nothing was written.") {
		t.Errorf("Describe() should announce a dry run:\n%s", text)
	}
	if strings.Contains(text, "Created tag") {
		t.Errorf("a dry run created nothing:\n%s", text)
	}
}
