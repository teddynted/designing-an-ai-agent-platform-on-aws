// Package git models the version-control domain and reads it from a repository.
//
// The types here — Commit, Tag, Comparison — describe what a version control
// system yields, not what `git log` prints. Nothing above the Repo type knows
// that a SHA is forty hexadecimal characters, which is what lets a Comparison be
// sourced from GitHub's Compare API rather than a local clone.
//
// Comparison lives in this package rather than in a release package because a
// diff between two refs is a version-control concept. That placement is also
// what keeps the dependency graph acyclic: git imports only version, and both
// the release domain and the GitHub adapter import git.
package git

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// This project tags releases as v0.2.0. Anything else in the tag namespace —
// backup-before-rewrite, say — is not a release and must be ignored rather than
// crashing the tooling that walks the tag list.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(?:[-+].*)?$`)

// IsReleaseTag reports whether name is a v-prefixed SemVer tag. It never errors.
//
// Stricter than version.IsValid, which also accepts the bare 1.2.3 form. A tag
// without the v is not this project's convention.
func IsReleaseTag(name string) bool {
	name = strings.TrimSpace(name)
	return releaseTag.MatchString(name) && version.IsValid(name)
}

// Tag is a tag that names a release. Non-release tags never become a Tag.
type Tag struct {
	Name    string
	Version version.Version
	SHA     string
	Date    time.Time
}

// ParseTag builds a Tag from a tag name, rejecting names that are not releases.
func ParseTag(name string) (Tag, error) {
	name = strings.TrimSpace(name)
	if !IsReleaseTag(name) {
		return Tag{}, fmt.Errorf("not a release tag: %q (expected vMAJOR.MINOR.PATCH)", name)
	}
	v, err := version.Parse(name)
	if err != nil {
		return Tag{}, err
	}
	return Tag{Name: name, Version: v}, nil
}

func (t Tag) String() string { return t.Name }

// Commit is one commit, reduced to what release notes actually need.
type Commit struct {
	SHA     string
	Subject string
	Body    string
	Author  string
	Date    time.Time

	// Parents are the commit's parent SHAs, oldest first. A merge has more than
	// one; the root commit has none. Populated by the local repository only:
	// the forge's compare payload does not carry them.
	Parents []string

	// Login is the author's account name on the forge, without the leading "@".
	// Populated by the forge adapter only; local git knows names and addresses,
	// not accounts. Empty means "unknown", not "none".
	Login string
}

// IsMergeCommit reports whether this commit has more than one parent.
//
// Distinct from IsMerge, which reads the subject: the forge's payload carries no
// parents, so subject matching is the only signal available there. Use this when
// the commit came from local git and the answer must be structural.
func (c Commit) IsMergeCommit() bool { return len(c.Parents) > 1 }

// FirstParent is the commit this one was built on, or "" for the root commit.
func (c Commit) FirstParent() string {
	if len(c.Parents) == 0 {
		return ""
	}
	return c.Parents[0]
}

// ShortSHA is the abbreviated form used in changelog entries.
func (c Commit) ShortSHA() string {
	if len(c.SHA) < 7 {
		return c.SHA
	}
	return c.SHA[:7]
}

// IsMerge reports whether the subject is one of the merge commits a non-squash
// merge leaves behind. They carry no information a release-note reader wants:
// the merged commits are already in the range.
func (c Commit) IsMerge() bool {
	return strings.HasPrefix(c.Subject, "Merge pull request ") ||
		strings.HasPrefix(c.Subject, "Merge branch ")
}

// conventionalBreaking matches the `feat!:` and `feat(scope)!:` subject markers.
var conventionalBreaking = regexp.MustCompile(`^[a-zA-Z]+(\([^)]*\))?!:`)

// breakingFooter matches the `BREAKING CHANGE:` / `BREAKING-CHANGE:` footer at
// the start of any body line.
var breakingFooter = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE:`)

// IsBreaking reports whether the commit declares a breaking change by either of
// Conventional Commits' two markers.
func (c Commit) IsBreaking() bool {
	return conventionalBreaking.MatchString(c.Subject) || breakingFooter.MatchString(c.Body)
}

// ChangeKind is how one path was touched between two refs.
type ChangeKind string

const (
	Added    ChangeKind = "added"
	Modified ChangeKind = "modified"
	Removed  ChangeKind = "removed"
	Renamed  ChangeKind = "renamed"
)

func (k ChangeKind) String() string { return string(k) }

// FileChange is one path touched between two releases.
//
// Binary files have no line counts. Binary is true there and the counts are
// zero — "no lines changed" and "lines are not a meaningful unit" are different
// facts, and silently summing the latter as zero understates a release.
type FileChange struct {
	Path         string
	Kind         ChangeKind
	Insertions   int
	Deletions    int
	Binary       bool
	PreviousPath string // set only when Kind is Renamed
}

// Validate rejects the one internally inconsistent FileChange: a rename with no
// origin, which would render as a changelog entry that cannot be read.
func (f FileChange) Validate() error {
	if f.Kind == Renamed && f.PreviousPath == "" {
		return fmt.Errorf("renamed %q without a previous path", f.Path)
	}
	return nil
}

// Directory is the containing directory, or "" for a path at the repository
// root. Note that path.Dir returns "." there, which is not what a reader wants.
func (f FileChange) Directory() string {
	dir := path.Dir(f.Path)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// Comparison is what changed between Base and Head.
//
// Base is nil for the very first release, where there is no predecessor and
// every file is, trivially, added.
//
// The same Comparison is produced whether the data came from `git diff` in a
// local clone or from GitHub's Compare API. The two disagree in one respect
// worth knowing: GitHub reports a rename as `renamed` with a previous filename,
// while git detects renames only when asked (-M) and otherwise reports an add
// plus a delete. Both adapters normalise into ChangeKind.
type Comparison struct {
	Head    version.Version
	Base    *version.Version
	Commits []Commit
	Files   []FileChange
}

// IsInitial reports whether this is the first release, with no predecessor.
func (c Comparison) IsInitial() bool { return c.Base == nil }

// Range renders the v0.1.0...v0.2.0 form GitHub uses for compare URLs.
func (c Comparison) Range() string {
	if c.Base == nil {
		return c.Head.Tag()
	}
	return c.Base.Tag() + "..." + c.Head.Tag()
}

// CommitCount is the number of commits in the range.
func (c Comparison) CommitCount() int { return len(c.Commits) }

// FileCount is the number of paths touched.
func (c Comparison) FileCount() int { return len(c.Files) }

// Insertions totals lines added across text files. Binary files contribute none.
func (c Comparison) Insertions() int {
	total := 0
	for _, f := range c.Files {
		if !f.Binary {
			total += f.Insertions
		}
	}
	return total
}

// Deletions totals lines removed across text files.
func (c Comparison) Deletions() int {
	total := 0
	for _, f := range c.Files {
		if !f.Binary {
			total += f.Deletions
		}
	}
	return total
}

// FilesOfKind returns every change of the given kind, preserving order.
func (c Comparison) FilesOfKind(kind ChangeKind) []FileChange {
	var out []FileChange
	for _, f := range c.Files {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// ChangedDirectories lists the distinct directories touched, sorted.
//
// A rename out of a directory changes that directory even though the new path
// lives elsewhere, so both sides are counted.
func (c Comparison) ChangedDirectories() []string {
	seen := map[string]struct{}{}
	for _, f := range c.Files {
		seen[f.Directory()] = struct{}{}
		if f.PreviousPath != "" {
			seen[(FileChange{Path: f.PreviousPath}).Directory()] = struct{}{}
		}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	return dirs
}
