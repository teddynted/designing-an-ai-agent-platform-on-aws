package router

import (
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// A strategy is a pure function of a request and a list of facts, so its test is a table —
// no providers, no network, no fakes. That is the entire reason [Candidate] carries facts
// rather than an llm.Provider.

func candidates(names ...string) []Candidate {
	out := make([]Candidate, 0, len(names))
	for _, n := range names {
		out = append(out, Candidate{Name: n, Healthy: true})
	}
	return out
}

func TestFixedStrategy(t *testing.T) {
	s := Fixed{Provider: "bedrock"}

	t.Run("picks its provider when eligible", func(t *testing.T) {
		if d := s.Select(llm.Request{}, candidates("ollama", "bedrock")); d.Provider != "bedrock" {
			t.Errorf("picked %q, want bedrock", d.Provider)
		}
	})

	// A preference bends: if the configured provider is not among the candidates (the gate
	// removed it because it cannot do the job), the strategy takes one that can — and the
	// reason says so, because a request that went somewhere other than the configuration
	// points is something to surface, not hide.
	t.Run("bends to a capable provider and says why", func(t *testing.T) {
		d := s.Select(llm.Request{}, candidates("ollama"))
		if d.Provider != "ollama" {
			t.Errorf("picked %q, want ollama (the only candidate)", d.Provider)
		}
		if d.Reason == "" {
			t.Error("a decision that overrode the configuration must explain itself")
		}
	})
}

func TestByPurposeStrategy(t *testing.T) {
	s := ByPurpose{
		Rules:   map[llm.Purpose]string{"release-notes": "bedrock", "diff-summary": "ollama"},
		Default: "ollama",
	}

	tests := []struct {
		name    string
		purpose llm.Purpose
		cands   []Candidate
		want    string
	}{
		{"a ruled purpose goes where the rule says", "release-notes", candidates("ollama", "bedrock"), "bedrock"},
		{"another ruled purpose", "diff-summary", candidates("ollama", "bedrock"), "ollama"},
		{"an unruled purpose falls to the default", "change-triage", candidates("ollama", "bedrock"), "ollama"},
		{"an empty purpose falls to the default", "", candidates("ollama", "bedrock"), "ollama"},
		{"a rule pointing at an ineligible provider bends to what can serve", "release-notes", candidates("ollama"), "ollama"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := s.Select(llm.Request{Purpose: tc.purpose}, tc.cands)
			if d.Provider != tc.want {
				t.Errorf("purpose %q picked %q, want %q", tc.purpose, d.Provider, tc.want)
			}
			if d.Reason == "" {
				t.Error("every decision must carry a reason for the log")
			}
		})
	}
}

// The whole platform's inference sits behind a strategy, so a strategy that could fail would
// be a new way to take inference down. Select returns no error BY DESIGN, and the contract is
// that it always names one of the candidates it was given. This pins the contract.
func TestAStrategyAlwaysPicksACandidateItWasGiven(t *testing.T) {
	strategies := []Strategy{
		Fixed{Provider: "not-in-the-list"},
		ByPurpose{Rules: map[llm.Purpose]string{"x": "not-in-the-list"}, Default: "also-not-in-the-list"},
	}
	cands := candidates("ollama", "bedrock")

	for _, s := range strategies {
		d := s.Select(llm.Request{Purpose: "x"}, cands)
		if !eligibleContains(cands, d.Provider) {
			t.Errorf("%s picked %q, which was not offered — a strategy must choose from the "+
				"candidates it is handed, never invent one", s.Name(), d.Provider)
		}
	}
}
