package releasenotes

import (
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/github"
)

// The adapters satisfy these ports structurally. Asserted here so a signature
// drift is a compile error in this package rather than at the call site.
var (
	_ Repository        = (*git.Repo)(nil)
	_ PullRequestSource = (*github.Releases)(nil)
)
