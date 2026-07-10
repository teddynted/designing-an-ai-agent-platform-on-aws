package git_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// fakeRunner answers git invocations from a table keyed by the joined arguments,
// and records every call so tests can assert on the exact command issued. That
// matters here: the difference between `base..head` and `base...head` is the
// difference between a correct release note and a wrong one.
type fakeRunner struct {
	responses map[string]string
	err       error
	calls     []string
}

func (f *fakeRunner) Run(args ...string) (string, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if f.err != nil {
		return "", f.err
	}
	out, ok := f.responses[key]
	if !ok {
		return "", errors.New("unexpected git invocation: " + key)
	}
	return out, nil
}

func (f *fakeRunner) called(substring string) bool {
	for _, call := range f.calls {
		if strings.Contains(call, substring) {
			return true
		}
	}
	return false
}

const forEachRef = "for-each-ref --format=%(refname:strip=2)\t%(objectname)\t%(creatordate:iso-strict) refs/tags"

func tagListing() string {
	// Deliberately unsorted, and salted with the non-release tags this
	// repository actually carries.
	return strings.Join([]string{
		"v0.2.0\tsha020\t2026-07-10T00:00:00+02:00",
		"backup-before-rewrite\tshabak\t2026-07-01T00:00:00+02:00",
		"v0.10.0\tsha0100\t2026-07-11T00:00:00+02:00",
		"v0.1.0\tsha010\t2026-07-09T00:00:00+02:00",
		"v0.2.0-rc.1\tsha020rc\t2026-07-08T00:00:00+02:00",
		"latest\tshalat\t2026-07-01T00:00:00+02:00",
	}, "\n") + "\n"
}

func TestListTagsSortsByPrecedenceAndSkipsNonReleases(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{forEachRef: tagListing()}}
	repo := git.NewRepoWithRunner(runner)

	tags, err := repo.ListTags()
	if err != nil {
		t.Fatalf("ListTags returned error: %v", err)
	}

	// 0.10.0 above 0.2.0 proves this is precedence, not lexical, ordering.
	want := []string{"v0.1.0", "v0.2.0-rc.1", "v0.2.0", "v0.10.0"}
	if len(tags) != len(want) {
		t.Fatalf("ListTags returned %d tags, want %d: %v", len(tags), len(want), tags)
	}
	for i, w := range want {
		if tags[i].Name != w {
			t.Errorf("tags[%d] = %s, want %s", i, tags[i].Name, w)
		}
	}
	if tags[0].SHA != "sha010" {
		t.Errorf("tags[0].SHA = %q, want sha010", tags[0].SHA)
	}
	if tags[0].Date.IsZero() {
		t.Error("tags[0].Date should have parsed")
	}
}

func TestListTagsEmptyRepository(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{forEachRef: ""}}
	tags, err := git.NewRepoWithRunner(runner).ListTags()
	if err != nil {
		t.Fatalf("ListTags returned error: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("ListTags on an untagged repository returned %v", tags)
	}
}

func TestLatestTag(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{forEachRef: tagListing()}}
	tag, err := git.NewRepoWithRunner(runner).LatestTag()
	if err != nil {
		t.Fatalf("LatestTag returned error: %v", err)
	}
	if tag == nil || tag.Name != "v0.10.0" {
		t.Fatalf("LatestTag() = %v, want v0.10.0", tag)
	}
}

func TestLatestTagOnUntaggedRepository(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{forEachRef: ""}}
	tag, err := git.NewRepoWithRunner(runner).LatestTag()
	if err != nil {
		t.Fatalf("LatestTag returned error: %v", err)
	}
	if tag != nil {
		t.Errorf("LatestTag() = %v, want nil", tag)
	}
}

func TestCurrentTagPicksHighestAtHead(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"tag --points-at HEAD": "v0.1.0\nbackup-before-rewrite\nv0.2.0\n",
	}}
	tag, err := git.NewRepoWithRunner(runner).CurrentTag()
	if err != nil {
		t.Fatalf("CurrentTag returned error: %v", err)
	}
	if tag == nil || tag.Name != "v0.2.0" {
		t.Fatalf("CurrentTag() = %v, want v0.2.0", tag)
	}
}

func TestCurrentTagUntaggedHead(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"tag --points-at HEAD": "\n"}}
	tag, err := git.NewRepoWithRunner(runner).CurrentTag()
	if err != nil {
		t.Fatalf("CurrentTag returned error: %v", err)
	}
	if tag != nil {
		t.Errorf("CurrentTag() = %v, want nil", tag)
	}
}

func TestPreviousTag(t *testing.T) {
	tests := []struct {
		name   string
		target string // "" means nil, i.e. "below the latest"
		want   string // "" means nil
	}{
		{"below the latest", "", "v0.2.0"},
		{"below an existing tag", "0.2.0", "v0.2.0-rc.1"},

		// The interesting case: the tag being released does not exist yet, so
		// the predecessor cannot be found by position in the tag list.
		{"below an unreleased version", "0.11.0", "v0.10.0"},
		{"below a version between tags", "0.3.0", "v0.2.0"},

		{"below the earliest is nothing", "0.1.0", ""},
		{"below everything is nothing", "0.0.1", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]string{forEachRef: tagListing()}}
			repo := git.NewRepoWithRunner(runner)

			var target *version.Version
			if tc.target != "" {
				v := version.MustParse(tc.target)
				target = &v
			}

			got, err := repo.PreviousTag(target)
			if err != nil {
				t.Fatalf("PreviousTag returned error: %v", err)
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("PreviousTag() = %v, want nil", got)
				}
				return
			}
			if got == nil || got.Name != tc.want {
				t.Fatalf("PreviousTag() = %v, want %s", got, tc.want)
			}
		})
	}
}

func TestCreateTagRefusesNonReleaseNames(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{}}
	if _, err := git.NewRepoWithRunner(runner).CreateTag("nightly", "msg", "HEAD"); err == nil {
		t.Fatal("CreateTag on a non-release name should fail")
	}
	if len(runner.calls) != 0 {
		t.Errorf("CreateTag should not touch git before validating the name; called %v", runner.calls)
	}
}

func TestCreateTagIsAnnotated(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"tag -a v0.2.0 -m Release v0.2.0 HEAD": "",
		"rev-list -n 1 v0.2.0":                 "deadbeefcafe\n",
	}}
	tag, err := git.NewRepoWithRunner(runner).CreateTag("v0.2.0", "Release v0.2.0", "HEAD")
	if err != nil {
		t.Fatalf("CreateTag returned error: %v", err)
	}
	if tag.SHA != "deadbeefcafe" {
		t.Errorf("tag.SHA = %q", tag.SHA)
	}
	// -a is the whole point: a release is an object, not a moving pointer.
	if !runner.called("tag -a v0.2.0") {
		t.Errorf("CreateTag did not create an annotated tag; calls: %v", runner.calls)
	}
}

func TestTagExists(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"tag --list v0.1.0": "v0.1.0\n",
		"tag --list v9.9.9": "",
	}}
	repo := git.NewRepoWithRunner(runner)

	if exists, err := repo.TagExists("v0.1.0"); err != nil || !exists {
		t.Errorf("TagExists(v0.1.0) = %v, %v; want true, nil", exists, err)
	}
	if exists, err := repo.TagExists("v9.9.9"); err != nil || exists {
		t.Errorf("TagExists(v9.9.9) = %v, %v; want false, nil", exists, err)
	}
}

func TestCurrentBranch(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"rev-parse --abbrev-ref HEAD": "release-management\n"}}
	branch, err := git.NewRepoWithRunner(runner).CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch returned error: %v", err)
	}
	if branch != "release-management" {
		t.Errorf("CurrentBranch() = %q", branch)
	}
}

// A detached HEAD reports itself as "HEAD"; the caller decides what that means.
func TestCurrentBranchDetached(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"rev-parse --abbrev-ref HEAD": "HEAD\n"}}
	branch, err := git.NewRepoWithRunner(runner).CurrentBranch()
	if err != nil || branch != "HEAD" {
		t.Errorf("CurrentBranch() = %q, %v", branch, err)
	}
}

func TestCommitStagesThenCommitsExactlyThePathspec(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"add -- VERSION CHANGELOG.md":                      "",
		"commit -m Release v0.2.0 -- VERSION CHANGELOG.md": "",
		"rev-parse HEAD":                                   "deadbeefcafe1234\n",
	}}
	sha, err := git.NewRepoWithRunner(runner).Commit("Release v0.2.0", "VERSION", "CHANGELOG.md")
	if err != nil {
		t.Fatalf("Commit returned error: %v", err)
	}
	if sha != "deadbeefcafe1234" {
		t.Errorf("Commit() = %q", sha)
	}

	// Staging first is what lets an untracked CHANGELOG.md be committed at all;
	// `git commit -- <path>` refuses a path git has never seen.
	if runner.calls[0] != "add -- VERSION CHANGELOG.md" {
		t.Errorf("expected the paths to be staged first, got %q", runner.calls[0])
	}
	// The pathspec on commit is what keeps unrelated staged work out.
	if !runner.called("commit -m Release v0.2.0 -- VERSION CHANGELOG.md") {
		t.Errorf("commit must carry a pathspec; calls: %v", runner.calls)
	}
}

func TestCommitRejectsEmptyInput(t *testing.T) {
	repo := git.NewRepoWithRunner(&fakeRunner{responses: map[string]string{}})

	if _, err := repo.Commit("Release v0.2.0"); err == nil {
		t.Error("committing without a pathspec should fail")
	}
	if _, err := repo.Commit("  ", "VERSION"); err == nil {
		t.Error("committing without a message should fail")
	}
}

func TestCommitDoesNotTagOnFailure(t *testing.T) {
	// `add` succeeds, `commit` is absent from the table and therefore errors.
	runner := &fakeRunner{responses: map[string]string{"add -- VERSION": ""}}
	if _, err := git.NewRepoWithRunner(runner).Commit("Release v0.2.0", "VERSION"); err == nil {
		t.Error("a failing commit should surface as an error")
	}
	if runner.called("rev-parse") {
		t.Error("a failed commit should not be resolved to a SHA")
	}
}

// Half a release on the remote — a tag with no commit, or a commit with no tag —
// is a state nothing in this system recovers from.
func TestPushIsAtomic(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"push --atomic origin main v0.2.0": ""}}
	if err := git.NewRepoWithRunner(runner).Push("origin", "main", "v0.2.0"); err != nil {
		t.Fatalf("Push returned error: %v", err)
	}
	if !runner.called("push --atomic origin main v0.2.0") {
		t.Errorf("push must be atomic; calls: %v", runner.calls)
	}
}

func TestPushRejectsEmptyInput(t *testing.T) {
	repo := git.NewRepoWithRunner(&fakeRunner{responses: map[string]string{}})

	if err := repo.Push("", "main"); err == nil {
		t.Error("pushing without a remote should fail")
	}
	if err := repo.Push("origin"); err == nil {
		t.Error("pushing without a ref should fail")
	}
}

func TestPushErrorPropagates(t *testing.T) {
	runner := &fakeRunner{err: errors.New("failed to push some refs")}
	if err := git.NewRepoWithRunner(runner).Push("origin", "main", "v0.2.0"); err == nil {
		t.Error("a failing push should surface as an error")
	}
}

// logOutput builds the NUL-free, separator-framed output the log format asks for.
func logOutput(records ...[5]string) string {
	var b strings.Builder
	for _, r := range records {
		b.WriteString(strings.Join(r[:], "\x1f"))
		b.WriteString("\x1e")
	}
	return b.String()
}

func TestCommitsBetweenUsesTwoDots(t *testing.T) {
	out := logOutput(
		[5]string{"sha1", "Add the roadmap", "", "Teddy", "2026-07-10T00:00:00+02:00"},
		[5]string{"sha2", "Fix the parser", "BREAKING CHANGE: gone", "Teddy", "2026-07-09T00:00:00+02:00"},
	)
	runner := &fakeRunner{responses: map[string]string{
		"log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e v0.1.0..HEAD": out,
	}}

	commits, err := git.NewRepoWithRunner(runner).CommitsBetween("v0.1.0", "HEAD")
	if err != nil {
		t.Fatalf("CommitsBetween returned error: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("CommitsBetween returned %d commits, want 2", len(commits))
	}
	if commits[0].Subject != "Add the roadmap" || commits[0].Author != "Teddy" {
		t.Errorf("commits[0] = %+v", commits[0])
	}
	if !commits[1].IsBreaking() {
		t.Error("commits[1] carries a BREAKING CHANGE footer")
	}
	if commits[0].Date.IsZero() {
		t.Error("commit date should have parsed")
	}

	// Three dots on a log is the symmetric difference, which is not "what is new".
	if runner.called("...") {
		t.Errorf("CommitsBetween must use two dots; calls: %v", runner.calls)
	}
}

func TestCommitsBetweenWithoutBase(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e HEAD": logOutput(
			[5]string{"sha1", "Initial commit", "", "Teddy", "2026-07-01T00:00:00+02:00"},
		),
	}}
	commits, err := git.NewRepoWithRunner(runner).CommitsBetween("", "HEAD")
	if err != nil {
		t.Fatalf("CommitsBetween returned error: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("want 1 commit, got %d", len(commits))
	}
}

func TestCommitsBetweenRejectsTruncatedRecord(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e HEAD": "sha1\x1fonly two fields\x1e",
	}}
	if _, err := git.NewRepoWithRunner(runner).CommitsBetween("", "HEAD"); err == nil {
		t.Error("a log record with too few fields should fail rather than silently truncate")
	}
}

// diffResponses builds the two `git diff` answers for a given rev spec.
func diffResponses(spec, nameStatus, numstat string) map[string]string {
	return map[string]string{
		"diff --name-status -M -z " + spec: nameStatus,
		"diff --numstat -M -z " + spec:     numstat,
	}
}

func TestCompareUsesThreeDotsAgainstARealBase(t *testing.T) {
	responses := diffResponses(
		"v0.1.0...v0.2.0",
		"M\x00README.md\x00A\x00docs/new.md\x00",
		"10\t2\tREADME.md\x0040\t0\tdocs/new.md\x00",
	)
	responses["log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e v0.1.0..v0.2.0"] = logOutput(
		[5]string{"sha1", "Add docs", "", "Teddy", "2026-07-10T00:00:00+02:00"},
	)
	runner := &fakeRunner{responses: responses}

	c, err := git.NewRepoWithRunner(runner).Compare("v0.1.0", "v0.2.0", nil)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}

	// The diff must use merge-base semantics so it agrees with GitHub's Compare
	// API; the log must not.
	if !runner.called("diff --name-status -M -z v0.1.0...v0.2.0") {
		t.Errorf("diff should use three dots; calls: %v", runner.calls)
	}
	if !runner.called("log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e v0.1.0..v0.2.0") {
		t.Errorf("log should use two dots; calls: %v", runner.calls)
	}

	if c.Base == nil || c.Base.String() != "0.1.0" || c.Head.String() != "0.2.0" {
		t.Errorf("Compare produced base=%v head=%v", c.Base, c.Head)
	}
	if c.FileCount() != 2 || c.Insertions() != 50 || c.Deletions() != 2 {
		t.Errorf("files=%d insertions=%d deletions=%d", c.FileCount(), c.Insertions(), c.Deletions())
	}
	// Files are sorted by path, so the ordering is deterministic across runs.
	if c.Files[0].Path != "README.md" || c.Files[1].Path != "docs/new.md" {
		t.Errorf("files not sorted by path: %v", c.Files)
	}
}

func TestCompareAgainstEmptyTreeForTheFirstRelease(t *testing.T) {
	spec := git.EmptyTree + "..v0.1.0"
	responses := diffResponses(spec, "A\x00README.md\x00", "5\t0\tREADME.md\x00")
	responses["log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e v0.1.0"] = logOutput(
		[5]string{"sha1", "Initial commit", "", "Teddy", "2026-07-01T00:00:00+02:00"},
	)
	runner := &fakeRunner{responses: responses}

	c, err := git.NewRepoWithRunner(runner).Compare("", "v0.1.0", nil)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}
	if !c.IsInitial() {
		t.Error("a comparison with no base is the initial release")
	}
	// The empty tree has no merge base with anything, so two dots there.
	if !runner.called("diff --name-status -M -z " + git.EmptyTree + "..v0.1.0") {
		t.Errorf("first release should diff the empty tree with two dots; calls: %v", runner.calls)
	}
}

// head may be a ref such as HEAD, which does not parse as a version. That is the
// case when generating notes for a tag that does not exist yet.
func TestCompareWithHeadVersionOverride(t *testing.T) {
	responses := diffResponses("v0.1.0...HEAD", "M\x00README.md\x00", "1\t1\tREADME.md\x00")
	responses["log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e v0.1.0..HEAD"] = ""
	runner := &fakeRunner{responses: responses}

	head := version.MustParse("0.2.0")
	c, err := git.NewRepoWithRunner(runner).Compare("v0.1.0", "HEAD", &head)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}
	if c.Head.String() != "0.2.0" {
		t.Errorf("Head = %s, want 0.2.0", c.Head)
	}
	if got, want := c.Range(), "v0.1.0...v0.2.0"; got != want {
		t.Errorf("Range() = %q, want %q", got, want)
	}
}

func TestCompareRejectsUnparseableHeadWithoutOverride(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{}}
	if _, err := git.NewRepoWithRunner(runner).Compare("v0.1.0", "HEAD", nil); err == nil {
		t.Error("Compare with a non-version head and no override should fail")
	}
}

// The -z parsers are where this package earns its keep. Renames are three
// fields; everything else is two. Binary files report "-", not zero.
func TestCompareParsesRenamesAndBinaries(t *testing.T) {
	nameStatus := strings.Join([]string{
		"R100", "pkg/old.go", "internal/new.go", // rename: three fields
		"M", "img/logo.png", // binary modify: two fields
		"C75", "a.txt", "b.txt", // copy: three fields, becomes an add of b.txt
		"D", "gone.md",
	}, "\x00") + "\x00"

	numstat := strings.Join([]string{
		"1\t1\t", "pkg/old.go", "internal/new.go", // rename: empty third field, then both paths
		"-\t-\timg/logo.png", // binary: dashes, not zeroes
		"3\t0\tb.txt",
		"0\t9\tgone.md",
	}, "\x00") + "\x00"

	responses := diffResponses("v0.1.0...v0.2.0", nameStatus, numstat)
	responses["log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e v0.1.0..v0.2.0"] = ""
	runner := &fakeRunner{responses: responses}

	c, err := git.NewRepoWithRunner(runner).Compare("v0.1.0", "v0.2.0", nil)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}

	byPath := map[string]git.FileChange{}
	for _, f := range c.Files {
		byPath[f.Path] = f
	}

	renamed, ok := byPath["internal/new.go"]
	if !ok || renamed.Kind != git.Renamed || renamed.PreviousPath != "pkg/old.go" {
		t.Errorf("rename not parsed: %+v", renamed)
	}
	if renamed.Insertions != 1 || renamed.Deletions != 1 {
		t.Errorf("rename line counts: %+v", renamed)
	}

	binary, ok := byPath["img/logo.png"]
	if !ok || !binary.Binary {
		t.Errorf("binary file not flagged: %+v", binary)
	}
	if binary.Insertions != 0 || binary.Deletions != 0 {
		t.Errorf("a binary file has no line counts: %+v", binary)
	}

	// A copy creates a new path; the source is untouched, so it is an add.
	copied, ok := byPath["b.txt"]
	if !ok || copied.Kind != git.Added || copied.PreviousPath != "" {
		t.Errorf("copy should become an add with no previous path: %+v", copied)
	}
	if _, present := byPath["a.txt"]; present {
		t.Error("the source of a copy should not appear as a change")
	}

	if removed := byPath["gone.md"]; removed.Kind != git.Removed || removed.Deletions != 9 {
		t.Errorf("delete not parsed: %+v", removed)
	}

	// The binary file must not drag the totals to a wrong number.
	if got, want := c.Insertions(), 4; got != want {
		t.Errorf("Insertions() = %d, want %d", got, want)
	}
	if got, want := c.Deletions(), 10; got != want {
		t.Errorf("Deletions() = %d, want %d", got, want)
	}
}

// Paths containing newlines are exactly why -z exists.
func TestCompareParsesPathsWithNewlines(t *testing.T) {
	awkward := "docs/a\nb.md"
	responses := diffResponses(
		"v0.1.0...v0.2.0",
		"A\x00"+awkward+"\x00",
		"2\t0\t"+awkward+"\x00",
	)
	responses["log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e v0.1.0..v0.2.0"] = ""
	runner := &fakeRunner{responses: responses}

	c, err := git.NewRepoWithRunner(runner).Compare("v0.1.0", "v0.2.0", nil)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}
	if c.FileCount() != 1 || c.Files[0].Path != awkward {
		t.Errorf("path with a newline was mangled: %+v", c.Files)
	}
}

func TestCompareRejectsTruncatedRenameRecord(t *testing.T) {
	responses := diffResponses("v0.1.0...v0.2.0", "R100\x00only-one-path\x00", "")
	responses["log --format=%H\x1f%s\x1f%b\x1f%an\x1f%aI\x1e v0.1.0..v0.2.0"] = ""
	runner := &fakeRunner{responses: responses}

	if _, err := git.NewRepoWithRunner(runner).Compare("v0.1.0", "v0.2.0", nil); err == nil {
		t.Error("a rename record missing its destination should fail")
	}
}

func TestRunnerErrorsPropagate(t *testing.T) {
	runner := &fakeRunner{err: errors.New("fatal: not a git repository")}
	if _, err := git.NewRepoWithRunner(runner).ListTags(); err == nil {
		t.Error("a failing git invocation should surface as an error")
	}
}
