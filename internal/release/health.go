package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
)

// Level is the severity of a health check result.
type Level int

const (
	// LevelOK means the check passed.
	LevelOK Level = iota
	// LevelWarn means the release can proceed, but something is worth knowing.
	LevelWarn
	// LevelFail means the release must not proceed.
	LevelFail
)

func (l Level) String() string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelWarn:
		return "warning"
	default:
		return "failure"
	}
}

// Check is one line of the repository health report: what was inspected, how it
// turned out, and a short human explanation.
//
// A Check carries no formatting. The CLI decides which glyph and colour to give
// each Level, so that this package stays free of presentation.
type Check struct {
	Name   string
	Level  Level
	Detail string
}

// Health is the result of inspecting a repository before a release.
type Health struct {
	Checks []Check
	// Branch is the branch that was inspected, or "(detached HEAD)".
	Branch string
}

// OK reports whether every check passed.
func (h Health) OK() bool { return h.Failures() == 0 && h.Warnings() == 0 }

// Warnings counts the checks that passed with a caveat.
func (h Health) Warnings() int { return h.count(LevelWarn) }

// Failures counts the checks that block a release.
func (h Health) Failures() int { return h.count(LevelFail) }

func (h Health) count(level Level) int {
	n := 0
	for _, c := range h.Checks {
		if c.Level == level {
			n++
		}
	}
	return n
}

// Concerns returns the details of every check that did not pass, so a caller can
// explain a rating without re-deriving it.
func (h Health) Concerns() []string {
	var out []string
	for _, c := range h.Checks {
		if c.Level != LevelOK {
			out = append(out, c.Detail)
		}
	}
	return out
}

// Health inspects the repository and reports on its readiness to publish a
// release.
//
// It never returns an error for a condition it can describe: a dirty tree or an
// unsynchronised branch becomes a Check, not a failure, so the caller can show
// the whole picture at once rather than stopping at the first problem. Only a
// repository it cannot inspect at all produces an error.
//
// Health does not enforce policy. Plan still refuses to release from a dirty
// tree; this is the report that explains why, before it happens.
func (s *Service) Health(ctx context.Context) (Health, error) {
	if err := s.ensureRepository(ctx); err != nil {
		return Health{}, err
	}

	health := Health{Checks: []Check{
		{Name: "Git repository", Level: LevelOK, Detail: "inside a Git work tree"},
	}}

	branch := s.checkBranch(ctx, &health)
	health.Branch = branch

	s.checkWorkTree(ctx, &health)
	s.checkSynchronised(ctx, &health)
	s.checkAuthentication(&health)

	return health, nil
}

// checkBranch reports whether releases are permitted from the current branch.
func (s *Service) checkBranch(ctx context.Context, health *Health) string {
	branch, err := s.git.CurrentBranch(ctx)
	if errors.Is(err, git.ErrDetachedHead) {
		branch = detachedHead
		level := LevelFail
		if len(s.cfg.Branches) == 0 {
			level = LevelWarn
		}
		health.Checks = append(health.Checks, Check{
			Name:   "Release branch",
			Level:  level,
			Detail: "HEAD is detached, so there is no branch to release from",
		})
		return branch
	}
	if err != nil {
		health.Checks = append(health.Checks, Check{
			Name: "Release branch", Level: LevelFail, Detail: err.Error(),
		})
		return ""
	}

	if !s.branchAllowed(branch) {
		health.Checks = append(health.Checks, Check{
			Name:  "Release branch",
			Level: LevelFail,
			Detail: fmt.Sprintf("on %s, but releases are only allowed from %s",
				branch, strings.Join(s.cfg.Branches, ", ")),
		})
		return branch
	}
	health.Checks = append(health.Checks, Check{
		Name: "Release branch", Level: LevelOK, Detail: "on " + branch,
	})
	return branch
}

// checkWorkTree reports separately on modified files and on untracked ones,
// because they call for different remedies: one is committed, the other is
// usually ignored.
func (s *Service) checkWorkTree(ctx context.Context, health *Health) {
	status, err := s.git.Status(ctx)
	if err != nil {
		health.Checks = append(health.Checks, Check{
			Name: "Working tree", Level: LevelFail, Detail: err.Error(),
		})
		return
	}

	modified, untracked := partitionStatus(status)

	switch {
	case len(modified) == 0:
		health.Checks = append(health.Checks, Check{
			Name: "Working tree", Level: LevelOK, Detail: "clean",
		})
	case s.cfg.AllowDirty:
		health.Checks = append(health.Checks, Check{
			Name:   "Working tree",
			Level:  LevelWarn,
			Detail: fmt.Sprintf("%s uncommitted, allowed by --allow-dirty", plural(len(modified), "change", "changes")),
		})
	default:
		health.Checks = append(health.Checks, Check{
			Name:   "Working tree",
			Level:  LevelFail,
			Detail: fmt.Sprintf("%s uncommitted", plural(len(modified), "change", "changes")),
		})
	}

	switch {
	case len(untracked) == 0:
		health.Checks = append(health.Checks, Check{
			Name: "Untracked files", Level: LevelOK, Detail: "none",
		})
	case s.cfg.AllowDirty:
		health.Checks = append(health.Checks, Check{
			Name:   "Untracked files",
			Level:  LevelWarn,
			Detail: fmt.Sprintf("%s, allowed by --allow-dirty", plural(len(untracked), "file", "files")),
		})
	default:
		health.Checks = append(health.Checks, Check{
			Name:   "Untracked files",
			Level:  LevelFail,
			Detail: fmt.Sprintf("%s not committed or ignored", plural(len(untracked), "file", "files")),
		})
	}
}

// checkSynchronised compares the branch with its upstream. A branch that is
// behind would tag a commit that is not the newest, which is nearly always a
// mistake; a branch that is ahead simply has unpushed work, which is fine.
func (s *Service) checkSynchronised(ctx context.Context, health *Health) {
	upstream, err := s.git.Upstream(ctx)
	if errors.Is(err, git.ErrNoUpstream) {
		health.Checks = append(health.Checks, Check{
			Name:   "Branch synchronised",
			Level:  LevelWarn,
			Detail: "the branch tracks no remote branch",
		})
		return
	}
	if err != nil {
		health.Checks = append(health.Checks, Check{
			Name: "Branch synchronised", Level: LevelWarn, Detail: err.Error(),
		})
		return
	}

	ahead, behind, err := s.git.AheadBehind(ctx, upstream)
	if err != nil {
		health.Checks = append(health.Checks, Check{
			Name: "Branch synchronised", Level: LevelWarn, Detail: err.Error(),
		})
		return
	}

	switch {
	case ahead == 0 && behind == 0:
		health.Checks = append(health.Checks, Check{
			Name: "Branch synchronised", Level: LevelOK, Detail: "up to date with " + upstream,
		})
	case behind > 0:
		health.Checks = append(health.Checks, Check{
			Name:   "Branch synchronised",
			Level:  LevelWarn,
			Detail: fmt.Sprintf("%s behind %s; the tag would miss those commits", plural(behind, "commit", "commits"), upstream),
		})
	default:
		health.Checks = append(health.Checks, Check{
			Name:   "Branch synchronised",
			Level:  LevelWarn,
			Detail: fmt.Sprintf("%s ahead of %s and not yet pushed", plural(ahead, "commit", "commits"), upstream),
		})
	}
}

// checkAuthentication reports whether a GitHub token is available.
//
// It deliberately makes no network call. Cutting a tag never touches the GitHub
// API — the release workflow publishes — so verifying a credential the command
// will not use would add latency and a network dependency to an operation that
// works offline. A missing token is a warning, not a failure.
func (s *Service) checkAuthentication(health *Health) {
	if os.Getenv("GITHUB_TOKEN") == "" {
		health.Checks = append(health.Checks, Check{
			Name:   "GitHub authentication",
			Level:  LevelWarn,
			Detail: "GITHUB_TOKEN is not set; the release workflow will publish",
		})
		return
	}
	health.Checks = append(health.Checks, Check{
		Name: "GitHub authentication", Level: LevelOK, Detail: "GITHUB_TOKEN is set",
	})
}

// partitionStatus splits porcelain status lines into modified and untracked.
func partitionStatus(status []string) (modified, untracked []string) {
	for _, line := range status {
		if strings.HasPrefix(line, "??") {
			untracked = append(untracked, line)
			continue
		}
		modified = append(modified, line)
	}
	return modified, untracked
}

// plural renders "1 change" or "3 changes".
func plural(n int, singular, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, many)
}
