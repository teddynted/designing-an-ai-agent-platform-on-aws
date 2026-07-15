//go:build integration

// Package router's integration tests. They build a REAL router over a real Ollama and a real
// Amazon Bedrock, and route real inferences between them.
//
// # Why they are behind a build tag
//
// The unit tests in this package prove the router's logic — the constraint gate, the
// fallback chain, the two guards that stop a stream or a conversation being failed over —
// and they prove it against fakes, in milliseconds, with no network. That is the right way
// to test routing LOGIC, because a routing bug is a decision bug, and a decision can be
// checked with a struct literal.
//
// What a fake cannot check is the thing the whole milestone is FOR: that a request really
// does land on a local model on one machine and a managed model in AWS on another, through
// one interface, chosen at request time. That needs both providers actually running, and a
// test that needs a GPU and an AWS account is not a unit test — it fails on an aeroplane, it
// costs money, and it goes red for reasons unrelated to the change. So it is opt-in:
//
//	OLLAMA_BASE_URL=http://localhost:11434 OLLAMA_MODEL=llama3.2 \
//	BEDROCK_MODEL_ID=us.anthropic.claude-3-5-haiku-20241022-v1:0 \
//	  go test -tags=integration ./internal/router/ -v
//
// Either provider missing skips, rather than fails — the same contract as the Bedrock suite.
package router

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/bedrock"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/ollama"
)

// liveFleet builds a router over the two real providers, or skips if either is not
// configured.
//
// This is the ONE test file in the package allowed to import the vendors, and only because
// it is the one asking a question a fake cannot answer: does routing work against the actual
// things. The architecture test excludes _test files from the import check for exactly this
// reason — a test that stands up the real providers is not the router learning their names.
func liveFleet(t *testing.T) *Router {
	t.Helper()

	if os.Getenv("OLLAMA_BASE_URL") == "" || os.Getenv("OLLAMA_MODEL") == "" {
		t.Skip("set OLLAMA_BASE_URL and OLLAMA_MODEL to run the router integration tests")
	}
	if os.Getenv("BEDROCK_MODEL_ID") == "" {
		t.Skip("set BEDROCK_MODEL_ID (and have AWS credentials) to run the router integration tests")
	}

	oCfg, err := ollama.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ollama config: %v", err)
	}
	oProvider, err := ollama.New(oCfg, discard())
	if err != nil {
		t.Fatalf("ollama: %v", err)
	}

	bCfg, err := bedrock.ConfigFromEnv()
	if err != nil {
		t.Fatalf("bedrock config: %v", err)
	}
	bCfg.MaxTokens = 64 // keep it cheap; this checks routing, not prose
	bProvider, err := bedrock.New(context.Background(), bCfg, discard())
	if err != nil {
		t.Fatalf("bedrock: %v", err)
	}

	providers := map[string]llm.Provider{"ollama": oProvider, "bedrock": bProvider}
	cfg := Config{
		Providers:       []string{"ollama", "bedrock"},
		Default:         "ollama",
		Strategy:        StrategyPurpose,
		Rules:           map[llm.Purpose]string{"release-notes": "bedrock"},
		Fallback:        true,
		HealthThreshold: DefaultHealthThreshold,
		HealthCooldown:  DefaultHealthCooldown,
	}
	r, err := New(providers, cfg, discard())
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	return r
}

// The core claim, checked against the real things: two requests, one interface, two
// different machines. An ordinary request runs on the local GPU; a release-notes request
// runs on Claude in AWS — and the only difference between them is the purpose.
func TestLiveRoutingSendsWorkToTheRightPlace(t *testing.T) {
	r := liveFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	local, err := r.Generate(ctx, llm.Request{
		Prompt:  "Say the word: hello",
		Purpose: "diff-summary", // unruled → the default, which is local
		Options: llm.Options{MaxTokens: 16, Temperature: 0},
	})
	if err != nil {
		t.Fatalf("local route: %v", err)
	}
	if local.Provider != "ollama" {
		t.Errorf("an unruled request was served by %q, want the local default", local.Provider)
	}

	hosted, err := r.Generate(ctx, llm.Request{
		Prompt:  "Say the word: hello",
		Purpose: "release-notes", // ruled → bedrock
		Options: llm.Options{MaxTokens: 16, Temperature: 0},
	})
	if err != nil {
		t.Fatalf("hosted route: %v", err)
	}
	if hosted.Provider != "bedrock" {
		t.Errorf("a release-notes request was served by %q, want bedrock", hosted.Provider)
	}

	t.Logf("routed: diff-summary → %s, release-notes → %s", local.Provider, hosted.Provider)
}

// A real prompt that must not leave really does stay on the local model.
func TestLiveRequireLocalStaysHome(t *testing.T) {
	r := liveFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Even though the purpose rule points at Bedrock, the constraint wins.
	res, err := r.Generate(ctx, llm.Request{
		Prompt:       "Say the word: hello",
		Purpose:      "release-notes",
		RequireLocal: true,
		Options:      llm.Options{MaxTokens: 16, Temperature: 0},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Provider != "ollama" {
		t.Errorf("a RequireLocal request was served by %q — it must stay on the local provider "+
			"no matter what the routing rule says", res.Provider)
	}
}

// The active probe reaches both real providers.
func TestLiveCheck(t *testing.T) {
	r := liveFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, s := range r.Check(ctx) {
		t.Logf("%s: healthy=%v models=%d latency=%v err=%q",
			s.Provider, s.Healthy, s.Models, s.Latency, s.Error)
		if !s.Healthy {
			t.Errorf("%s is not healthy: %s", s.Provider, s.Error)
		}
	}
}
