package releasenotes_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/releasenotes"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// fakeRepo answers from canned history. Nothing here touches a repository.
type fakeRepo struct {
	commits  []git.Commit
	revList  map[string][]string // "base..head" -> shas
	files    map[string][]string // sha -> paths
	filesErr error
}

func (f *fakeRepo) CommitsBetween(base, head string) ([]git.Commit, error) { return f.commits, nil }

func (f *fakeRepo) RevList(base, head string) ([]string, error) {
	return f.revList[base+".."+head], nil
}

func (f *fakeRepo) FilesChanged(base, head string) ([]string, error) {
	if f.filesErr != nil {
		return nil, f.filesErr
	}
	return f.files[head], nil
}

type fakeLabels struct {
	labels map[int][]string
	calls  int
	err    error
}

func (f *fakeLabels) PullRequestLabels(number int) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.labels[number], nil
}

func mustBuild(t *testing.T, b *releasenotes.Builder, input releasenotes.Input) releasenotes.Notes {
	t.Helper()
	notes, err := b.Build(input)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	return notes
}

func baseInput() releasenotes.Input {
	base := version.MustParse("0.1.0")
	return releasenotes.Input{
		Version:     version.MustParse("0.2.0"),
		Date:        release.MustDate("2026-07-10"),
		Repository:  "teddynted/platform",
		PreviousTag: "v0.1.0",
		Head:        "HEAD",
		Comparison: git.Comparison{
			Head:  version.MustParse("0.2.0"),
			Base:  &base,
			Files: []git.FileChange{{Path: "a.go", Kind: git.Added, Insertions: 10, Deletions: 2}},
		},
	}
}

// The shape this repository actually has: a pull request merge, a plain local
// merge, and direct commits.
func TestBuildRepresentsPullRequestsNotTheirCommits(t *testing.T) {
	repo := &fakeRepo{
		commits: []git.Commit{
			{SHA: "plain2", Subject: "Add a gitignore", Author: "Teddy", Parents: []string{"merge1"}},
			{SHA: "merge1", Subject: "Merge pull request #6 from teddynted/rm", Body: "Add the release management module",
				Author: "Teddy", Parents: []string{"old", "branchTip"}},
			{SHA: "inner1", Subject: "Add the module internals", Author: "Teddy", Parents: []string{"old"}},
		},
		revList: map[string][]string{"old..branchTip": {"inner1"}},
	}
	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), baseInput())

	features := notes.Entries(releasenotes.Features)
	if len(features) != 2 {
		t.Fatalf("expected 2 feature entries, got %d: %+v", len(features), features)
	}
	// The pull request stands for the commit it brought in.
	if features[1].Title != "Add the release management module" || features[1].Number != 6 {
		t.Errorf("pull request entry = %+v", features[1])
	}
	if features[0].Title != "Add a gitignore" || features[0].Number != 0 {
		t.Errorf("direct commit entry = %+v", features[0])
	}
	for _, entry := range features {
		if entry.Title == "Add the module internals" {
			t.Error("a commit absorbed by a pull request must not be listed separately")
		}
	}
}

// A local `Merge branch` is not a pull request. Dropping it must not hide the
// commits it brought in, because those are not absorbed.
func TestBuildDropsPlainMergesButKeepsTheirCommits(t *testing.T) {
	repo := &fakeRepo{
		commits: []git.Commit{
			{SHA: "m", Subject: "Merge branch 'release-management'", Author: "Teddy", Parents: []string{"a", "b"}},
			{SHA: "b", Subject: "Fix the numstat parser", Author: "Teddy", Parents: []string{"a"}},
			{SHA: "a", Subject: "Add the roadmap", Author: "Teddy", Parents: []string{"root"}},
		},
	}
	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), baseInput())

	for _, section := range notes.PopulatedSections() {
		for _, entry := range notes.Entries(section) {
			if strings.HasPrefix(entry.Title, "Merge branch") {
				t.Errorf("a plain merge should not appear as an entry: %+v", entry)
			}
		}
	}
	if len(notes.Entries(releasenotes.BugFixes)) != 1 {
		t.Error("the commits a plain merge brought in must still appear")
	}
	if len(notes.Entries(releasenotes.Features)) != 1 {
		t.Error("the commits a plain merge brought in must still appear")
	}
}

// The release commit the tooling wrote never appears in the notes it generates.
func TestBuildDropsTheReleaseCommit(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "rel", Subject: "Release v0.2.0", Author: "Teddy", Parents: []string{"a"}},
		{SHA: "a", Subject: "Add the roadmap", Author: "Teddy", Parents: []string{"root"}},
	}}
	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), baseInput())

	if got := notes.Statistics.Commits; got != 1 {
		t.Errorf("Statistics.Commits = %d, want 1 (the release commit is not work)", got)
	}
	for _, section := range notes.PopulatedSections() {
		for _, entry := range notes.Entries(section) {
			if strings.HasPrefix(entry.Title, "Release v") {
				t.Errorf("the release commit leaked into the notes: %+v", entry)
			}
		}
	}
}

// Classification must read the pull request's title. "Merge pull request #6
// from teddynted/branch" says nothing; the title says everything.
func TestBuildClassifiesOnThePullRequestTitle(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "m", Subject: "Merge pull request #7 from o/b", Body: "fix: correct the numstat parser",
			Author: "Teddy", Parents: []string{"a", "b"}},
	}}
	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), baseInput())

	if len(notes.Entries(releasenotes.BugFixes)) != 1 {
		t.Fatalf("the pull request title should classify it as a bug fix: %+v", notes.Sections)
	}
	if title := notes.Entries(releasenotes.BugFixes)[0].Title; title != "fix: correct the numstat parser" {
		t.Errorf("title = %q", title)
	}
}

// A breaking marker in the pull request title must reach the Breaking section.
func TestBuildDetectsBreakingChangesFromThePullRequestTitle(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "m", Subject: "Merge pull request #8 from o/b", Body: "feat!: drop the v1 endpoint",
			Author: "Teddy", Parents: []string{"a", "b"}},
	}}
	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), baseInput())

	breaking := notes.Entries(releasenotes.Breaking)
	if len(breaking) != 1 || !breaking[0].Breaking {
		t.Fatalf("breaking = %+v", breaking)
	}
	if breaking[0].Number != 8 {
		t.Errorf("number = %d", breaking[0].Number)
	}
}

func TestBuildUsesLabelsWhenAvailable(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "m", Subject: "Merge pull request #6 from o/b", Body: "Rework the seam",
			Author: "Teddy", Parents: []string{"a", "b"}},
	}}
	labels := &fakeLabels{labels: map[int][]string{6: {"security"}}}

	notes := mustBuild(t, releasenotes.NewBuilder(repo, labels, nil), baseInput())
	if len(notes.Entries(releasenotes.Security)) != 1 {
		t.Errorf("the security label should have won: %+v", notes.Sections)
	}
}

// A rate limit must degrade the classification, not fail the release.
func TestBuildToleratesALabelSourceThatFails(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "m", Subject: "Merge pull request #6 from o/b", Body: "Add the roadmap registry",
			Author: "Teddy", Parents: []string{"a", "b"}},
	}}
	labels := &fakeLabels{err: errors.New("403 rate limited")}

	notes := mustBuild(t, releasenotes.NewBuilder(repo, labels, nil), baseInput())
	if len(notes.Entries(releasenotes.Features)) != 1 {
		t.Errorf("classification should fall back to the title: %+v", notes.Sections)
	}
}

// Labels for the same pull request are fetched once.
func TestBuildCachesLabelLookups(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "m1", Subject: "Add a thing (#6)", Author: "Teddy", Parents: []string{"a"}},
		{SHA: "m2", Subject: "Add another thing (#6)", Author: "Teddy", Parents: []string{"b"}},
	}}
	labels := &fakeLabels{labels: map[int][]string{6: {"feature"}}}

	mustBuild(t, releasenotes.NewBuilder(repo, labels, nil), baseInput())
	if labels.calls != 1 {
		t.Errorf("expected 1 label lookup, got %d", labels.calls)
	}
}

// Path evidence is a refinement. A release must not fail because one commit's
// diff could not be read.
func TestBuildToleratesUnreadableDiffs(t *testing.T) {
	repo := &fakeRepo{
		commits:  []git.Commit{{SHA: "a", Subject: "Add the roadmap", Author: "Teddy", Parents: []string{"root"}}},
		filesErr: errors.New("fatal: bad object"),
	}
	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), baseInput())
	if len(notes.Entries(releasenotes.Features)) != 1 {
		t.Errorf("build should have continued: %+v", notes.Sections)
	}
}

func TestBuildUsesFilePathsForDocumentation(t *testing.T) {
	repo := &fakeRepo{
		commits: []git.Commit{{SHA: "a", Subject: "Rework the overview", Author: "Teddy", Parents: []string{"root"}}},
		files:   map[string][]string{"a": {"docs/architecture/01-overview.md"}},
	}
	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), baseInput())
	if len(notes.Entries(releasenotes.Documentation)) != 1 {
		t.Errorf("a docs-only change belongs under Documentation: %+v", notes.Sections)
	}
}

func TestBuildStatistics(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "a", Subject: "Add a thing", Author: "Teddy", Parents: []string{"root"}},
		{SHA: "b", Subject: "Fix a thing", Author: "Ada", Parents: []string{"a"}},
		{SHA: "rel", Subject: "Release v0.2.0", Author: "Teddy", Parents: []string{"b"}},
	}}
	input := baseInput()
	input.Comparison.Files = []git.FileChange{
		{Path: "a.go", Kind: git.Added, Insertions: 10, Deletions: 2},
		{Path: "b.go", Kind: git.Modified, Insertions: 1, Deletions: 3},
	}

	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), input)
	s := notes.Statistics

	if s.Commits != 2 {
		t.Errorf("Commits = %d, want 2 (the release commit is excluded)", s.Commits)
	}
	if s.Contributors != 2 {
		t.Errorf("Contributors = %d, want 2", s.Contributors)
	}
	if s.FilesChanged != 2 || s.Insertions != 11 || s.Deletions != 5 {
		t.Errorf("stats = %+v", s)
	}
}

// Logins come from the forge; git records names. A contributor with a login is
// @-mentionable, one without is not.
func TestBuildContributorsPreferLogins(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "a", Subject: "Add a thing", Author: "Teddy Kekana", Parents: []string{"root"}},
		{SHA: "b", Subject: "Fix a thing", Author: "Ada Lovelace", Parents: []string{"a"}},
	}}
	input := baseInput()
	input.Comparison.Commits = []git.Commit{{SHA: "a", Login: "teddynted"}}

	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), input)
	if len(notes.Contributors) != 2 {
		t.Fatalf("contributors = %+v", notes.Contributors)
	}

	mentions := []string{notes.Contributors[0].Mention(), notes.Contributors[1].Mention()}
	// Sorted case-insensitively by mention: "@teddynted" then "Ada Lovelace"?
	// "@" sorts below letters, so the login comes first.
	if !slicesContain(mentions, "@teddynted") {
		t.Errorf("a matched author should be @-mentioned: %v", mentions)
	}
	if !slicesContain(mentions, "Ada Lovelace") {
		t.Errorf("an unmatched author falls back to their name: %v", mentions)
	}
}

// The same person committing under two names is credited once.
func TestBuildContributorsDeduplicateByLogin(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "a", Subject: "Add a thing", Author: "Teddy Kekana", Parents: []string{"root"}},
		{SHA: "b", Subject: "Fix a thing", Author: "teddy", Parents: []string{"a"}},
	}}
	input := baseInput()
	input.Comparison.Commits = []git.Commit{
		{SHA: "a", Login: "teddynted"},
		{SHA: "b", Login: "teddynted"},
	}

	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), input)
	if len(notes.Contributors) != 1 {
		t.Errorf("one person, one credit: %+v", notes.Contributors)
	}
}

// The Highlights prose is written by a human, in RELEASES.yaml.
func TestBuildHighlightsComeFromTheRoadmap(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{{SHA: "a", Subject: "Add a thing", Author: "Teddy", Parents: []string{"r"}}}}
	input := baseInput()
	input.Roadmap = &release.Release{
		Version:    version.MustParse("0.2.0"),
		Title:      "Release management",
		Summary:    "A Go release-management module.",
		Highlights: []string{"SemVer arithmetic", "Changelog generation"},
	}

	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), input)
	if notes.Summary != "A Go release-management module." {
		t.Errorf("Summary = %q", notes.Summary)
	}
	if len(notes.Highlights) != 2 || notes.Highlights[0] != "SemVer arithmetic" {
		t.Errorf("Highlights = %v", notes.Highlights)
	}
}

// With no roadmap prose, the fallback states measurements rather than inventing
// a narrative.
func TestBuildFallbackSummaryStatesFactsOnly(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{
		{SHA: "a", Subject: "Add a thing", Author: "Teddy", Parents: []string{"r"}},
		{SHA: "b", Subject: "Fix a thing", Author: "Ada", Parents: []string{"a"}},
	}}
	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), baseInput())

	want := "2 commits from 2 contributors, across 1 file."
	if notes.Summary != want {
		t.Errorf("Summary = %q, want %q", notes.Summary, want)
	}
}

func TestBuildFallbackSummaryForTheFirstRelease(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{{SHA: "a", Subject: "Initial commit", Author: "Teddy"}}}
	input := baseInput()
	input.PreviousTag = ""
	input.Comparison.Base = nil

	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), input)
	if !strings.HasPrefix(notes.Summary, "The first release.") {
		t.Errorf("Summary = %q", notes.Summary)
	}
	if !notes.IsInitial() {
		t.Error("a release with no previous tag is the first")
	}
}

func TestBuildEmptyRelease(t *testing.T) {
	repo := &fakeRepo{commits: []git.Commit{{SHA: "rel", Subject: "Release v0.2.0", Author: "Teddy"}}}
	notes := mustBuild(t, releasenotes.NewBuilder(repo, nil, nil), baseInput())

	if !notes.IsEmpty() {
		t.Errorf("a release of only the release commit has no entries: %+v", notes.Sections)
	}
	if len(notes.Contributors) != 0 {
		t.Errorf("contributors = %+v", notes.Contributors)
	}
}

func slicesContain(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
