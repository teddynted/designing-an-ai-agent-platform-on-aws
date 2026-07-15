package webhook

import (
	"strings"
	"testing"
)

// The filter is a pure function, so its test is a table of (delivery, config) →
// disposition. Every rule the milestone lists appears here, and the reasons are
// asserted loosely (non-empty) because a filter that drops an event without saying why
// is a filter nobody can operate.

func TestFilter(t *testing.T) {
	// A permissive base config: accept everything, ignore nothing. Each case turns on
	// exactly the rule it is testing, so a failure names the rule.
	base := Config{}

	strict := Config{
		SupportedEvents:     []string{EventPush, EventRelease},
		RepoAllowList:       []string{"acme/platform"},
		BranchAllowList:     []string{"main", "release/*"},
		IgnoreForks:         true,
		IgnoreArchived:      true,
		IgnoreBranchDeletes: true,
	}

	tests := []struct {
		name     string
		delivery Delivery
		cfg      Config
		want     Disposition
	}{
		{
			"a ping is acknowledged, never published",
			Delivery{Event: EventPing}, base, Acknowledged,
		},
		{
			"a normal push is accepted",
			Delivery{Event: EventPush, Repository: "acme/platform", Branch: "main"}, strict, Accepted,
		},
		{
			"an unsupported event is ignored",
			Delivery{Event: EventCreate, Repository: "acme/platform", Branch: "main"}, strict, Ignored,
		},
		{
			"a repository off the allow-list is ignored",
			Delivery{Event: EventPush, Repository: "someone/else", Branch: "main"}, strict, Ignored,
		},
		{
			"a fork is ignored (security-adjacent)",
			Delivery{Event: EventPush, Repository: "acme/platform", Branch: "main", Fork: true}, strict, Ignored,
		},
		{
			"an archived repository is ignored",
			Delivery{Event: EventPush, Repository: "acme/platform", Branch: "main", Archived: true}, strict, Ignored,
		},
		{
			"a branch off the allow-list is ignored",
			Delivery{Event: EventPush, Repository: "acme/platform", Branch: "dev"}, strict, Ignored,
		},
		{
			"a branch matching a /* prefix is accepted",
			Delivery{Event: EventPush, Repository: "acme/platform", Branch: "release/1.2"}, strict, Accepted,
		},
		{
			"a branch deletion is ignored",
			Delivery{Event: EventPush, Repository: "acme/platform", Deleted: true, Ref: "refs/heads/gone"}, strict, Ignored,
		},
		{
			"a release has no branch, so branch filtering does not silently drop it",
			Delivery{Event: EventRelease, Repository: "acme/platform", Action: "published"}, strict, Accepted,
		},
		{
			"with no config, everything supported is accepted",
			Delivery{Event: EventPush, Repository: "o/r", Branch: "any-branch"}, base, Accepted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Filter(tc.delivery, tc.cfg)
			if got.Disposition != tc.want {
				t.Errorf("disposition = %q (%s), want %q", got.Disposition, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Error("every decision must carry a reason")
			}
		})
	}
}

// The dangerous drops (fork, archived) are checked before the merely-uninteresting ones
// (allow-list, branch), so a fork is refused even when it would ALSO be dropped for
// another reason — the log then says "fork", the actionable reason, not "branch".
func TestSecurityDropsComeFirst(t *testing.T) {
	cfg := Config{IgnoreForks: true, RepoAllowList: []string{"acme/platform"}}
	// A fork that is also off the allow-list. The fork reason must win.
	d := Delivery{Event: EventPush, Repository: "attacker/platform", Fork: true, Branch: "main"}

	got := Filter(d, cfg)
	if got.Disposition != Ignored {
		t.Fatalf("want ignored, got %q", got.Disposition)
	}
	// The reason should mention the fork, the security-relevant fact — not the
	// allow-list, which is the reason it would ALSO have been dropped.
	if !strings.Contains(got.Reason, "fork") {
		t.Errorf("reason = %q, want it to name the fork drop (the security-relevant one)", got.Reason)
	}
}
