// Package release orchestrates a release: it validates the repository, decides
// the next version, and creates and pushes the annotated tag.
//
// This package holds the release policy. The packages it builds on are
// deliberately unaware of it: semver knows nothing about Git, git knows nothing
// about versions, and changelog knows nothing about either. Everything here is
// driven through the Git interface, so the whole workflow can be exercised in
// tests without touching a repository.
package release

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// Errors a caller may want to recognise rather than merely report.
var (
	ErrDirtyWorkTree    = errors.New("working tree is not clean")
	ErrBranchNotAllowed = errors.New("branch is not allowed to publish releases")
	ErrNoChanges        = errors.New("no releasable commits since the last tag")
	ErrTagExists        = errors.New("tag already exists")
	ErrNoSuchTag        = errors.New("tag does not exist")
)

// Git is the set of repository operations a release needs. *git.Repo satisfies
// it; tests supply a fake.
type Git interface {
	EnsureRepository(ctx context.Context) error
	CurrentBranch(ctx context.Context) (string, error)
	Status(ctx context.Context) ([]string, error)
	FetchTags(ctx context.Context, remote string) error
	Tags(ctx context.Context, pattern string) ([]string, error)
	TagExists(ctx context.Context, name string) (bool, error)
	CreateTag(ctx context.Context, name, message string, sign bool) error
	PushTag(ctx context.Context, remote, name string) error
	Commits(ctx context.Context, from, to string) ([]git.Commit, error)
	HeadSHA(ctx context.Context) (string, error)
	CommitDate(ctx context.Context, rev string) (time.Time, error)
	RemoteURL(ctx context.Context, remote string) (string, error)
}

// Config is the release policy for one repository.
type Config struct {
	// Dir is the repository directory. Empty means the working directory.
	Dir string
	// Remote is the Git remote that tags are pushed to.
	Remote string
	// TagPrefix is prepended to the version to form the tag name.
	TagPrefix string
	// Branches lists the branches releases may be published from. Entries are
	// matched as shell patterns, so "release/*" works. An empty list allows any
	// branch.
	Branches []string

	// Sign creates a GPG-signed tag instead of an annotated one.
	Sign bool
	// FetchTags refreshes tags from the remote before calculating the version,
	// so a release cut on a stale clone cannot reuse a version number.
	FetchTags bool
	// AllowDirty skips the clean-working-tree check.
	AllowDirty bool
	// AllowEmpty permits a release with no commits since the last tag.
	AllowEmpty bool
}

// DefaultConfig is the policy most projects want.
func DefaultConfig() Config {
	return Config{
		Remote:    "origin",
		TagPrefix: "v",
		Branches:  []string{"main", "master"},
		FetchTags: true,
	}
}

// Service performs releases against one repository.
type Service struct {
	cfg Config
	git Git
}

// New returns a Service backed by the real git binary.
func New(cfg Config) *Service {
	return &Service{cfg: cfg, git: git.New(cfg.Dir)}
}

// NewWithGit returns a Service backed by a custom Git implementation.
func NewWithGit(cfg Config, g Git) *Service {
	return &Service{cfg: cfg, git: g}
}

// Plan is a release that has been calculated and validated but not yet applied.
type Plan struct {
	Bump    semver.Bump
	Branch  string
	HeadSHA string

	// Current is the version of the most recent tag, or 0.0.0 for a repository
	// with no releases yet.
	Current semver.Version
	// Next is the version this release will publish.
	Next semver.Version

	// PreviousTag is empty for a first release.
	PreviousTag string
	Tag         string

	Commits []changelog.Commit
	Repo    changelog.Repository
}

// IsFirstRelease reports whether the repository has no prior release tag.
func (p *Plan) IsFirstRelease() bool { return p.PreviousTag == "" }

// Release converts the plan into the input for rendering notes.
func (p *Plan) Release(date time.Time) changelog.Release {
	return changelog.Release{
		Tag:         p.Tag,
		Version:     p.Next,
		PreviousTag: p.PreviousTag,
		Date:        date,
		Repo:        p.Repo,
		Commits:     p.Commits,
	}
}

// detachedHead is the branch name reported when HEAD points straight at a
// commit, as it does for a pull request build or a tag checkout.
const detachedHead = "(detached HEAD)"

// Check runs the preflight validations without calculating a version. It is
// what `release check` calls, and what Plan runs first.
func (s *Service) Check(ctx context.Context) (branch string, err error) {
	if err := s.git.EnsureRepository(ctx); err != nil {
		return "", err
	}

	branch, err = s.git.CurrentBranch(ctx)
	switch {
	case err == nil:
	// A detached HEAD has no branch to check. That is fatal when releases are
	// restricted to particular branches, but harmless when they are not, which
	// is how a dry run works on a pull request's merge commit.
	case errors.Is(err, git.ErrDetachedHead) && len(s.cfg.Branches) == 0:
		branch = detachedHead
	default:
		return "", fmt.Errorf("determining the current branch: %w", err)
	}

	if !s.branchAllowed(branch) {
		return branch, fmt.Errorf("%w: on %q, expected one of %s",
			ErrBranchNotAllowed, branch, strings.Join(s.cfg.Branches, ", "))
	}

	if !s.cfg.AllowDirty {
		status, err := s.git.Status(ctx)
		if err != nil {
			return branch, fmt.Errorf("reading the working tree status: %w", err)
		}
		if len(status) > 0 {
			return branch, fmt.Errorf("%w:\n  %s", ErrDirtyWorkTree, strings.Join(status, "\n  "))
		}
	}
	return branch, nil
}

// branchAllowed matches a branch against the configured patterns.
func (s *Service) branchAllowed(branch string) bool {
	if len(s.cfg.Branches) == 0 {
		return true
	}
	for _, pattern := range s.cfg.Branches {
		if pattern == branch {
			return true
		}
		if ok, err := path.Match(pattern, branch); err == nil && ok {
			return true
		}
	}
	return false
}

// Plan validates the repository and calculates the next version.
//
// When prerelease is non-empty, the next version is a pre-release in that
// series, for example "rc".
func (s *Service) Plan(ctx context.Context, bump semver.Bump, prerelease string) (*Plan, error) {
	branch, err := s.Check(ctx)
	if err != nil {
		return nil, err
	}

	if s.cfg.FetchTags {
		if err := s.git.FetchTags(ctx, s.cfg.Remote); err != nil {
			return nil, fmt.Errorf("fetching tags from %s: %w", s.cfg.Remote, err)
		}
	}

	tags, err := s.taggedVersions(ctx)
	if err != nil {
		return nil, err
	}

	// A repository with no tags starts from 0.0.0, so the first patch release
	// is 0.0.1 and the first major release is 1.0.0.
	var current semver.Version
	var previousTag string
	if latest, ok := tags.latest(); ok {
		current, previousTag = latest.version, latest.tag
	}

	next := current.Bump(bump)
	if prerelease != "" {
		next = current.BumpPrerelease(bump, prerelease)
	}
	tag := next.Tag(s.cfg.TagPrefix)

	exists, err := s.git.TagExists(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("checking whether %s exists: %w", tag, err)
	}
	if exists {
		return nil, fmt.Errorf("%w: %s", ErrTagExists, tag)
	}

	commits, err := s.commits(ctx, previousTag, "HEAD")
	if err != nil {
		return nil, err
	}
	if len(commits) == 0 && !s.cfg.AllowEmpty {
		if previousTag == "" {
			return nil, fmt.Errorf("%w: the repository has no commits to release", ErrNoChanges)
		}
		return nil, fmt.Errorf("%w: %s already points at HEAD", ErrNoChanges, previousTag)
	}

	head, err := s.git.HeadSHA(ctx)
	if err != nil {
		return nil, err
	}

	return &Plan{
		Bump:        bump,
		Branch:      branch,
		HeadSHA:     head,
		Current:     current,
		Next:        next,
		PreviousTag: previousTag,
		Tag:         tag,
		Commits:     commits,
		Repo:        s.repository(ctx),
	}, nil
}

// Apply creates the annotated tag described by the plan and, when push is set,
// sends it to the remote. Pushing the tag is what triggers the release
// workflow, so it is the last thing to happen.
func (s *Service) Apply(ctx context.Context, p *Plan, push bool) error {
	message := s.tagMessage(p)
	if err := s.git.CreateTag(ctx, p.Tag, message, s.cfg.Sign); err != nil {
		return fmt.Errorf("creating tag %s: %w", p.Tag, err)
	}
	if !push {
		return nil
	}
	if err := s.git.PushTag(ctx, s.cfg.Remote, p.Tag); err != nil {
		return fmt.Errorf("pushing tag %s to %s: %w\nthe tag exists locally; delete it with `git tag -d %s` before retrying",
			p.Tag, s.cfg.Remote, err, p.Tag)
	}
	return nil
}

// tagMessage is the annotation stored in the tag object: a subject line and the
// release notes, so `git show <tag>` explains the release without a network
// round trip.
func (s *Service) tagMessage(p *Plan) string {
	notes := changelog.RenderNotes(p.Release(time.Now().UTC()), changelog.DefaultSections())
	return fmt.Sprintf("Release %s\n\n%s", p.Tag, notes)
}

// Snapshot describes an existing tag: the version it names, the tag before it,
// and the commits between them. It is what the post-tag automation uses to
// render notes for a tag that has already been pushed.
func (s *Service) Snapshot(ctx context.Context, tag string) (changelog.Release, error) {
	if err := s.git.EnsureRepository(ctx); err != nil {
		return changelog.Release{}, err
	}

	exists, err := s.git.TagExists(ctx, tag)
	if err != nil {
		return changelog.Release{}, err
	}
	if !exists {
		return changelog.Release{}, fmt.Errorf("%w: %s", ErrNoSuchTag, tag)
	}

	version, err := semver.Parse(strings.TrimPrefix(tag, s.cfg.TagPrefix))
	if err != nil {
		return changelog.Release{}, fmt.Errorf("tag %s: %w", tag, err)
	}

	tags, err := s.taggedVersions(ctx)
	if err != nil {
		return changelog.Release{}, err
	}

	previousTag := ""
	if prev, ok := tags.predecessorOf(version); ok {
		previousTag = prev.tag
	}

	commits, err := s.commits(ctx, previousTag, tag)
	if err != nil {
		return changelog.Release{}, err
	}

	date, err := s.git.CommitDate(ctx, tag)
	if err != nil {
		// A missing date should not stop a release from being published.
		date = time.Now().UTC()
	}

	return changelog.Release{
		Tag:         tag,
		Version:     version,
		PreviousTag: previousTag,
		Date:        date.UTC(),
		Repo:        s.repository(ctx),
		Commits:     commits,
	}, nil
}

// LatestTag returns the highest-precedence release tag, or "" when there is
// none.
func (s *Service) LatestTag(ctx context.Context) (string, error) {
	if err := s.git.EnsureRepository(ctx); err != nil {
		return "", err
	}
	tags, err := s.taggedVersions(ctx)
	if err != nil {
		return "", err
	}
	if latest, ok := tags.latest(); ok {
		return latest.tag, nil
	}
	return "", nil
}

// commits reads the commit range and converts it into the changelog's own
// commit type.
func (s *Service) commits(ctx context.Context, from, to string) ([]changelog.Commit, error) {
	raw, err := s.git.Commits(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("reading commits: %w", err)
	}
	out := make([]changelog.Commit, 0, len(raw))
	for _, c := range raw {
		out = append(out, changelog.Commit{
			SHA:         c.SHA,
			Subject:     c.Subject,
			Body:        c.Body,
			AuthorName:  c.AuthorName,
			AuthorEmail: c.AuthorEmail,
		})
	}
	return out, nil
}

// repository resolves the remote into an owner and name for changelog links.
// A repository without a usable remote still gets a changelog, just without
// links, so this never fails the release.
func (s *Service) repository(ctx context.Context) changelog.Repository {
	url, err := s.git.RemoteURL(ctx, s.cfg.Remote)
	if err != nil {
		return changelog.Repository{}
	}
	host, owner, name, err := git.ParseRemoteURL(url)
	if err != nil {
		return changelog.Repository{}
	}
	return changelog.Repository{Host: host, Owner: owner, Name: name}
}
