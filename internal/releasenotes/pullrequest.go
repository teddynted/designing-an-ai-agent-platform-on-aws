package releasenotes

import (
	"strconv"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
)

// Recovering pull requests from commits, without the API.
//
// A merged pull request leaves a commit whose shape depends on how it was
// merged, and both shapes carry the number and the human-written title:
//
//	merge commit    subject: Merge pull request #6 from teddynted/branch
//	                body:    Add the release management module
//
//	squash commit   subject: Add the release management module (#6)
//
// A rebase merge leaves no trace at all, and its commits appear individually.
// That is not a failure — the commits are still there, and their subjects were
// still written by a person.
//
// The title matters more than the number. It is the one line an author wrote
// knowing it would be read by users, which is why it is preferred over the
// commit subjects beneath it.

// PullRequest is a merged pull request as recovered from a commit.
type PullRequest struct {
	Number int
	Title  string
}

// mergeSubject matches the commit GitHub writes for a merge-commit merge.
const mergePrefix = "Merge pull request #"

// squashSuffix matches the "(#42)" GitHub appends on a squash merge.
func squashNumber(subject string) (title string, number int, ok bool) {
	subject = strings.TrimSpace(subject)
	if !strings.HasSuffix(subject, ")") {
		return "", 0, false
	}
	open := strings.LastIndex(subject, "(#")
	if open <= 0 {
		return "", 0, false
	}
	digits := subject[open+2 : len(subject)-1]
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return strings.TrimSpace(subject[:open]), n, true
}

// mergeNumber reads the number out of "Merge pull request #6 from owner/branch".
func mergeNumber(subject string) (int, bool) {
	if !strings.HasPrefix(subject, mergePrefix) {
		return 0, false
	}
	rest := subject[len(mergePrefix):]
	digits, _, _ := strings.Cut(rest, " ")
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// firstLine is the first non-empty line of a commit body, which for a merge
// commit is the pull request's title.
func firstLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ParsePullRequest recovers the pull request a commit represents, if it is one.
//
// A merge commit whose body carries no title falls back to the branch-shaped
// subject rather than reporting a pull request with no name.
func ParsePullRequest(commit git.Commit) (PullRequest, bool) {
	subject := strings.TrimSpace(commit.Subject)

	if number, ok := mergeNumber(subject); ok {
		title := firstLine(commit.Body)
		if title == "" {
			return PullRequest{}, false
		}
		return PullRequest{Number: number, Title: title}, true
	}

	if title, number, ok := squashNumber(subject); ok && title != "" {
		return PullRequest{Number: number, Title: title}, true
	}

	return PullRequest{}, false
}
