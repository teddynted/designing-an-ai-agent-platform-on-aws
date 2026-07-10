package git

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubRunner answers a fixed set of git invocations, keyed by the joined
// argument list, and records what it was asked to run.
type stubRunner struct {
	responses map[string]string
	errs      map[string]error
	calls     []string
}

func (s *stubRunner) Run(_ context.Context, args ...string) (string, error) {
	key := strings.Join(args, " ")
	s.calls = append(s.calls, key)
	if err, ok := s.errs[key]; ok {
		return "", err
	}
	out, ok := s.responses[key]
	if !ok {
		return "", &CommandError{Args: args, Stderr: "unexpected call", Err: errors.New("exit status 1")}
	}
	return out, nil
}

func TestParseLog(t *testing.T) {
	// Two commits, the first with a multi-line body containing a colon and a
	// blank line, which a naive line-based parser would mangle.
	raw := strings.Join([]string{
		"abc1234def" + fieldSep + "feat(cli): add --dry-run" + fieldSep +
			"Adds a preview mode.\n\nBREAKING CHANGE: --force is gone" + fieldSep +
			"Ada Lovelace" + fieldSep + "ada@example.com" + recordSep,
		"\n0987654fed" + fieldSep + "fix: correct off-by-one" + fieldSep + "" + fieldSep +
			"Alan Turing" + fieldSep + "alan@example.com" + recordSep,
	}, "")

	commits := parseLog(raw)
	if len(commits) != 2 {
		t.Fatalf("parseLog returned %d commits, want 2", len(commits))
	}

	first := commits[0]
	if first.SHA != "abc1234def" {
		t.Errorf("SHA = %q", first.SHA)
	}
	if first.Subject != "feat(cli): add --dry-run" {
		t.Errorf("Subject = %q", first.Subject)
	}
	if !strings.Contains(first.Body, "BREAKING CHANGE: --force is gone") {
		t.Errorf("Body = %q, want it to retain the footer", first.Body)
	}
	if first.AuthorEmail != "ada@example.com" {
		t.Errorf("AuthorEmail = %q", first.AuthorEmail)
	}
	if first.Short() != "abc1234" {
		t.Errorf("Short() = %q, want abc1234", first.Short())
	}

	if commits[1].Subject != "fix: correct off-by-one" {
		t.Errorf("second Subject = %q", commits[1].Subject)
	}
	if commits[1].Body != "" {
		t.Errorf("second Body = %q, want empty", commits[1].Body)
	}
}

func TestParseLogEmpty(t *testing.T) {
	if got := parseLog(""); len(got) != 0 {
		t.Errorf("parseLog(\"\") = %v, want no commits", got)
	}
}

func TestCommitsBuildsRevisionRange(t *testing.T) {
	stub := &stubRunner{responses: map[string]string{}}
	repo := NewWithRunner(stub)
	ctx := context.Background()

	// The exact log arguments are an implementation detail, so assert on the
	// revision range only: it is the part that changes with from/to.
	stub.responses[logCall("v1.0.0..HEAD")] = ""
	if _, err := repo.Commits(ctx, "v1.0.0", "HEAD"); err != nil {
		t.Fatalf("Commits with a lower bound: %v", err)
	}

	stub.responses[logCall("HEAD")] = ""
	if _, err := repo.Commits(ctx, "", "HEAD"); err != nil {
		t.Fatalf("Commits without a lower bound: %v", err)
	}
}

func logCall(rev string) string {
	format := strings.Join([]string{"%H", "%s", "%b", "%an", "%ae"}, fieldSep) + recordSep
	return strings.Join([]string{"log", "--no-merges", "--pretty=format:" + format, rev}, " ")
}

func TestTagExists(t *testing.T) {
	stub := &stubRunner{
		responses: map[string]string{
			"rev-parse --verify --quiet refs/tags/v1.0.0": "deadbeef",
		},
		errs: map[string]error{
			// A missing ref: --quiet means git exits 1 and prints nothing.
			"rev-parse --verify --quiet refs/tags/v9.9.9": &CommandError{Stderr: "", Err: errors.New("exit status 1")},
			"rev-parse --verify --quiet refs/tags/v8.8.8": &CommandError{Stderr: "fatal: not a git repository", Err: errors.New("exit status 128")},
		},
	}
	repo := NewWithRunner(stub)
	ctx := context.Background()

	if ok, err := repo.TagExists(ctx, "v1.0.0"); err != nil || !ok {
		t.Errorf("TagExists(v1.0.0) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := repo.TagExists(ctx, "v9.9.9"); err != nil || ok {
		t.Errorf("TagExists(v9.9.9) = %v, %v; want false, nil", ok, err)
	}
	// A real failure must not be reported as "tag absent".
	if _, err := repo.TagExists(ctx, "v8.8.8"); err == nil {
		t.Error("TagExists should surface an unexpected git failure")
	}
}

func TestCreateTagSelectsAnnotatedOrSigned(t *testing.T) {
	stub := &stubRunner{responses: map[string]string{
		"tag --cleanup=verbatim --annotate v1.0.0 --message Release v1.0.0": "",
		"tag --cleanup=verbatim --sign v2.0.0 --message Release v2.0.0":     "",
	}}
	repo := NewWithRunner(stub)
	ctx := context.Background()

	if err := repo.CreateTag(ctx, "v1.0.0", "Release v1.0.0", false); err != nil {
		t.Errorf("CreateTag annotated: %v", err)
	}
	if err := repo.CreateTag(ctx, "v2.0.0", "Release v2.0.0", true); err != nil {
		t.Errorf("CreateTag signed: %v", err)
	}
}

// Release notes are Markdown. Git's default cleanup mode treats a leading '#'
// as a comment and would delete every heading from the tag message.
func TestCreateTagKeepsMarkdownHeadings(t *testing.T) {
	stub := &stubRunner{responses: map[string]string{}}
	repo := NewWithRunner(stub)

	// The stub returns an error for an unrecognised call, which is fine here:
	// the assertion is on what was attempted.
	_ = repo.CreateTag(context.Background(), "v1.0.0", "Release v1.0.0\n\n### Features\n\n- a thing\n", false)

	if len(stub.calls) != 1 {
		t.Fatalf("expected one git call, got %v", stub.calls)
	}
	if !strings.Contains(stub.calls[0], "--cleanup=verbatim") {
		t.Errorf("git tag must run with --cleanup=verbatim, got %q", stub.calls[0])
	}
}

func TestStatus(t *testing.T) {
	stub := &stubRunner{responses: map[string]string{"status --porcelain": ""}}
	repo := NewWithRunner(stub)
	if lines, err := repo.Status(context.Background()); err != nil || len(lines) != 0 {
		t.Errorf("Status on a clean tree = %v, %v; want none, nil", lines, err)
	}

	stub.responses["status --porcelain"] = " M go.mod\n?? scratch.txt"
	lines, err := repo.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(lines) != 2 {
		t.Errorf("Status returned %d lines, want 2", len(lines))
	}
}

func TestCurrentBranchDetached(t *testing.T) {
	stub := &stubRunner{errs: map[string]error{
		"symbolic-ref --quiet --short HEAD": &CommandError{Err: errors.New("exit status 1")},
	}}
	repo := NewWithRunner(stub)
	if _, err := repo.CurrentBranch(context.Background()); !errors.Is(err, ErrDetachedHead) {
		t.Errorf("CurrentBranch on a detached HEAD = %v, want ErrDetachedHead", err)
	}
}

func TestParseRemoteURL(t *testing.T) {
	tests := []struct {
		raw                     string
		host, owner, repository string
	}{
		{"git@github.com:teddynted/repo.git", "github.com", "teddynted", "repo"},
		{"git@github.com:teddynted/repo", "github.com", "teddynted", "repo"},
		{"https://github.com/teddynted/repo.git", "github.com", "teddynted", "repo"},
		{"https://github.com/teddynted/repo", "github.com", "teddynted", "repo"},
		{"ssh://git@github.com/teddynted/repo.git", "github.com", "teddynted", "repo"},
		{"ssh://git@github.com:22/teddynted/repo.git", "github.com", "teddynted", "repo"},
		{"https://oauth2:token@git.example.com/team/group/repo.git", "git.example.com", "group", "repo"},
		{"https://github.com/teddynted/repo/", "github.com", "teddynted", "repo"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			host, owner, name, err := ParseRemoteURL(tt.raw)
			if err != nil {
				t.Fatalf("ParseRemoteURL(%q): %v", tt.raw, err)
			}
			if host != tt.host || owner != tt.owner || name != tt.repository {
				t.Errorf("ParseRemoteURL(%q) = %q, %q, %q; want %q, %q, %q",
					tt.raw, host, owner, name, tt.host, tt.owner, tt.repository)
			}
		})
	}

	for _, raw := range []string{"", "not a url", "file:///srv/repo.git", "git@github.com:repo.git"} {
		if _, _, _, err := ParseRemoteURL(raw); err == nil {
			t.Errorf("ParseRemoteURL(%q) succeeded, want error", raw)
		}
	}
}

func TestCommandErrorMessage(t *testing.T) {
	err := &CommandError{Args: []string{"push", "origin"}, Stderr: "permission denied", Err: errors.New("exit status 128")}
	if got, want := err.Error(), "git push origin: permission denied"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, err.Err) {
		t.Error("CommandError should unwrap to the underlying error")
	}
}
