package git_test

import (
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

func TestIsReleaseTag(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"v1.2.3", true},
		{"v0.1.0", true},
		{"v1.0.0-rc.1", true},
		{"v1.0.0+build.7", true},

		// A tag without the v is a valid SemVer version but not this project's
		// tag convention.
		{"1.2.3", false},

		// Real tags in this repository's namespace that are not releases.
		{"backup-before-rewrite", false},
		{"latest", false},

		{"v1.2", false},
		{"v01.2.3", false},
		{"", false},
		{"vv1.2.3", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := git.IsReleaseTag(tc.in); got != tc.want {
				t.Errorf("IsReleaseTag(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseTag(t *testing.T) {
	tag, err := git.ParseTag("v1.2.3")
	if err != nil {
		t.Fatalf("ParseTag returned error: %v", err)
	}
	if tag.Name != "v1.2.3" || tag.String() != "v1.2.3" {
		t.Errorf("tag name = %q", tag.Name)
	}
	if !tag.Version.Equal(version.MustParse("1.2.3")) {
		t.Errorf("tag version = %s", tag.Version)
	}
	if _, err := git.ParseTag("backup-before-rewrite"); err == nil {
		t.Error("ParseTag on a non-release tag should fail")
	}
}

func TestCommitShortSHA(t *testing.T) {
	tests := []struct{ sha, want string }{
		{"abcdef1234567890", "abcdef1"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := (git.Commit{SHA: tc.sha}).ShortSHA(); got != tc.want {
			t.Errorf("ShortSHA(%q) = %q, want %q", tc.sha, got, tc.want)
		}
	}
}

func TestCommitIsMerge(t *testing.T) {
	tests := []struct {
		subject string
		want    bool
	}{
		{"Merge pull request #5 from teddynted/revert/milestones-changes", true},
		{"Merge branch 'main' into feature", true},
		{"Restore milestone framing", false},
		{"Merge the two diagrams into one", false}, // not a merge commit subject
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.subject, func(t *testing.T) {
			if got := (git.Commit{Subject: tc.subject}).IsMerge(); got != tc.want {
				t.Errorf("IsMerge(%q) = %v, want %v", tc.subject, got, tc.want)
			}
		})
	}
}

func TestCommitIsBreaking(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		body    string
		want    bool
	}{
		{"bang after type", "feat!: drop the v1 endpoint", "", true},
		{"bang after scope", "feat(api)!: drop the v1 endpoint", "", true},
		{"footer with space", "feat: rework routing", "BREAKING CHANGE: the header moved", true},
		{"footer with hyphen", "feat: rework routing", "BREAKING-CHANGE: the header moved", true},
		{"footer on a later line", "feat: rework", "Some prose\nBREAKING CHANGE: gone", true},
		{"plain feature", "feat: add a flag", "", false},
		{"bang inside prose", "Fix the ! in the parser", "", false},
		{"footer mid-line", "feat: x", "see BREAKING CHANGE: below", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := git.Commit{Subject: tc.subject, Body: tc.body}
			if got := c.IsBreaking(); got != tc.want {
				t.Errorf("IsBreaking() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFileChangeDirectory(t *testing.T) {
	tests := []struct{ path, want string }{
		{"README.md", ""}, // root, not "."
		{"docs/adr/0001.md", "docs/adr"},
		{"internal/git/repo.go", "internal/git"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			if got := (git.FileChange{Path: tc.path}).Directory(); got != tc.want {
				t.Errorf("Directory(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestFileChangeValidate(t *testing.T) {
	if err := (git.FileChange{Path: "a", Kind: git.Renamed}).Validate(); err == nil {
		t.Error("a rename without a previous path should not validate")
	}
	if err := (git.FileChange{Path: "a", Kind: git.Renamed, PreviousPath: "b"}).Validate(); err != nil {
		t.Errorf("a rename with a previous path should validate: %v", err)
	}
	if err := (git.FileChange{Path: "a", Kind: git.Added}).Validate(); err != nil {
		t.Errorf("an add should validate: %v", err)
	}
}

func comparison() git.Comparison {
	base := version.MustParse("0.1.0")
	return git.Comparison{
		Head: version.MustParse("0.2.0"),
		Base: &base,
		Commits: []git.Commit{
			{SHA: "aaaaaaa1", Subject: "Add a thing"},
			{SHA: "bbbbbbb2", Subject: "Fix a thing"},
		},
		Files: []git.FileChange{
			{Path: "README.md", Kind: git.Modified, Insertions: 10, Deletions: 2},
			{Path: "docs/a.md", Kind: git.Added, Insertions: 40},
			{Path: "docs/old.md", Kind: git.Removed, Deletions: 5},
			{Path: "img/logo.png", Kind: git.Modified, Binary: true},
			{Path: "internal/new.go", Kind: git.Renamed, PreviousPath: "pkg/old.go", Insertions: 1, Deletions: 1},
		},
	}
}

func TestComparisonTotals(t *testing.T) {
	c := comparison()
	if got, want := c.CommitCount(), 2; got != want {
		t.Errorf("CommitCount() = %d, want %d", got, want)
	}
	if got, want := c.FileCount(), 5; got != want {
		t.Errorf("FileCount() = %d, want %d", got, want)
	}
	// The binary file contributes nothing, rather than zero-summing silently.
	if got, want := c.Insertions(), 51; got != want {
		t.Errorf("Insertions() = %d, want %d", got, want)
	}
	if got, want := c.Deletions(), 8; got != want {
		t.Errorf("Deletions() = %d, want %d", got, want)
	}
}

func TestComparisonFilesOfKind(t *testing.T) {
	c := comparison()
	tests := []struct {
		kind git.ChangeKind
		want int
	}{
		{git.Added, 1},
		{git.Modified, 2},
		{git.Removed, 1},
		{git.Renamed, 1},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := len(c.FilesOfKind(tc.kind)); got != tc.want {
				t.Errorf("FilesOfKind(%s) returned %d, want %d", tc.kind, got, tc.want)
			}
		})
	}
}

// A rename out of a directory changes that directory too, so both sides count.
func TestComparisonChangedDirectories(t *testing.T) {
	got := comparison().ChangedDirectories()
	want := []string{"", "docs", "img", "internal", "pkg"}
	if len(got) != len(want) {
		t.Fatalf("ChangedDirectories() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ChangedDirectories()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestComparisonRange(t *testing.T) {
	if got, want := comparison().Range(), "v0.1.0...v0.2.0"; got != want {
		t.Errorf("Range() = %q, want %q", got, want)
	}
	initial := git.Comparison{Head: version.MustParse("0.1.0")}
	if !initial.IsInitial() {
		t.Error("a comparison with no base is the initial release")
	}
	if got, want := initial.Range(), "v0.1.0"; got != want {
		t.Errorf("initial Range() = %q, want %q", got, want)
	}
}
