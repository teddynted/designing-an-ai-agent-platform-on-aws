// Package changelog turns a range of commits into human-readable release notes
// and CHANGELOG.md entries.
//
// Commit subjects are interpreted as Conventional Commits
// (https://www.conventionalcommits.org/en/v1.0.0/) and grouped into categories.
// A subject that does not follow the convention is not discarded: it is filed
// under "Other Changes" so that nothing silently disappears from a release.
//
// Rendering runs through text/template, so a project can replace the built-in
// layout without touching Go code. See render.go for the data a template
// receives.
//
// The package operates on its own Commit type rather than on git.Commit, so
// that rendering stays independent of how commits were obtained, and it
// performs no I/O of its own.
package changelog

import (
	"fmt"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// Commit is the subset of a Git commit that release notes are derived from.
type Commit struct {
	SHA         string
	Subject     string
	Body        string
	AuthorName  string
	AuthorEmail string
}

// Repository identifies the forge the release belongs to, so that entries can
// link to commits and comparisons. A zero Repository disables linking, which is
// what lets a repository without a usable remote still produce a changelog.
type Repository struct {
	Host  string
	Owner string
	Name  string
}

// Known reports whether enough is known about the repository to build URLs.
func (r Repository) Known() bool { return r.Host != "" && r.Owner != "" && r.Name != "" }

// CommitURL returns a permalink to a commit, or "" when the repository is
// unknown.
func (r Repository) CommitURL(sha string) string {
	if !r.Known() || sha == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/%s/commit/%s", r.Host, r.Owner, r.Name, sha)
}

// CompareURL returns a diff link between two refs.
func (r Repository) CompareURL(from, to string) string {
	if !r.Known() || from == "" || to == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/%s/compare/%s...%s", r.Host, r.Owner, r.Name, from, to)
}

// CommitsURL returns a link to the history leading up to a ref. It is the
// fallback for a first release, which has nothing to compare against.
func (r Repository) CommitsURL(ref string) string {
	if !r.Known() || ref == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/%s/commits/%s", r.Host, r.Owner, r.Name, ref)
}

// Release is everything needed to render one version's notes.
type Release struct {
	Tag         string
	Version     semver.Version
	PreviousTag string
	Date        time.Time
	Repo        Repository
	Commits     []Commit

	// Bump names the increment this release applies, for the statistics block.
	// It is optional: an empty value simply omits the bump from the output.
	Bump string
}

// IsFirstRelease reports whether the release has no predecessor to compare
// against.
func (r Release) IsFirstRelease() bool { return r.PreviousTag == "" }

// CompareURL links to the diff between the previous tag and this one. It is
// empty for a first release, and for a repository with no known remote.
func (r Release) CompareURL() string { return r.Repo.CompareURL(r.PreviousTag, r.Tag) }

// HistoryURL links to the changes this release contains: a comparison against
// the previous tag, or the full commit history for a first release.
func (r Release) HistoryURL() string {
	if url := r.CompareURL(); url != "" {
		return url
	}
	return r.Repo.CommitsURL(r.Tag)
}
