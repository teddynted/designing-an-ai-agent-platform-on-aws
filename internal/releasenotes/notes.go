package releasenotes

import (
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// Repository is the local history the notes are assembled from.
//
// Narrower than release.Repository because that is all this needs, and a fake
// implements three methods rather than ten.
type Repository interface {
	// CommitsBetween returns commits reachable from head but not base, newest
	// first, with their parents.
	CommitsBetween(base, head string) ([]git.Commit, error)

	// RevList returns the SHAs reachable from head but not base.
	RevList(base, head string) ([]string, error)

	// FilesChanged lists the paths touched between two refs.
	FilesChanged(base, head string) ([]string, error)
}

// PullRequestSource supplies the labels a project put on a pull request.
//
// Optional. Without it — no token, or a pull request in another repository —
// classification falls back to commit metadata and file paths, which is what
// happens on a developer's laptop.
type PullRequestSource interface {
	PullRequestLabels(number int) ([]string, error)
}

// Entry is one line in the release body.
//
// Title is written by a person: a pull request title, or a commit subject.
// Nothing here rewrites it.
type Entry struct {
	Title    string
	Number   int // pull request number, 0 when the change arrived as a commit
	SHA      string
	Breaking bool
}

// Contributor is one person credited in the release.
type Contributor struct {
	Name  string // as git recorded it
	Login string // the forge account, when known
}

// Mention renders the contributor as the release body should show them.
func (c Contributor) Mention() string {
	if c.Login != "" {
		return "@" + c.Login
	}
	return c.Name
}

// Statistics are the release's measurements.
type Statistics struct {
	Commits      int
	FilesChanged int
	Insertions   int
	Deletions    int
	Contributors int
}

// Notes is a release body as structured data. Rendering lives in render.go.
type Notes struct {
	Version      version.Version
	Date         time.Time
	Repository   string // owner/name; empty disables the compare link
	PreviousTag  string // empty for the first release
	Summary      string
	Highlights   []string
	Sections     map[Section][]Entry
	Contributors []Contributor
	Statistics   Statistics
	Commits      []git.Commit // the raw list, shown only when expanded
}

// Entries returns the entries under a section, in the order they were built.
func (n Notes) Entries(section Section) []Entry { return n.Sections[section] }

// PopulatedSections returns the non-empty sections, in Order. Empty sections are
// omitted from the rendered body entirely.
func (n Notes) PopulatedSections() []Section {
	var sections []Section
	for _, section := range Order {
		if len(n.Sections[section]) > 0 {
			sections = append(sections, section)
		}
	}
	return sections
}

// IsEmpty reports whether no section carries an entry.
func (n Notes) IsEmpty() bool { return len(n.PopulatedSections()) == 0 }

// IsInitial reports whether this is the first release, which has no predecessor
// to compare against.
func (n Notes) IsInitial() bool { return n.PreviousTag == "" }

// Input is everything Build needs that it cannot discover for itself.
type Input struct {
	Version     version.Version
	Date        time.Time
	Repository  string // owner/name
	PreviousTag string // "" for the first release
	Head        string // the ref being released

	// Comparison supplies the file and line statistics, and — when it came from
	// the forge — the account logins that a git commit does not carry.
	Comparison git.Comparison

	// Roadmap is the RELEASES.yaml entry for this version, and the only source
	// of the Highlights prose. Nil when the project keeps no roadmap.
	Roadmap *release.Release
}

// Builder assembles release notes.
type Builder struct {
	repo   Repository
	labels PullRequestSource // may be nil
	logger *slog.Logger
}

// NewBuilder wires the builder. A nil label source disables label-driven
// classification; a nil logger discards the warnings.
func NewBuilder(repo Repository, labels PullRequestSource, logger *slog.Logger) *Builder {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Builder{repo: repo, labels: labels, logger: logger}
}

// releaseCommitPrefixes are the release mechanics themselves, which never belong
// in the notes they generate.
//
// Deliberately shorter than the changelog's housekeeping list: `chore`, `ci`,
// and `test` commits are dropped from CHANGELOG.md but kept here, under
// Internal. A changelog is a ledger of user-facing change; a release body also
// answers "what did the maintainers do", and hiding it entirely is its own kind
// of dishonesty.
var releaseCommitPrefixes = []string{"release v", "bump version", "prepare release"}

func isReleaseCommit(subject string) bool {
	lowered := strings.ToLower(strings.TrimSpace(subject))
	for _, prefix := range releaseCommitPrefixes {
		if strings.HasPrefix(lowered, prefix) {
			return true
		}
	}
	return false
}

// Build assembles the notes for one release.
func (b *Builder) Build(input Input) (Notes, error) {
	notes := Notes{
		Version:     input.Version,
		Date:        input.Date,
		Repository:  input.Repository,
		PreviousTag: input.PreviousTag,
		Sections:    map[Section][]Entry{},
	}

	head := input.Head
	if head == "" {
		head = input.Version.Tag()
	}

	commits, err := b.repo.CommitsBetween(input.PreviousTag, head)
	if err != nil {
		return Notes{}, err
	}
	notes.Commits = commits

	// Commits brought in by a merged pull request are represented by the pull
	// request, not listed individually beneath it.
	absorbed, err := b.absorbedByPullRequests(commits)
	if err != nil {
		return Notes{}, err
	}

	labelCache := map[int][]string{}

	for _, commit := range commits {
		if absorbed[commit.SHA] || isReleaseCommit(commit.Subject) {
			continue
		}

		pr, isPR := ParsePullRequest(commit)

		// A merge that is not a pull request — a local `Merge branch` — is
		// noise. The commits it brought in are not absorbed, so they appear on
		// their own and nothing is lost by dropping the merge itself.
		if !isPR && commit.IsMergeCommit() {
			continue
		}

		// Classification reads the pull request's title, not the merge commit's
		// subject: "Merge pull request #6 from teddynted/branch" says nothing
		// about what changed, and the title says everything.
		effective := commit
		if isPR {
			effective.Subject = pr.Title
		}

		paths, err := b.repo.FilesChanged(commit.FirstParent(), commit.SHA)
		if err != nil {
			// Path evidence is a refinement, not a requirement. A release must
			// not fail because one commit's diff could not be read.
			b.logger.Warn("could not read the files a commit touched", "sha", commit.ShortSHA(), "error", err)
			paths = nil
		}

		var labels []string
		if isPR {
			labels = b.labelsFor(pr.Number, labelCache)
		}

		entry := Entry{
			Title:    effective.Subject,
			SHA:      commit.SHA,
			Breaking: effective.IsBreaking() || hasLabel(labels, "breaking-change", "breaking"),
		}
		if isPR {
			entry.Number = pr.Number
		}

		section := SectionOf(effective, labels, paths)
		notes.Sections[section] = append(notes.Sections[section], entry)
	}

	notes.Contributors = contributors(commits, input.Comparison)
	notes.Statistics = Statistics{
		Commits:      countReal(commits),
		FilesChanged: input.Comparison.FileCount(),
		Insertions:   input.Comparison.Insertions(),
		Deletions:    input.Comparison.Deletions(),
		Contributors: len(notes.Contributors),
	}

	notes.Summary, notes.Highlights = highlights(input, notes.Statistics)

	// A release cut without a roadmap entry still publishes, leading with counts
	// rather than prose. That is the honest fallback, not a good release note:
	// say so, so the summary can be written before anyone reads the tag.
	if !hasSummary(input.Roadmap) {
		b.logger.Warn("no summary in RELEASES.yaml; the release will lead with commit counts rather than prose",
			"version", input.Version.Tag())
	}

	return notes, nil
}

// hasSummary reports whether a human wrote a summary for this release.
func hasSummary(roadmap *release.Release) bool {
	return roadmap != nil && strings.TrimSpace(roadmap.Summary) != ""
}

// absorbedByPullRequests collects the SHAs a merged pull request brought in, so
// that the pull request stands for them.
//
// A squash merge brings in nothing — its single commit is the pull request — so
// only merge commits contribute. The second parent is the branch tip; everything
// reachable from it but not from the first parent came with the pull request.
func (b *Builder) absorbedByPullRequests(commits []git.Commit) (map[string]bool, error) {
	absorbed := map[string]bool{}

	for _, commit := range commits {
		if !commit.IsMergeCommit() || len(commit.Parents) < 2 {
			continue
		}
		if _, ok := ParsePullRequest(commit); !ok {
			continue
		}
		shas, err := b.repo.RevList(commit.Parents[0], commit.Parents[1])
		if err != nil {
			return nil, err
		}
		for _, sha := range shas {
			absorbed[sha] = true
		}
	}
	return absorbed, nil
}

// labelsFor fetches a pull request's labels, tolerating a source that cannot
// answer. A rate limit must degrade the classification, not fail the release.
func (b *Builder) labelsFor(number int, cache map[int][]string) []string {
	if b.labels == nil || number <= 0 {
		return nil
	}
	if cached, ok := cache[number]; ok {
		return cached
	}
	labels, err := b.labels.PullRequestLabels(number)
	if err != nil {
		b.logger.Warn("could not read pull request labels; classifying from commit metadata instead",
			"pull_request", number, "error", err)
		labels = nil
	}
	cache[number] = labels
	return labels
}

// countReal counts the commits a reader would recognise as work: everything but
// the release commit the tooling wrote itself.
func countReal(commits []git.Commit) int {
	count := 0
	for _, commit := range commits {
		if !isReleaseCommit(commit.Subject) {
			count++
		}
	}
	return count
}

// contributors credits everyone who authored a commit in the range.
//
// Logins come from the forge's comparison, which is the only place they exist —
// git records a name and an email address, not an account. When the comparison
// came from local git there are no logins, and names are shown instead.
func contributors(commits []git.Commit, comparison git.Comparison) []Contributor {
	logins := map[string]string{}
	for _, commit := range comparison.Commits {
		if commit.Login != "" {
			logins[commit.SHA] = commit.Login
		}
	}

	seen := map[string]Contributor{}
	for _, commit := range commits {
		if isReleaseCommit(commit.Subject) || commit.Author == "" {
			continue
		}
		login := logins[commit.SHA]

		// Key on the login when there is one, so the same person committing
		// under two names is credited once.
		key := login
		if key == "" {
			key = commit.Author
		}
		if existing, ok := seen[key]; ok && existing.Login != "" {
			continue
		}
		seen[key] = Contributor{Name: commit.Author, Login: login}
	}

	out := make([]Contributor, 0, len(seen))
	for _, contributor := range seen {
		out = append(out, contributor)
	}
	slices.SortFunc(out, func(a, b Contributor) int {
		return strings.Compare(strings.ToLower(a.Mention()), strings.ToLower(b.Mention()))
	})
	return out
}

// highlights returns the summary paragraph and the highlight bullets.
//
// Both come from RELEASES.yaml, where a human wrote them. When the roadmap has
// nothing to say, the fallback states measurements rather than inventing a
// narrative: a sentence of counts is honest, and an invented one is not.
func highlights(input Input, stats Statistics) (summary string, bullets []string) {
	if input.Roadmap != nil {
		bullets = input.Roadmap.Highlights
	}
	if !hasSummary(input.Roadmap) {
		return fallbackSummary(input, stats), bullets
	}
	return strings.TrimSpace(input.Roadmap.Summary), bullets
}

func fallbackSummary(input Input, stats Statistics) string {
	var b strings.Builder
	if input.Comparison.IsInitial() {
		b.WriteString("The first release. ")
	}
	b.WriteString(plural(stats.Commits, "commit", "commits"))
	b.WriteString(" from ")
	b.WriteString(plural(stats.Contributors, "contributor", "contributors"))
	b.WriteString(", across ")
	b.WriteString(plural(stats.FilesChanged, "file", "files"))
	b.WriteString(".")
	return b.String()
}
