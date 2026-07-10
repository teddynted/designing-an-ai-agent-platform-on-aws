package git

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// EmptyTree is `git hash-object -t tree /dev/null`, identical in every
// repository. Diffing against it yields the first release, which has no
// predecessor.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Field and record separators for the machine-readable log format. Chosen from
// the ASCII control block because they cannot appear in a commit subject.
const (
	unitSep   = "\x1f"
	recordSep = "\x1e"
)

var logFormat = strings.Join([]string{"%H", "%s", "%b", "%an", "%aI"}, unitSep) + recordSep

// statusToKind maps the letter git prints in --name-status output.
var statusToKind = map[byte]ChangeKind{
	'A': Added,
	'M': Modified,
	'D': Removed,
	'R': Renamed,
	'C': Added,    // a copy creates a new path; the source is untouched
	'T': Modified, // type change, e.g. file becomes a symlink
}

// Runner performs one git invocation. It is the only thing that touches the
// filesystem, which is what makes Repo testable without a repository.
type Runner interface {
	Run(args ...string) (string, error)
}

// ExecRunner runs the real git binary.
//
// Everything is asked for in machine-readable form: --format with explicit
// separators, -z for paths. Never the porcelain a human reads. Paths may contain
// spaces, quotes, and newlines; git's default output quotes such paths ("a\nb"),
// and un-quoting them correctly is a parser nobody should write twice. -z
// sidesteps it by emitting raw bytes with NUL terminators.
type ExecRunner struct {
	Dir    string // repository working directory; "" means the process's own
	Binary string // git executable; "" means "git" on PATH
}

// Run executes git with args and returns its stdout.
func (e ExecRunner) Run(args ...string) (string, error) {
	binary := e.Binary
	if binary == "" {
		binary = "git"
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = e.Dir

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var notFound *exec.Error
		if errors.As(err, &notFound) {
			return "", fmt.Errorf("%s is not on PATH: %w", binary, err)
		}
		message := strings.TrimSpace(stderr.String())
		return "", fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, message)
	}
	return stdout.String(), nil
}

// Repo reads and writes one local repository's tags and history.
type Repo struct {
	runner Runner
}

// NewRepo opens the repository rooted at dir, using the git binary on PATH.
func NewRepo(dir string) *Repo {
	return &Repo{runner: ExecRunner{Dir: dir}}
}

// NewRepoWithRunner injects a Runner, for tests and for callers that need a
// different git binary.
func NewRepoWithRunner(runner Runner) *Repo {
	return &Repo{runner: runner}
}

// parseTime reads git's strict ISO 8601 output. An unparseable or empty
// timestamp yields the zero Time rather than an error: a missing date is not a
// reason to fail a release.
func parseTime(text string) time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	when, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}
	}
	return when
}

// ListTags returns every release tag, ascending by SemVer precedence.
//
// Tags that are not releases are skipped rather than raising: a repository's tag
// namespace belongs to its humans too.
func (r *Repo) ListTags() ([]Tag, error) {
	out, err := r.runner.Run(
		"for-each-ref",
		"--format=%(refname:strip=2)\t%(objectname)\t%(creatordate:iso-strict)",
		"refs/tags",
	)
	if err != nil {
		return nil, err
	}

	var tags []Tag
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		name := fields[0]
		if !IsReleaseTag(name) {
			continue
		}
		v, err := version.Parse(name)
		if err != nil {
			continue // IsReleaseTag already vouched for it; belt and braces
		}
		tag := Tag{Name: name, Version: v}
		if len(fields) > 1 {
			tag.SHA = fields[1]
		}
		if len(fields) > 2 {
			tag.Date = parseTime(fields[2])
		}
		tags = append(tags, tag)
	}
	slices.SortFunc(tags, func(a, b Tag) int { return a.Version.Compare(b.Version) })
	return tags, nil
}

// LatestTag is the highest release tag, or nil in a repository with none.
func (r *Repo) LatestTag() (*Tag, error) {
	tags, err := r.ListTags()
	if err != nil || len(tags) == 0 {
		return nil, err
	}
	return &tags[len(tags)-1], nil
}

// CurrentTag is the highest release tag pointing at HEAD, if HEAD is tagged.
func (r *Repo) CurrentTag() (*Tag, error) {
	out, err := r.runner.Run("tag", "--points-at", "HEAD")
	if err != nil {
		return nil, err
	}
	var best *Tag
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if !IsReleaseTag(name) {
			continue
		}
		tag, err := ParseTag(name)
		if err != nil {
			continue
		}
		if best == nil || best.Version.Less(tag.Version) {
			candidate := tag
			best = &candidate
		}
	}
	return best, nil
}

// PreviousTag is the highest release tag strictly below target.
//
// Defined by precedence rather than by position in the tag list, so it is
// correct even when target has not been created yet — which is exactly the case
// when generating notes for a release before pushing it. A nil target means "the
// tag below the latest".
func (r *Repo) PreviousTag(target *version.Version) (*Tag, error) {
	tags, err := r.ListTags()
	if err != nil || len(tags) == 0 {
		return nil, err
	}
	ceiling := target
	if ceiling == nil {
		ceiling = &tags[len(tags)-1].Version
	}
	for i := len(tags) - 1; i >= 0; i-- {
		if tags[i].Version.Less(*ceiling) {
			return &tags[i], nil
		}
	}
	return nil, nil
}

// TagExists reports whether a tag of that exact name is present.
func (r *Repo) TagExists(name string) (bool, error) {
	out, err := r.runner.Run("tag", "--list", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// CreateTag creates an annotated tag at ref.
//
// Annotated, never lightweight: a release is an object with an author and a
// date, not a moving pointer. Names that are not release tags are refused.
func (r *Repo) CreateTag(name, message, ref string) (Tag, error) {
	if !IsReleaseTag(name) {
		return Tag{}, fmt.Errorf("refusing to create %q: release tags look like v1.2.3", name)
	}
	if ref == "" {
		ref = "HEAD"
	}
	if _, err := r.runner.Run("tag", "-a", name, "-m", message, ref); err != nil {
		return Tag{}, err
	}
	tag, err := ParseTag(name)
	if err != nil {
		return Tag{}, err
	}
	sha, err := r.runner.Run("rev-list", "-n", "1", name)
	if err != nil {
		return Tag{}, err
	}
	tag.SHA = strings.TrimSpace(sha)
	return tag, nil
}

// CurrentBranch is the branch HEAD is on, or "HEAD" when detached.
//
// A detached HEAD has no branch to push, which is why the caller is told rather
// than guessed at.
func (r *Repo) CurrentBranch() (string, error) {
	out, err := r.runner.Run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Commit stages the given paths and commits exactly those, returning the new
// commit's SHA.
//
// The pathspec on `git commit` is what confines a release commit to the release
// artefacts: a developer's unrelated staged changes stay in the index rather
// than being swept into a commit that claims to be nothing but a version bump.
//
// Paths are staged first because `git commit -- <path>` refuses a path git has
// never seen, and CHANGELOG.md is untracked before the first release.
func (r *Repo) Commit(message string, paths ...string) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("refusing to commit without a pathspec")
	}
	if strings.TrimSpace(message) == "" {
		return "", errors.New("refusing to commit without a message")
	}

	if _, err := r.runner.Run(append([]string{"add", "--"}, paths...)...); err != nil {
		return "", err
	}
	if _, err := r.runner.Run(append([]string{"commit", "-m", message, "--"}, paths...)...); err != nil {
		return "", err
	}

	sha, err := r.runner.Run("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

// Push sends refs to a remote.
//
// --atomic so that the release commit and its tag land together or not at all.
// Half a release on the remote — a tag with no commit, or a commit with no tag —
// is a state no part of this system knows how to recover from.
func (r *Repo) Push(remote string, refs ...string) error {
	if remote == "" {
		return errors.New("refusing to push without a remote")
	}
	if len(refs) == 0 {
		return errors.New("refusing to push without a ref")
	}
	_, err := r.runner.Run(append([]string{"push", "--atomic", remote}, refs...)...)
	return err
}

// CommitsBetween returns commits reachable from head but not base, newest first.
//
// Two dots, not three: `base..head` is the ordinary "what is new" question.
// Three dots on a log means the symmetric difference, which is emphatically not
// that. An empty base means "every commit reachable from head".
func (r *Repo) CommitsBetween(base, head string) ([]Commit, error) {
	revision := head
	if base != "" {
		revision = base + ".." + head
	}
	out, err := r.runner.Run("log", "--format="+logFormat, revision)
	if err != nil {
		return nil, err
	}
	return parseLog(out)
}

func parseLog(out string) ([]Commit, error) {
	var commits []Commit
	for _, record := range strings.Split(out, recordSep) {
		record = strings.Trim(record, "\n")
		if strings.TrimSpace(record) == "" {
			continue
		}
		fields := strings.Split(record, unitSep)
		if len(fields) < 5 {
			return nil, fmt.Errorf("unparseable git log record: %q", record)
		}
		commits = append(commits, Commit{
			SHA:     strings.TrimSpace(fields[0]),
			Subject: strings.TrimSpace(fields[1]),
			Body:    strings.TrimSpace(fields[2]),
			Author:  strings.TrimSpace(fields[3]),
			Date:    parseTime(fields[4]),
		})
	}
	return commits, nil
}

// nameStatusEntry is the kind and origin of one path, before line counts join it.
type nameStatusEntry struct {
	kind         ChangeKind
	previousPath string
}

// splitNUL splits -z output into fields.
//
// NUL is a terminator, not a separator, so the final one leaves a trailing empty
// element. Dropping it matters: it otherwise inflates the field count and lets a
// truncated record read past its own end into an empty string.
func splitNUL(raw string) []string {
	raw = strings.TrimSuffix(raw, "\x00")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\x00")
}

// parseNameStatus reads `git diff --name-status -M -z`.
//
// Records are NUL-terminated and of variable arity: a rename or copy emits three
// fields (status, old, new); everything else emits two.
func parseNameStatus(raw string) (map[string]nameStatusEntry, error) {
	fields := splitNUL(raw)
	result := map[string]nameStatusEntry{}

	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			i++
			continue
		}
		letter := status[0]
		if letter == 'R' || letter == 'C' {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("truncated rename record in diff: %q", status)
			}
			old, current := fields[i+1], fields[i+2]
			if old == "" || current == "" {
				return nil, fmt.Errorf("rename record with an empty path: %q", status)
			}
			kind := statusToKind[letter]
			entry := nameStatusEntry{kind: kind}
			if kind == Renamed {
				entry.previousPath = old
			}
			result[current] = entry
			i += 3
			continue
		}
		if i+1 >= len(fields) {
			return nil, fmt.Errorf("truncated record in diff: %q", status)
		}
		if fields[i+1] == "" {
			return nil, fmt.Errorf("diff record with an empty path: %q", status)
		}
		kind, ok := statusToKind[letter]
		if !ok {
			kind = Modified
		}
		result[fields[i+1]] = nameStatusEntry{kind: kind}
		i += 2
	}
	return result, nil
}

// lineCounts is the insertions and deletions for one path.
type lineCounts struct {
	insertions int
	deletions  int
	binary     bool
}

// parseNumstat reads `git diff --numstat -M -z`.
//
// A rename emits "add\tdel\t" with an empty third field, then the old and new
// paths as two further NUL-terminated fields. A binary file emits "-" for both
// counts, which becomes binary — not zero.
func parseNumstat(raw string) (map[string]lineCounts, error) {
	fields := splitNUL(raw)
	result := map[string]lineCounts{}

	for i := 0; i < len(fields); {
		record := fields[i]
		if strings.TrimSpace(record) == "" {
			i++
			continue
		}
		parts := strings.SplitN(record, "\t", 3)
		if len(parts) < 3 {
			return nil, fmt.Errorf("unparseable numstat record: %q", record)
		}
		counts, err := parseCounts(parts[0], parts[1])
		if err != nil {
			return nil, fmt.Errorf("unparseable numstat record %q: %w", record, err)
		}

		if parts[2] == "" {
			// Rename: the two paths follow as separate NUL-terminated fields.
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("truncated numstat rename record: %q", record)
			}
			result[fields[i+2]] = counts
			i += 3
			continue
		}
		result[parts[2]] = counts
		i++
	}
	return result, nil
}

func parseCounts(added, removed string) (lineCounts, error) {
	if added == "-" && removed == "-" {
		return lineCounts{binary: true}, nil
	}
	insertions, err := strconv.Atoi(added)
	if err != nil {
		return lineCounts{}, fmt.Errorf("bad insertion count %q", added)
	}
	deletions, err := strconv.Atoi(removed)
	if err != nil {
		return lineCounts{}, fmt.Errorf("bad deletion count %q", removed)
	}
	return lineCounts{insertions: insertions, deletions: deletions}, nil
}

// diffFiles assembles the per-path changes between two refs.
func (r *Repo) diffFiles(base, head string) ([]FileChange, error) {
	// Three dots against a real base (merge-base semantics, as GitHub's Compare
	// API does); the empty tree has no merge base with anything, so two dots.
	spec := EmptyTree + ".." + head
	if base != "" {
		spec = base + "..." + head
	}

	rawStatus, err := r.runner.Run("diff", "--name-status", "-M", "-z", spec)
	if err != nil {
		return nil, err
	}
	kinds, err := parseNameStatus(rawStatus)
	if err != nil {
		return nil, err
	}

	rawNumstat, err := r.runner.Run("diff", "--numstat", "-M", "-z", spec)
	if err != nil {
		return nil, err
	}
	counts, err := parseNumstat(rawNumstat)
	if err != nil {
		return nil, err
	}

	changes := make([]FileChange, 0, len(kinds))
	for path, entry := range kinds {
		count := counts[path] // a path absent from numstat reports zero changes
		change := FileChange{
			Path:         path,
			Kind:         entry.kind,
			Insertions:   count.insertions,
			Deletions:    count.deletions,
			Binary:       count.binary,
			PreviousPath: entry.previousPath,
		}
		if err := change.Validate(); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	slices.SortFunc(changes, func(a, b FileChange) int { return strings.Compare(a.Path, b.Path) })
	return changes, nil
}

// Compare diffs two refs into a Comparison. An empty base compares against the
// empty tree, which is the first release.
//
// headVersion names the release when head is a ref such as HEAD that does not
// parse as a version — the case when generating notes for a tag not yet created.
func (r *Repo) Compare(base, head string, headVersion *version.Version) (Comparison, error) {
	comparison := Comparison{}

	if headVersion != nil {
		comparison.Head = *headVersion
	} else {
		parsed, err := version.Parse(head)
		if err != nil {
			return Comparison{}, fmt.Errorf("head %q is not a version and no head version was given: %w", head, err)
		}
		comparison.Head = parsed
	}

	if base != "" {
		parsed, err := version.Parse(base)
		if err != nil {
			return Comparison{}, fmt.Errorf("base %q is not a version: %w", base, err)
		}
		comparison.Base = &parsed
	}

	commits, err := r.CommitsBetween(base, head)
	if err != nil {
		return Comparison{}, err
	}
	comparison.Commits = commits

	files, err := r.diffFiles(base, head)
	if err != nil {
		return Comparison{}, err
	}
	comparison.Files = files

	return comparison, nil
}
