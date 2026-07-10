package release

import (
	"log/slog"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// Comparing two releases, from whichever source can answer.
//
// The GitHub Compare API is preferred when a host is configured: it performs
// rename detection server-side, reports per-file line counts, and works from a
// shallow clone — which is what actions/checkout gives you unless you remember
// `fetch-depth: 0`. Local git is the fallback, and the only option offline.
//
// The fallback is not silent. A comparison assembled from a shallow clone can be
// missing commits, and a release note that quietly omits half a release is worse
// than one that fails. When the host is asked and errors, the error propagates
// unless AllowFallback is set.

// ComparisonService produces a Comparison for a release and finds its
// predecessor.
type ComparisonService struct {
	repo Repository
	host Host // may be nil, meaning "local git only"

	// AllowFallback permits a failing host comparison to fall back to local git.
	AllowFallback bool

	// Logger records the fallback. Never nil after NewComparisonService.
	Logger *slog.Logger
}

// NewComparisonService builds the service. A nil host means local git only. A
// nil logger discards the fallback warning.
func NewComparisonService(repo Repository, host Host, allowFallback bool, logger *slog.Logger) *ComparisonService {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &ComparisonService{repo: repo, host: host, AllowFallback: allowFallback, Logger: logger}
}

// PreviousRelease is the release tag immediately below target, or nil for the
// first release.
//
// Resolved from git tags rather than from GitHub releases: a tag can exist
// without a published release — that is precisely the state during a release run
// — and the diff must be anchored to the tag either way.
func (s *ComparisonService) PreviousRelease(target *version.Version) (*git.Tag, error) {
	return s.repo.PreviousTag(target)
}

// canAskHost reports whether the forge could possibly answer this comparison.
//
// The host knows refs it has been pushed. An empty base is the first release,
// which only local git can express as a diff against the empty tree. A head such
// as HEAD names a commit the forge has not seen — that is the ordinary case when
// generating notes before the tag is pushed — and asking anyway would spend a
// request to earn a fallback warning on every local release.
func canAskHost(base, head string) bool {
	return base != "" && version.IsValid(base) && version.IsValid(head)
}

// Compare compares two refs, preferring the release host.
func (s *ComparisonService) Compare(base, head string, headVersion *version.Version) (git.Comparison, error) {
	if s.host != nil && canAskHost(base, head) {
		comparison, err := s.host.Compare(base, head)
		if err == nil {
			return comparison, nil
		}
		if !s.AllowFallback {
			return git.Comparison{}, err
		}
		s.Logger.Warn(
			"GitHub compare failed; falling back to local git. If this clone is shallow the comparison may be incomplete.",
			"base", base, "head", head, "error", err,
		)
	}
	return s.repo.Compare(base, head, headVersion)
}

// CompareRelease compares a release against its immediate predecessor.
//
// The one call a release pipeline needs: it resolves the predecessor and handles
// the first-release case in one step. head names the ref to diff, which is the
// release tag once it exists and HEAD before it does.
func (s *ComparisonService) CompareRelease(target version.Version, head string) (git.Comparison, error) {
	previous, err := s.PreviousRelease(&target)
	if err != nil {
		return git.Comparison{}, err
	}
	base := ""
	if previous != nil {
		base = previous.Name
	}
	if head == "" {
		head = target.Tag()
	}
	return s.Compare(base, head, &target)
}
