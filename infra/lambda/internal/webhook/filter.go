package webhook

import (
	"fmt"
	"strings"
)

// Disposition is what the platform decided to do with a delivery. It is the filter's
// output, and it is deliberately a small enum rather than a bool: "we published it"
// and "we deliberately ignored it" and "we refused it" are three different outcomes,
// and collapsing the middle one into either of the others is how a webhook pipeline
// becomes impossible to reason about — you cannot tell a dropped event from a rejected
// one from a processed one in the logs.
type Disposition string

const (
	// Accepted: the event passed every filter and was published to EventBridge.
	Accepted Disposition = "accepted"

	// Ignored: the event was valid and authentic, and the platform chose not to act on
	// it — an unsupported event, a repository not on the allow-list, a branch that does
	// not match, a fork, an archived repo, a branch deletion. This is a SUCCESS. GitHub
	// gets a 200, because the request was fine; the platform simply had nothing to do.
	Ignored Disposition = "ignored"

	// Acknowledged: a ping. The platform confirms the endpoint works and publishes
	// nothing. Its own disposition because "the webhook was just installed" is a thing
	// worth seeing in the logs as itself, not as an ignored event.
	Acknowledged Disposition = "acknowledged"
)

// FilterResult is the decision and the reason for it. The reason is not decoration:
// "ignored" with no reason is a support ticket, and "ignored: repository
// owner/other is not on the allow-list" is self-service.
type FilterResult struct {
	Disposition Disposition
	Reason      string
}

// Filter decides what to do with a parsed, verified delivery. It is a pure function
// of the delivery and the config — no I/O — so every rule below is a table test, and
// the order of the rules is visible and deliberate.
//
// # Filtering happens HERE, before anything downstream
//
// The milestone asks that filtering occur before downstream processing, and this is
// where "before" is. Nothing has been published yet when Filter runs; an ignored
// event costs one EventBridge PutEvents that never happens, rather than a workflow
// that starts and then discovers it should not have. The cheapest place to not do
// work is before you have started it.
//
// # The order is safe-first, then cheap-first
//
// Ping is answered first because it is not really an event at all. Then the
// SECURITY-ADJACENT drops — fork, archived — because those are the ones where
// processing the event would be actively wrong (an agent reading an attacker's fork),
// not merely wasteful. Then the POLICY drops — supported events, allow-lists, branch
// filters, deletions — which are about what this deployment cares about. A reader can
// see, top to bottom, that the dangerous things are refused before the merely
// uninteresting ones.
func Filter(d Delivery, cfg Config) FilterResult {
	// A ping is the webhook saying hello. Acknowledge it and publish nothing.
	if d.Event == EventPing {
		return FilterResult{Acknowledged, "ping acknowledged; the endpoint is reachable"}
	}

	// The parser understands it, but does this deployment accept it? An event type not
	// in the configured set (when a set is configured) is ignored.
	if !eventAccepted(d.Event, cfg.SupportedEvents) {
		return ignored("event type %q is not in this deployment's supported set", d.Event)
	}

	// --- security-adjacent: processing these would be wrong, not just wasteful ------

	if cfg.IgnoreForks && d.Fork {
		// An agent pointed at a fork reads content the fork's owner controls. That is
		// the Milestone 6 untrusted-content hazard arriving through the front door, so
		// the safe default is to not let a fork start anything.
		return ignored("repository %s is a fork", d.Repository)
	}
	if cfg.IgnoreArchived && d.Archived {
		return ignored("repository %s is archived", d.Repository)
	}

	// --- policy: what this deployment has been told to care about -------------------

	if len(cfg.RepoAllowList) > 0 && !contains(cfg.RepoAllowList, d.Repository) {
		return ignored("repository %s is not on the allow-list", d.Repository)
	}

	if cfg.IgnoreBranchDeletes && d.Deleted {
		return ignored("a branch or tag deletion (%s) — nothing to act on", orRef(d))
	}

	// Branch filtering applies only to events that HAVE a branch. A release or a
	// repository event has none, and filtering it on a branch it does not have would
	// silently drop every one of them.
	if d.Branch != "" && len(cfg.BranchAllowList) > 0 && !branchMatches(d.Branch, cfg.BranchAllowList) {
		return ignored("branch %q is not in the branch allow-list", d.Branch)
	}

	return FilterResult{Accepted, "passed every filter"}
}

// eventAccepted reports whether an event is in the configured set. An empty set means
// "every supported event" — the parser has already guaranteed the event is one it
// understands, so empty is safe to read as "all".
func eventAccepted(event string, configured []string) bool {
	if len(configured) == 0 {
		return true
	}
	return contains(configured, event)
}

// branchMatches supports exact names and a single trailing "/*" prefix wildcard, which
// is enough for the real cases ("main", "release/*") without importing a glob library
// whose full power nobody wants aimed at a branch name.
func branchMatches(branch string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "/*") {
			if strings.HasPrefix(branch, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if branch == p {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func ignored(format string, args ...any) FilterResult {
	return FilterResult{Ignored, fmt.Sprintf(format, args...)}
}

func orRef(d Delivery) string {
	if d.Ref != "" {
		return d.Ref
	}
	if d.Branch != "" {
		return d.Branch
	}
	return "unknown ref"
}
