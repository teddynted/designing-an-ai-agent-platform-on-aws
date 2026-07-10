package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// Releases implements release.Host against GitHub Releases.
//
// This is the only type that knows GitHub's JSON shapes. Everything above it
// sees release.Release and git.Comparison. The mapping is deliberately lossy:
// node_id, upload_url, assets, and the author object are dropped, because
// nothing in this system needs them and carrying them would let GitHub's schema
// leak upward.
//
// One asymmetry to know about: a GitHub release's title is its `name` field,
// which is free text and frequently just repeats the tag. Title keeps whatever
// is there rather than trying to be clever, and falls back to the tag when name
// is null, which GitHub permits.
//
// Releases whose tag is not a SemVer release tag are skipped when listing. A
// repository may publish releases against arbitrary tags; they are not ours.
type Releases struct {
	client *Client
}

// NewReleases wraps a client in the release.Host interface.
func NewReleases(client *Client) *Releases { return &Releases{client: client} }

// statusToKind maps GitHub's compare API status strings onto our kinds.
var statusToKind = map[string]git.ChangeKind{
	"added":     git.Added,
	"modified":  git.Modified,
	"changed":   git.Modified,
	"removed":   git.Removed,
	"renamed":   git.Renamed,
	"copied":    git.Added,
	"unchanged": git.Modified,
}

// releasePayload is the subset of GitHub's release object this system reads.
type releasePayload struct {
	ID          int64  `json:"id"`
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
}

type filePayload struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	PreviousFilename string `json:"previous_filename"`
}

// commitPayload has two authors, and they are different things. `commit.author`
// is what git recorded: a name and an email typed into a config file. `author`
// is the GitHub account GitHub matched that email to, and it may be absent —
// for a commit authored by someone with no account, or an unverified address.
// Only the account has a login, and only a login can be @-mentioned.
type commitPayload struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
}

// labelPayload is one label on a pull request.
type labelPayload struct {
	Name string `json:"name"`
}

// pullRequestPayload is the subset of a pull request this system reads.
type pullRequestPayload struct {
	Number int            `json:"number"`
	Title  string         `json:"title"`
	Labels []labelPayload `json:"labels"`
	User   *struct {
		Login string `json:"login"`
	} `json:"user"`
}

type comparePayload struct {
	Files   []filePayload   `json:"files"`
	Commits []commitPayload `json:"commits"`
}

// parseTimestamp reads GitHub's RFC 3339 timestamps. An unparseable value yields
// the zero Time, not an error.
func parseTimestamp(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	when, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return when
}

// toRelease projects GitHub's payload onto the domain.
//
// A draft, or a release with no publication timestamp, is in progress rather
// than released — Release.Validate would otherwise reject a released version
// carrying no date.
func toRelease(payload releasePayload) (release.Release, error) {
	v, err := version.Parse(payload.TagName)
	if err != nil {
		return release.Release{}, fmt.Errorf("release %q does not carry a SemVer tag: %w", payload.TagName, err)
	}

	title := payload.Name
	if title == "" {
		title = payload.TagName
	}

	published := parseTimestamp(payload.PublishedAt)
	status := release.Released
	when := published
	if payload.Draft || published.IsZero() {
		status = release.InProgress
		when = time.Time{}
	}

	return release.Release{
		Version: v,
		Title:   title,
		Status:  status,
		Date:    when,
		Summary: strings.TrimSpace(payload.Body),
	}, nil
}

func toFileChange(payload filePayload) git.FileChange {
	kind, known := statusToKind[payload.Status]
	if !known {
		kind = git.Modified
	}
	previous := payload.PreviousFilename

	// GitHub reports `renamed` for pure renames and for renames with edits.
	// Either way a previous filename is present; without one the domain object
	// would reject the change, so degrade to a modification.
	if kind == git.Renamed && previous == "" {
		kind = git.Modified
	}
	if kind != git.Renamed {
		previous = ""
	}

	return git.FileChange{
		Path:         payload.Filename,
		Kind:         kind,
		Insertions:   payload.Additions,
		Deletions:    payload.Deletions,
		PreviousPath: previous,
	}
}

func toCommit(payload commitPayload) git.Commit {
	subject, body, _ := strings.Cut(payload.Commit.Message, "\n")
	commit := git.Commit{
		SHA:     payload.SHA,
		Subject: strings.TrimSpace(subject),
		Body:    strings.TrimSpace(body),
		Author:  payload.Commit.Author.Name,
		Date:    parseTimestamp(payload.Commit.Author.Date),
	}
	// Absent when GitHub could not match the commit's email to an account.
	if payload.Author != nil {
		commit.Login = payload.Author.Login
	}
	return commit
}

// PullRequest is a merged pull request, as the release notes need it.
type PullRequest struct {
	Number int
	Title  string
	Labels []string
	Login  string // the author's account, without the leading "@"
}

// PullRequest fetches one pull request, for its labels and title.
//
// It returns nil when the pull request does not exist. A release note is not
// worth failing over a number that turned out to be an issue, or a reference to
// a pull request in another repository.
func (r *Releases) PullRequest(number int) (*PullRequest, error) {
	var payload pullRequestPayload
	if err := r.client.get(fmt.Sprintf("pulls/%d", number), nil, &payload); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}

	pr := &PullRequest{Number: payload.Number, Title: strings.TrimSpace(payload.Title)}
	for _, label := range payload.Labels {
		if name := strings.TrimSpace(label.Name); name != "" {
			pr.Labels = append(pr.Labels, name)
		}
	}
	if payload.User != nil {
		pr.Login = payload.User.Login
	}
	return pr, nil
}

// ListReleases returns published and draft releases, newest version first.
func (r *Releases) ListReleases(limit int) ([]release.Release, error) {
	if limit <= 0 {
		limit = MaxPerPage
	}
	var raw []json.RawMessage
	if err := r.client.paginate("releases", limit, &raw); err != nil {
		return nil, err
	}

	var releases []release.Release
	for _, item := range raw {
		var payload releasePayload
		if err := json.Unmarshal(item, &payload); err != nil {
			return nil, &Error{Message: fmt.Sprintf("GET releases: unreadable release object: %v", err)}
		}
		if !git.IsReleaseTag(payload.TagName) {
			continue
		}
		mapped, err := toRelease(payload)
		if err != nil {
			return nil, err
		}
		releases = append(releases, mapped)
	}
	slices.SortFunc(releases, func(a, b release.Release) int { return b.Version.Compare(a.Version) })
	return releases, nil
}

// LatestRelease is GitHub's /releases/latest: the newest non-draft,
// non-prerelease. It returns nil when the repository has none.
func (r *Releases) LatestRelease() (*release.Release, error) {
	var payload releasePayload
	if err := r.client.get("releases/latest", nil, &payload); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	mapped, err := toRelease(payload)
	if err != nil {
		return nil, err
	}
	return &mapped, nil
}

// ReleaseByTag returns nil when no release exists for the tag.
func (r *Releases) ReleaseByTag(tag string) (*release.Release, error) {
	payload, err := r.releasePayloadByTag(tag)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	mapped, err := toRelease(payload)
	if err != nil {
		return nil, err
	}
	return &mapped, nil
}

func (r *Releases) releasePayloadByTag(tag string) (releasePayload, error) {
	var payload releasePayload
	err := r.client.get("releases/tags/"+url.PathEscape(tag), nil, &payload)
	return payload, err
}

// CreateRelease publishes a release for an existing tag.
//
// Prerelease defaults to the version's own pre-release status rather than to
// false, so that pushing v1.0.0-rc.1 cannot silently announce itself as stable.
func (r *Releases) CreateRelease(options release.CreateOptions) (release.Release, error) {
	v, err := version.Parse(options.Tag)
	if err != nil {
		return release.Release{}, fmt.Errorf("cannot create a release for %q: %w", options.Tag, err)
	}

	body := map[string]any{
		"tag_name":   options.Tag,
		"name":       options.Title,
		"body":       options.Body,
		"draft":      options.Draft,
		"prerelease": options.Prerelease || v.IsPrerelease(),
	}
	if options.Target != "" {
		body["target_commitish"] = options.Target
	}

	var payload releasePayload
	if err := r.client.post("releases", body, &payload); err != nil {
		return release.Release{}, err
	}
	return toRelease(payload)
}

// UpdateRelease patches an existing release. Only the fields given are sent.
func (r *Releases) UpdateRelease(tag string, options release.UpdateOptions) (release.Release, error) {
	if options.IsEmpty() {
		return release.Release{}, errors.New("UpdateRelease called with nothing to update")
	}

	existing, err := r.releasePayloadByTag(tag)
	if err != nil {
		return release.Release{}, err
	}
	if existing.ID == 0 {
		return release.Release{}, &Error{Message: fmt.Sprintf("release %s has no usable id", tag)}
	}

	body := map[string]any{}
	if options.Title != nil {
		body["name"] = *options.Title
	}
	if options.Body != nil {
		body["body"] = *options.Body
	}
	if options.Draft != nil {
		body["draft"] = *options.Draft
	}
	if options.Prerelease != nil {
		body["prerelease"] = *options.Prerelease
	}

	var payload releasePayload
	if err := r.client.patch(fmt.Sprintf("releases/%d", existing.ID), body, &payload); err != nil {
		return release.Release{}, err
	}
	return toRelease(payload)
}

// DeleteRelease deletes the release. The underlying git tag survives.
func (r *Releases) DeleteRelease(tag string) error {
	existing, err := r.releasePayloadByTag(tag)
	if err != nil {
		return err
	}
	if existing.ID == 0 {
		return &Error{Message: fmt.Sprintf("release %s has no usable id", tag)}
	}
	return r.client.delete(fmt.Sprintf("releases/%d", existing.ID))
}

// UpsertRelease creates the release, or updates it if it already exists.
//
// Re-running a release workflow — after a transient failure, say — must not fail
// because the release was created on the first attempt.
func (r *Releases) UpsertRelease(options release.CreateOptions) (release.Release, error) {
	existing, err := r.ReleaseByTag(options.Tag)
	if err != nil {
		return release.Release{}, err
	}
	if existing == nil {
		return r.CreateRelease(options)
	}
	return r.UpdateRelease(options.Tag, release.UpdateOptions{
		Title: &options.Title,
		Body:  &options.Body,
	})
}

// PullRequestLabels returns the labels on a pull request, or nothing when it
// does not exist. It satisfies the release notes' label source.
func (r *Releases) PullRequestLabels(number int) ([]string, error) {
	pr, err := r.PullRequest(number)
	if err != nil || pr == nil {
		return nil, err
	}
	return pr.Labels, nil
}

// Compare performs GitHub's server-side comparison, which diffs from the merge
// base of the two refs.
func (r *Releases) Compare(base, head string) (git.Comparison, error) {
	var payload comparePayload
	path := "compare/" + url.PathEscape(base) + "..." + url.PathEscape(head)
	if err := r.client.get(path, nil, &payload); err != nil {
		return git.Comparison{}, err
	}

	headVersion, err := version.Parse(head)
	if err != nil {
		return git.Comparison{}, fmt.Errorf("compare head %q is not a version: %w", head, err)
	}
	baseVersion, err := version.Parse(base)
	if err != nil {
		return git.Comparison{}, fmt.Errorf("compare base %q is not a version: %w", base, err)
	}

	files := make([]git.FileChange, 0, len(payload.Files))
	for _, f := range payload.Files {
		change := toFileChange(f)
		if err := change.Validate(); err != nil {
			return git.Comparison{}, err
		}
		files = append(files, change)
	}
	slices.SortFunc(files, func(a, b git.FileChange) int { return strings.Compare(a.Path, b.Path) })

	// GitHub returns commits oldest-first; the rest of this system reads
	// newest-first, matching `git log`.
	commits := make([]git.Commit, 0, len(payload.Commits))
	for i := len(payload.Commits) - 1; i >= 0; i-- {
		commits = append(commits, toCommit(payload.Commits[i]))
	}

	return git.Comparison{
		Head:    headVersion,
		Base:    &baseVersion,
		Commits: commits,
		Files:   files,
	}, nil
}
