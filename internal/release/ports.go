package release

import (
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// The seams. Interfaces the services depend on, and adapters satisfy.
//
// An implementation never imports this file: git.Repo and github.Releases
// satisfy these structurally, and a test fake is a plain struct with the right
// methods. That is the whole point — GitHubReleases and a map-backed fake are
// interchangeable to the notes builder, which is what lets note generation be
// tested without a network.
//
// Two ports, split by what they abstract rather than by who provides them:
//
//   - Repository — the local repository: tags, commits, diffs.
//   - Host — the forge: published releases, and a server-side compare.
//
// Host.Compare exists alongside Repository.Compare on purpose. GitHub's Compare
// API returns rename detection and per-file line counts for a range that may not
// exist in a shallow CI clone, and it is the appropriate source when the tag
// being released has been pushed. The local implementation is the fallback, and
// the only option offline.

// Repository reads and writes the local repository's tags and history.
type Repository interface {
	// ListTags returns every release tag, ascending by SemVer precedence.
	// Non-release tags are omitted rather than raising.
	ListTags() ([]git.Tag, error)

	// LatestTag is the highest release tag, or nil in a repository with none.
	LatestTag() (*git.Tag, error)

	// CurrentTag is the release tag pointing at HEAD, if any.
	CurrentTag() (*git.Tag, error)

	// PreviousTag is the release tag immediately below target. A nil target
	// means "below the latest". It is defined by precedence rather than by
	// position, so it is correct even when target does not exist yet.
	PreviousTag(target *version.Version) (*git.Tag, error)

	// TagExists reports whether a tag of that exact name is present.
	TagExists(name string) (bool, error)

	// CreateTag creates an annotated tag. Annotated, never lightweight: a
	// release is an object with an author and a date, not a moving pointer.
	CreateTag(name, message, ref string) (git.Tag, error)

	// CommitsBetween returns commits reachable from head but not base, newest
	// first.
	CommitsBetween(base, head string) ([]git.Commit, error)

	// Compare diffs two refs. headVersion names the release when head is a ref
	// such as HEAD that does not parse as a version.
	Compare(base, head string, headVersion *version.Version) (git.Comparison, error)
}

// CreateOptions describes a release to publish.
type CreateOptions struct {
	Tag        string
	Title      string
	Body       string
	Draft      bool
	Prerelease bool
	Target     string // commit-ish the tag should point at; "" means the default branch
}

// UpdateOptions patches an existing release. Nil fields are left untouched.
type UpdateOptions struct {
	Title      *string
	Body       *string
	Draft      *bool
	Prerelease *bool
}

// IsEmpty reports whether an update would change nothing.
func (u UpdateOptions) IsEmpty() bool {
	return u.Title == nil && u.Body == nil && u.Draft == nil && u.Prerelease == nil
}

// Host is published releases on the forge.
type Host interface {
	// ListReleases returns published and draft releases, newest version first.
	ListReleases(limit int) ([]Release, error)

	// LatestRelease is the forge's notion of "latest": the newest non-draft,
	// non-prerelease. It returns nil when the repository has none.
	LatestRelease() (*Release, error)

	// ReleaseByTag returns nil when no release exists for the tag.
	ReleaseByTag(tag string) (*Release, error)

	// CreateRelease publishes a release for an existing tag.
	CreateRelease(options CreateOptions) (Release, error)

	// UpdateRelease patches an existing release.
	UpdateRelease(tag string, options UpdateOptions) (Release, error)

	// DeleteRelease deletes the release. The underlying git tag survives.
	DeleteRelease(tag string) error

	// Compare performs a server-side comparison between two refs.
	Compare(base, head string) (git.Comparison, error)
}
