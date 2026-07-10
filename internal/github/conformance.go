package github

import "github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"

// Releases satisfies the release host port structurally. Asserting it here means
// a signature drift is a compile error in this package rather than a puzzling
// one at the call site.
var _ release.Host = (*Releases)(nil)
