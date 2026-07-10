package releasenotes_test

import (
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/releasenotes"
)

func TestParsePullRequest(t *testing.T) {
	tests := []struct {
		name       string
		subject    string
		body       string
		wantOK     bool
		wantNumber int
		wantTitle  string
	}{
		{
			name:       "merge commit",
			subject:    "Merge pull request #6 from teddynted/release-management",
			body:       "Add the release management module",
			wantOK:     true,
			wantNumber: 6,
			wantTitle:  "Add the release management module",
		},
		{
			// The title is on the first non-empty body line, which git separates
			// from the subject with a blank one.
			name:       "merge commit with a blank line",
			subject:    "Merge pull request #12 from o/b",
			body:       "\n\nFix the numstat parser\n\nMore prose here.",
			wantOK:     true,
			wantNumber: 12,
			wantTitle:  "Fix the numstat parser",
		},
		{
			name:       "squash commit",
			subject:    "Add the release management module (#6)",
			wantOK:     true,
			wantNumber: 6,
			wantTitle:  "Add the release management module",
		},
		{
			name:       "squash commit with parentheses in the title",
			subject:    "Fix the parser (again) (#42)",
			wantOK:     true,
			wantNumber: 42,
			wantTitle:  "Fix the parser (again)",
		},
		{
			name:       "conventional squash commit",
			subject:    "feat(git): add rename detection (#7)",
			wantOK:     true,
			wantNumber: 7,
			wantTitle:  "feat(git): add rename detection",
		},

		// Not pull requests.
		{name: "plain merge", subject: "Merge branch 'main' into feature", wantOK: false},
		{name: "ordinary commit", subject: "Add the roadmap registry", wantOK: false},
		{name: "issue reference, not a squash", subject: "Fix the thing for #42", wantOK: false},
		{name: "trailing parens without a hash", subject: "Rework routing (finally)", wantOK: false},
		{name: "merge with no title in the body", subject: "Merge pull request #9 from o/b", body: "", wantOK: false},
		{name: "merge with a non-numeric number", subject: "Merge pull request #abc from o/b", body: "T", wantOK: false},
		{name: "squash with a zero number", subject: "Add a thing (#0)", wantOK: false},
		{name: "squash with an empty title", subject: "(#5)", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr, ok := releasenotes.ParsePullRequest(git.Commit{Subject: tc.subject, Body: tc.body})
			if ok != tc.wantOK {
				t.Fatalf("ParsePullRequest(%q) ok = %v, want %v", tc.subject, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if pr.Number != tc.wantNumber {
				t.Errorf("number = %d, want %d", pr.Number, tc.wantNumber)
			}
			if pr.Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", pr.Title, tc.wantTitle)
			}
		})
	}
}
