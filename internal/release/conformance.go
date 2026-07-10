package release

import "github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"

// git.Repo satisfies the repository port structurally. Asserted here because
// git must not import this package: the dependency runs the other way, and that
// is what keeps the graph acyclic.
var _ Repository = (*git.Repo)(nil)
