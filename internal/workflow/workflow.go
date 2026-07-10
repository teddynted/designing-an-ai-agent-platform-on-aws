// Package workflow orchestrates a release.
//
// It owns the order of operations and nothing else: every step delegates to a
// focused package, and the interesting decisions live there. What this package
// contributes is the sequence, and the rule that governs it — nothing is written
// until everything that can be checked has been checked.
//
// The order matters. The tag is created after the changelog is written, so that
// the tag points at a commit whose changelog already describes it. Publishing to
// GitHub happens last, because it is the only step this tool cannot undo.
package workflow

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/releasenotes"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/roadmap"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// git.Repo satisfies the write-side port structurally. Asserted here so a
// signature drift is a compile error in this package, not at the call site.
var _ Repository = (*git.Repo)(nil)

// DefaultRemote is the remote a release is pushed to.
const DefaultRemote = "origin"

// detachedHEAD is what `git rev-parse --abbrev-ref HEAD` reports off a branch.
const detachedHEAD = "HEAD"

// Publisher publishes a release to the forge. Narrower than release.Host because
// this is all the workflow needs, and a fake need implement no more.
type Publisher interface {
	UpsertRelease(options release.CreateOptions) (release.Release, error)
}

// Repository is the write side of the local repository.
//
// Narrower than release.Repository on purpose: the reading services need tags
// and diffs, this needs to commit, tag, and push. Declaring it here rather than
// widening the shared port means a fake for the workflow implements six methods
// instead of ten, and the reading services cannot accidentally acquire a Push.
type Repository interface {
	TagExists(name string) (bool, error)
	CreateTag(name, message, ref string) (git.Tag, error)
	CurrentBranch() (string, error)
	Commit(message string, paths ...string) (string, error)
	Push(remote string, refs ...string) error
}

// Options configure one release run.
type Options struct {
	// Part is the level to bump: major, minor, or patch.
	Part version.Part

	// Root is the repository working directory.
	Root string

	// Repository is "owner/name", used for the compare link. May be empty.
	Repository string

	// Remote is pushed to. Empty means DefaultRemote.
	Remote string

	// DryRun computes everything and writes nothing.
	DryRun bool

	// SkipPush stops after tagging, leaving the release local.
	SkipPush bool

	// SkipPublish leaves the GitHub release uncreated. CI publishes on the tag
	// push, so this is the default for a local run.
	SkipPublish bool

	// Draft publishes the GitHub release as a draft.
	Draft bool
}

// remote resolves the remote to push to.
func (o Options) remote() string {
	if o.Remote == "" {
		return DefaultRemote
	}
	return o.Remote
}

// Result reports what a run did, or in a dry run what it would have done.
type Result struct {
	Previous         version.Version
	Next             version.Version
	Notes            release.Notes
	Body             string
	ChangelogChanged bool
	Committed        bool
	CommitSHA        string
	Branch           string
	Tagged           bool
	Pushed           bool
	Published        bool
	DryRun           bool
}

// Runner executes releases.
//
// Two note generators, because the two documents differ. `notes` produces the
// Keep a Changelog entry that lands in CHANGELOG.md, inside the tag. `body`
// produces the announcement published to the forge: grouped by what a reader
// cares about, with pull request titles rather than commit subjects.
type Runner struct {
	repo        Repository
	comparisons *release.ComparisonService
	notes       *release.NotesService
	body        *releasenotes.Builder // may be nil, meaning "no release body"
	publisher   Publisher             // may be nil, meaning "never publish"
	clock       release.Clock
	logger      *slog.Logger
}

// NewRunner wires the workflow. A nil publisher skips publication; a nil clock
// uses the system clock; a nil logger discards progress messages.
func NewRunner(
	repo Repository,
	comparisons *release.ComparisonService,
	notes *release.NotesService,
	body *releasenotes.Builder,
	publisher Publisher,
	clock release.Clock,
	logger *slog.Logger,
) *Runner {
	if clock == nil {
		clock = release.SystemClock{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		repo:        repo,
		comparisons: comparisons,
		notes:       notes,
		body:        body,
		publisher:   publisher,
		clock:       clock,
		logger:      logger,
	}
}

// releaseBody renders the announcement published to the forge.
//
// The changelog notes are passed in rather than recomputed: they already carry
// the comparison, and asking the forge for it twice would be a request spent to
// arrive at the same answer.
func (r *Runner) releaseBody(target version.Version, notes release.Notes, options Options, head string) (string, error) {
	if r.body == nil {
		return "", nil
	}

	previous, err := r.comparisons.PreviousRelease(&target)
	if err != nil {
		return "", err
	}
	previousTag := ""
	if previous != nil {
		previousTag = previous.Name
	}

	comparison := git.Comparison{}
	if notes.Comparison != nil {
		comparison = *notes.Comparison
	}

	built, err := r.body.Build(releasenotes.Input{
		Version:     target,
		Date:        notes.Date,
		Repository:  options.Repository,
		PreviousTag: previousTag,
		Head:        head,
		Comparison:  comparison,
		Roadmap:     r.roadmapEntry(options.Root, target),
	})
	if err != nil {
		return "", err
	}
	return releasenotes.Render(built), nil
}

// Preview renders the release body for a version without writing anything.
//
// What `release notes` prints, and what a dry run reports: the announcement
// exactly as it would be published.
func (r *Runner) Preview(target version.Version, options Options, head string) (string, error) {
	if head == "" {
		head = "HEAD"
	}
	notes, err := r.notes.Generate(target, head)
	if err != nil {
		return "", err
	}
	return r.releaseBody(target, notes, options, head)
}

// roadmapEntry finds this version in RELEASES.yaml, where a human wrote its
// summary and highlights. Nil when there is no roadmap, or no entry in it.
func (r *Runner) roadmapEntry(root string, target version.Version) *release.Release {
	file := roadmap.NewFile(root)
	if !file.Exists() {
		return nil
	}
	registry, err := file.Load()
	if err != nil {
		r.logger.Warn("could not read the roadmap; release highlights will be generated", "error", err)
		return nil
	}
	return registry.Find(target)
}

// Run performs a release.
//
// Everything that can fail without side effects is done first: read VERSION,
// compute the next version, refuse an existing tag, establish that there is a
// branch to push, generate the notes. Only then does anything get written.
//
// The write order is the design. VERSION, CHANGELOG.md, and RELEASES.yaml are
// written, then committed, and only then is the tag created — on that commit.
// A tag created before the commit points at a tree that has never heard of the
// version it names: `git show v0.2.0:CHANGELOG.md` would not find the entry
// describing v0.2.0, and CI's tag-matches-VERSION check would read the previous
// version and fail. The push is last, because it is the first irreversible step.
func (r *Runner) Run(options Options) (Result, error) {
	result := Result{DryRun: options.DryRun}

	versionFile := release.NewVersionFile(options.Root)
	current, err := versionFile.Read()
	if err != nil {
		return result, err
	}
	result.Previous = current

	next, err := current.Bump(options.Part)
	if err != nil {
		return result, err
	}
	result.Next = next

	// Refuse before writing anything. A tag that already exists means either a
	// half-finished release or a mistaken bump; both want a human.
	exists, err := r.repo.TagExists(next.Tag())
	if err != nil {
		return result, err
	}
	if exists {
		return result, fmt.Errorf("tag %s already exists; delete it or choose another level", next.Tag())
	}

	// A detached HEAD has no branch to push. Discover that now, rather than
	// after the release has been committed and tagged locally.
	branch, err := r.repo.CurrentBranch()
	if err != nil {
		return result, err
	}
	result.Branch = branch
	if !options.SkipPush && branch == detachedHEAD {
		return result, fmt.Errorf(
			"HEAD is detached, so there is no branch to push; check out a branch, or pass --no-push to release locally",
		)
	}

	// Notes are generated against HEAD, before the release commit and its tag
	// exist. The release commit itself carries no user-facing change, so its
	// absence from the range it describes is correct.
	notes, err := r.notes.Generate(next, "HEAD")
	if err != nil {
		return result, err
	}
	result.Notes = notes

	if result.Body, err = r.releaseBody(next, notes, options, "HEAD"); err != nil {
		return result, err
	}

	if notes.IsEmpty() {
		r.logger.Warn("this release carries no user-facing changes", "version", next.Tag())
	}

	if options.DryRun {
		return result, nil
	}

	// From here on, the working tree changes.

	if err := versionFile.Write(next, false); err != nil {
		return result, err
	}
	r.logger.Info("wrote VERSION", "version", next)

	changed, err := changelog.NewFile(options.Root).Insert(notes)
	if err != nil {
		return result, err
	}
	result.ChangelogChanged = changed
	if changed {
		r.logger.Info("updated CHANGELOG.md", "version", next)
	}

	if err := r.updateRoadmap(options.Root, next, notes.Date); err != nil {
		return result, err
	}

	// One commit, containing exactly the release artefacts. The pathspec keeps a
	// developer's unrelated staged work out of a commit that claims to be a
	// version bump and nothing else.
	sha, err := r.repo.Commit(releaseCommitSubject(next), r.artefacts(options.Root)...)
	if err != nil {
		return result, fmt.Errorf("could not commit the release artefacts: %w", err)
	}
	result.Committed = true
	result.CommitSHA = sha
	r.logger.Info("committed the release", "commit", sha, "version", next)

	// The tag points at the release commit, so a checkout of the tag carries the
	// changelog entry and the VERSION that describe it.
	if _, err := r.repo.CreateTag(next.Tag(), r.tagMessage(next, notes), sha); err != nil {
		return result, fmt.Errorf("committed the release but could not tag it: %w", err)
	}
	result.Tagged = true
	r.logger.Info("created annotated tag", "tag", next.Tag(), "commit", sha)

	if options.SkipPush {
		return result, nil
	}

	// Atomic, so the commit and its tag land together or not at all.
	if err := r.repo.Push(options.remote(), branch, next.Tag()); err != nil {
		return result, fmt.Errorf(
			"committed and tagged %s locally but could not push: %w\nre-run the push when ready:\n  git push --atomic %s %s %s",
			next.Tag(), err, options.remote(), branch, next.Tag(),
		)
	}
	result.Pushed = true
	r.logger.Info("pushed the release", "remote", options.remote(), "branch", branch, "tag", next.Tag())

	if options.SkipPublish || r.publisher == nil {
		return result, nil
	}

	// Publishing is last because it is the only step this tool cannot undo.
	// Normally CI does it, on the tag push that just happened.
	if _, err := r.publisher.UpsertRelease(release.CreateOptions{
		Tag:   next.Tag(),
		Title: r.title(options.Root, next),
		Body:  result.Body,
		Draft: options.Draft,
	}); err != nil {
		return result, fmt.Errorf("pushed %s but could not publish the release: %w", next.Tag(), err)
	}
	result.Published = true
	r.logger.Info("published the GitHub release", "tag", next.Tag())

	return result, nil
}

// releaseCommitSubject names the release commit.
//
// It begins "Release v" deliberately: the commit classifier drops subjects with
// that prefix, so a release commit never appears in the notes of the release
// after it.
func releaseCommitSubject(next version.Version) string {
	return "Release " + next.Tag()
}

// artefacts lists the files a release commit may contain, and nothing else.
// RELEASES.yaml is included only when the project keeps a roadmap.
func (r *Runner) artefacts(root string) []string {
	paths := []string{release.VersionFilename, changelog.Filename}
	if roadmap.NewFile(root).Exists() {
		paths = append(paths, roadmap.Filename)
	}
	return paths
}

// Publish creates or updates the GitHub release for a tag that already exists.
//
// This is the step CI performs on a tag push, and it is deliberately separate
// from Run: the local flow tags and pushes, and the forge learns about the
// release from the tag. Publishing from a developer's machine would announce a
// release whose tag nobody else can see yet.
//
// It is safe to re-run. UpsertRelease updates a release that already exists, so
// a retried workflow does not fail on its own first attempt.
func (r *Runner) Publish(target version.Version, options Options) (Result, error) {
	result := Result{DryRun: options.DryRun, Next: target}

	exists, err := r.repo.TagExists(target.Tag())
	if err != nil {
		return result, err
	}
	if !exists {
		return result, fmt.Errorf("tag %s does not exist; publish runs after the tag is pushed", target.Tag())
	}

	notes, err := r.notes.Generate(target, target.Tag())
	if err != nil {
		return result, err
	}
	result.Notes = notes

	if result.Body, err = r.releaseBody(target, notes, options, target.Tag()); err != nil {
		return result, err
	}

	// A dry run renders the body and stops, so it needs no credentials.
	if options.DryRun {
		return result, nil
	}

	if r.publisher == nil {
		return result, fmt.Errorf("cannot publish %s: no GitHub token was configured", target.Tag())
	}

	if _, err := r.publisher.UpsertRelease(release.CreateOptions{
		Tag:   target.Tag(),
		Title: r.title(options.Root, target),
		Body:  result.Body,
		Draft: options.Draft,
	}); err != nil {
		return result, err
	}
	result.Published = true
	r.logger.Info("published the GitHub release", "tag", target.Tag())

	return result, nil
}

// updateRoadmap records the release in RELEASES.yaml when one exists. A project
// without a roadmap still releases, so an absent file is not an error.
func (r *Runner) updateRoadmap(root string, next version.Version, when time.Time) error {
	file := roadmap.NewFile(root)
	if !file.Exists() {
		return nil
	}
	registry, err := file.Load()
	if err != nil {
		return err
	}
	if err := registry.MarkReleased(next, next.Tag(), when); err != nil {
		return err
	}
	if err := file.Save(registry); err != nil {
		return err
	}
	r.logger.Info("marked the release on the roadmap", "version", next)
	return nil
}

// title prefers the roadmap's title for this version, falling back to the tag.
func (r *Runner) title(root string, next version.Version) string {
	file := roadmap.NewFile(root)
	if !file.Exists() {
		return next.Tag()
	}
	registry, err := file.Load()
	if err != nil {
		return next.Tag()
	}
	if entry := registry.Find(next); entry != nil && entry.Title != "" {
		return fmt.Sprintf("%s — %s", next.Tag(), entry.Title)
	}
	return next.Tag()
}

// tagMessage is the annotated tag's body: the version, and the notes' summary.
func (r *Runner) tagMessage(next version.Version, notes release.Notes) string {
	message := "Release " + next.Tag()
	if notes.Summary != "" {
		message += "\n\n" + notes.Summary
	}
	return message
}

// shortSHA abbreviates a commit for display, tolerating a short or empty one.
func shortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

// Describe writes a human-readable account of a result.
func Describe(w io.Writer, result Result) {
	var b strings.Builder

	if result.DryRun {
		b.WriteString("Dry run — nothing was written.\n\n")
	}
	fmt.Fprintf(&b, "%s -> %s\n", result.Previous, result.Next)

	if summary := result.Notes.Summary; summary != "" {
		fmt.Fprintf(&b, "%s\n", summary)
	}

	sections := result.Notes.PopulatedSections()
	if len(sections) == 0 {
		b.WriteString("\nNo user-facing changes.\n")
	}
	for _, section := range sections {
		fmt.Fprintf(&b, "\n%s\n", section.Category)
		for _, entry := range section.Entries {
			fmt.Fprintf(&b, "  - %s\n", entry)
		}
	}

	if !result.DryRun {
		b.WriteString("\n")
		if result.ChangelogChanged {
			b.WriteString("Updated CHANGELOG.md\n")
		}
		if result.Committed {
			fmt.Fprintf(&b, "Committed the release (%s)\n", shortSHA(result.CommitSHA))
		}
		if result.Tagged {
			fmt.Fprintf(&b, "Created tag %s\n", result.Next.Tag())
		}
		if result.Pushed {
			fmt.Fprintf(&b, "Pushed %s and %s\n", result.Branch, result.Next.Tag())
		}
		if result.Published {
			b.WriteString("Published the GitHub release\n")
		}
		switch {
		case result.Pushed && !result.Published:
			b.WriteString("\nGitHub Actions will publish the release.\n")

		case result.Tagged && !result.Pushed && result.Branch == detachedHEAD:
			// There is no branch to name, so naming one would be bad advice.
			fmt.Fprintf(&b, "\nHEAD is detached. Move a branch onto %s before pushing.\n", shortSHA(result.CommitSHA))

		case result.Tagged && !result.Pushed:
			fmt.Fprintf(&b, "\nPush it when you are ready:\n  git push --atomic %s %s %s\n",
				DefaultRemote, result.Branch, result.Next.Tag())
		}
	}

	io.WriteString(w, b.String())
}
