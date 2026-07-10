// Package git is a thin, testable wrapper around the git command line.
//
// It exposes only the operations the release tool needs. Every method takes a
// context so a slow network operation such as fetch or push can be cancelled.
// The package holds no release policy: it neither knows what a version is nor
// decides which branches may be released from.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Sentinel errors returned by Repo. Callers should match with errors.Is.
var (
	// ErrNotRepository means the directory is not inside a Git work tree.
	ErrNotRepository = errors.New("not a git repository")
	// ErrDetachedHead means HEAD does not point at a branch.
	ErrDetachedHead = errors.New("HEAD is detached")
)

// Field and record separators for the log format. Both are ASCII control
// characters that cannot appear in a commit message, which makes parsing
// unambiguous regardless of what a subject or body contains.
const (
	fieldSep  = "\x1f"
	recordSep = "\x1e"
)

// Commit is a single commit as reported by git log.
type Commit struct {
	SHA         string
	Subject     string
	Body        string
	AuthorName  string
	AuthorEmail string
}

// Short returns the abbreviated commit SHA used in changelog links.
func (c Commit) Short() string {
	if len(c.SHA) > 7 {
		return c.SHA[:7]
	}
	return c.SHA
}

// Runner executes a git subcommand and returns its standard output with the
// trailing newline removed. It exists so tests can drive Repo without a real
// repository on disk.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// CommandRunner runs the real git binary in Dir.
type CommandRunner struct{ Dir string }

// Run implements Runner.
func (r CommandRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	// GIT_TERMINAL_PROMPT stops git blocking on a credential prompt in CI;
	// LC_ALL pins the language of messages we may need to interpret.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return "", &CommandError{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// CommandError reports a git subcommand that exited non-zero. It carries the
// captured standard error, which is usually the only useful diagnostic.
type CommandError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	msg := e.Stderr
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *CommandError) Unwrap() error { return e.Err }

// Repo runs git operations against a single repository.
type Repo struct{ runner Runner }

// New returns a Repo that shells out to git in dir. An empty dir means the
// current working directory.
func New(dir string) *Repo { return &Repo{runner: CommandRunner{Dir: dir}} }

// NewWithRunner returns a Repo backed by a custom Runner, for testing.
func NewWithRunner(r Runner) *Repo { return &Repo{runner: r} }

// EnsureRepository verifies that the directory is inside a Git work tree.
func (r *Repo) EnsureRepository(ctx context.Context) error {
	if _, err := r.runner.Run(ctx, "rev-parse", "--git-dir"); err != nil {
		return ErrNotRepository
	}
	return nil
}

// CurrentBranch returns the checked-out branch name. It returns ErrDetachedHead
// when HEAD points directly at a commit, as it does during a tag-triggered
// GitHub Actions run.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, err := r.runner.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", ErrDetachedHead
	}
	return out, nil
}

// Status returns the porcelain status lines. An empty slice means the working
// tree and index are clean, with no untracked files.
func (r *Repo) Status(ctx context.Context) ([]string, error) {
	out, err := r.runner.Run(ctx, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// FetchTags downloads tags from remote so that version calculation sees tags
// created on other machines.
func (r *Repo) FetchTags(ctx context.Context, remote string) error {
	_, err := r.runner.Run(ctx, "fetch", "--tags", "--quiet", remote)
	return err
}

// Tags lists tag names matching a glob pattern, for example "v*". An empty
// pattern lists every tag.
func (r *Repo) Tags(ctx context.Context, pattern string) ([]string, error) {
	args := []string{"tag", "--list"}
	if pattern != "" {
		args = append(args, pattern)
	}
	out, err := r.runner.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// TagExists reports whether a tag of that name is present locally.
func (r *Repo) TagExists(ctx context.Context, name string) (bool, error) {
	_, err := r.runner.Run(ctx, "rev-parse", "--verify", "--quiet", "refs/tags/"+name)
	if err != nil {
		var cmdErr *CommandError
		// rev-parse --quiet exits 1 with no output when the ref is absent.
		if errors.As(err, &cmdErr) && cmdErr.Stderr == "" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CreateTag creates an annotated tag at HEAD. When sign is true the tag is
// signed with the committer's GPG key instead.
//
// The message is stored verbatim. Git's default cleanup mode strips lines that
// begin with '#', which would silently delete the Markdown headings from the
// release notes embedded in the tag.
func (r *Repo) CreateTag(ctx context.Context, name, message string, sign bool) error {
	kind := "--annotate"
	if sign {
		kind = "--sign"
	}
	_, err := r.runner.Run(ctx, "tag", "--cleanup=verbatim", kind, name, "--message", message)
	return err
}

// PushTag pushes a single tag ref to remote.
func (r *Repo) PushTag(ctx context.Context, remote, name string) error {
	_, err := r.runner.Run(ctx, "push", remote, "refs/tags/"+name)
	return err
}

// HeadSHA returns the full SHA of HEAD.
func (r *Repo) HeadSHA(ctx context.Context) (string, error) {
	return r.runner.Run(ctx, "rev-parse", "HEAD")
}

// CommitDate returns the committer date of a revision.
func (r *Repo) CommitDate(ctx context.Context, rev string) (time.Time, error) {
	out, err := r.runner.Run(ctx, "log", "-1", "--format=%cI", rev)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(out))
}

// RemoteURL returns the fetch URL configured for remote.
func (r *Repo) RemoteURL(ctx context.Context, remote string) (string, error) {
	return r.runner.Run(ctx, "remote", "get-url", remote)
}

// Commits lists the commits reachable from to but not from from, newest first.
// An empty from walks the entire history. Merge commits are excluded: their
// subjects carry no release-note value and their contents are already covered
// by the commits they merge.
func (r *Repo) Commits(ctx context.Context, from, to string) ([]Commit, error) {
	rev := to
	if from != "" {
		rev = from + ".." + to
	}
	format := strings.Join([]string{"%H", "%s", "%b", "%an", "%ae"}, fieldSep) + recordSep
	out, err := r.runner.Run(ctx, "log", "--no-merges", "--pretty=format:"+format, rev)
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}

// parseLog splits the output of the log format used by Commits.
func parseLog(out string) []Commit {
	var commits []Commit
	for record := range strings.SplitSeq(out, recordSep) {
		// git separates records with a newline, which lands at the front of
		// every record after the first.
		record = strings.Trim(record, "\n")
		if record == "" {
			continue
		}
		fields := strings.Split(record, fieldSep)
		if len(fields) != 5 {
			continue
		}
		commits = append(commits, Commit{
			SHA:         strings.TrimSpace(fields[0]),
			Subject:     strings.TrimSpace(fields[1]),
			Body:        strings.TrimSpace(fields[2]),
			AuthorName:  strings.TrimSpace(fields[3]),
			AuthorEmail: strings.TrimSpace(fields[4]),
		})
	}
	return commits
}

// ParseRemoteURL extracts the host, owner, and repository name from a Git
// remote URL. It understands the scp-like syntax (git@host:owner/repo.git) as
// well as https:// and ssh:// URLs.
func ParseRemoteURL(raw string) (host, owner, name string, err error) {
	url := strings.TrimSpace(raw)
	if url == "" {
		return "", "", "", errors.New("empty remote URL")
	}

	switch {
	case strings.HasPrefix(url, "git@"), strings.HasPrefix(url, "ssh://"), strings.HasPrefix(url, "https://"), strings.HasPrefix(url, "http://"):
	default:
		return "", "", "", fmt.Errorf("unsupported remote URL %q", raw)
	}

	// Normalise the scp-like form into something with a single scheme prefix.
	if !strings.Contains(url, "://") {
		url = strings.Replace(url, ":", "/", 1)
	}
	for _, scheme := range []string{"https://", "http://", "ssh://"} {
		url = strings.TrimPrefix(url, scheme)
	}
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSuffix(url, "/")

	// Drop any userinfo such as "git@" or "oauth2:token@".
	if i := strings.LastIndex(url, "@"); i >= 0 {
		url = url[i+1:]
	}

	parts := strings.Split(url, "/")
	if len(parts) < 3 {
		return "", "", "", fmt.Errorf("cannot derive owner and repository from remote URL %q", raw)
	}
	host = parts[0]
	// A host may carry a port, and an owner may be a nested GitLab-style group;
	// the repository is always last and the owner immediately precedes it.
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	owner, name = parts[len(parts)-2], parts[len(parts)-1]
	if host == "" || owner == "" || name == "" {
		return "", "", "", fmt.Errorf("cannot derive owner and repository from remote URL %q", raw)
	}
	return host, owner, name, nil
}
