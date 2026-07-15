package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// --- the seam -------------------------------------------------------------------------

// THE test of the milestone: a router is an llm.Provider, so everything above it — the
// service, the tool loop, every caller — is holding one interface and cannot tell that there
// are two models behind it.
func TestARouterIsJustAProvider(t *testing.T) {
	var p llm.Provider = build(t, Config{}, local("ollama"), hosted("bedrock"))

	if p.Name() != Name {
		t.Errorf("Name() = %q, want %q", p.Name(), Name)
	}

	// And the platform's own Service takes it without knowing what it is.
	svc := llm.NewService(p, discard())
	res, err := svc.Generate(context.Background(), llm.Request{Prompt: "hello"})
	if err != nil {
		t.Fatalf("Generate through llm.Service: %v", err)
	}
	if res.Provider != "ollama" {
		t.Errorf("served by %q, want ollama (the default)", res.Provider)
	}
}

// --- capabilities: the union/intersection rule -----------------------------------------

// Getting this backwards is a security bug rather than a rounding error, so it is pinned.
//
// A CAPABILITY is a union: the router can use tools because Bedrock can, even though Ollama
// cannot. Reporting the intersection would mean that adding a small local model REMOVED the
// platform's ability to use tools, which is an absurd thing for adding a provider to do.
//
// A GUARANTEE is an intersection: Local means "the prompt does not leave", and a router that
// might send this one to Bedrock cannot promise that.
func TestCapabilitiesAreAUnionButLocalIsAGuarantee(t *testing.T) {
	t.Run("a mixed fleet can do everything and promises nothing", func(t *testing.T) {
		r := build(t, Config{}, local("ollama"), hosted("bedrock"))
		caps := r.Capabilities()

		if !caps.Tools || !caps.Reasoning || !caps.StructuredOutput {
			t.Errorf("caps = %+v, want tools/reasoning/structured — Bedrock can, so the router can", caps)
		}
		if caps.MaxContextTokens != 200_000 {
			t.Errorf("MaxContextTokens = %d, want the largest window in the fleet", caps.MaxContextTokens)
		}
		if caps.Local {
			t.Error("Local must be FALSE for a fleet containing a hosted provider — the router " +
				"might send this prompt to Bedrock, so it cannot promise the prompt stays home")
		}
		// Pessimistic on purpose: the tool loop's cost budget uses this, and under-estimating
		// lets a conversation run past a budget somebody set deliberately.
		if caps.CostPer1MInputTokensUSD != 3 {
			t.Errorf("cost = %v, want the WORST price in the fleet", caps.CostPer1MInputTokensUSD)
		}
	})

	t.Run("an all-local fleet keeps the guarantee", func(t *testing.T) {
		r := build(t, Config{}, local("ollama"), local("ollama-2"))

		if !r.Capabilities().Local {
			t.Error("Local must be TRUE when every enabled provider is local — this is what makes " +
				"LLM_ROUTER_PROVIDERS=ollama a real guarantee rather than a preference")
		}
	})
}

// --- the constraint gate ---------------------------------------------------------------

// The most important test in the package.
//
// RequireLocal is a constraint, not a preference. No strategy, no configuration, and no
// outage may cause a request that sets it to be served by a hosted provider — and when that
// leaves nothing to serve it, being REFUSED is the correct outcome.
func TestRequireLocalIsNeverTradedAway(t *testing.T) {
	t.Run("it routes to the local provider even when the strategy prefers the hosted one", func(t *testing.T) {
		ollama, bedrock := local("ollama"), hosted("bedrock")
		r := build(t, Config{Default: "bedrock"}, ollama, bedrock)

		res, err := r.Generate(context.Background(), llm.Request{Prompt: "secret", RequireLocal: true})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if res.Provider != "ollama" {
			t.Errorf("served by %q — a hosted provider was configured as the default, and the "+
				"constraint must beat the configuration", res.Provider)
		}
		if bedrock.count() != 0 {
			t.Error("the hosted provider was CALLED with a prompt that must not leave the network")
		}
	})

	t.Run("it is refused rather than served by a hosted provider when the local one is DOWN", func(t *testing.T) {
		ollama, bedrock := local("ollama"), hosted("bedrock")
		ollama.err = llm.ErrUnavailable

		r := build(t, Config{Fallback: true}, ollama, bedrock)

		_, err := r.Generate(context.Background(), llm.Request{Prompt: "secret", RequireLocal: true})
		if err == nil {
			t.Fatal("want an error — there is nothing left that may serve this request")
		}
		// This is the line that matters. Fallback is ENABLED, a working provider is sitting
		// right there, and the router must refuse to use it: an outage is not a reason to send
		// somebody's source code to a third party.
		if bedrock.count() != 0 {
			t.Fatal("the router FELL BACK to a hosted provider with a prompt that required local " +
				"inference. An outage is not a reason to relax a privacy constraint — this is the " +
				"single worst thing this package could do")
		}
	})

	t.Run("it is refused when no local provider is enabled at all", func(t *testing.T) {
		r := build(t, Config{}, hosted("bedrock"))

		_, err := r.Generate(context.Background(), llm.Request{Prompt: "secret", RequireLocal: true})
		if !errors.Is(err, llm.ErrNoProvider) {
			t.Fatalf("err = %v, want llm.ErrNoProvider", err)
		}
		// ErrNoProvider, NOT ErrUnavailable: nothing is broken, and no amount of retrying will
		// make this work. An alert that confused the two would page someone about an outage
		// that is really a missing environment variable.
		if llm.Retryable(err) {
			t.Error("ErrNoProvider must not be retryable — a retry loop would ask a fleet of " +
				"providers the same impossible question forever")
		}
		if !strings.Contains(err.Error(), EnvProviders) {
			t.Errorf("the error should say how to FIX it (mention %s); got: %v", EnvProviders, err)
		}
	})
}

// A request needing tools is routed to a provider that HAS tools, automatically — because the
// one that does not is not a candidate. Capability-aware routing falls out of the gate rather
// than being a feature anybody had to build.
func TestARequestIsRoutedToAProviderThatCanActuallyServeIt(t *testing.T) {
	tests := []struct {
		name string
		req  llm.Request
		want string
	}{
		{
			name: "tools go to the provider that has them",
			req:  llm.Request{Prompt: "x", Tools: []llm.ToolSpec{{Name: "run_workflow"}}},
			want: "bedrock",
		},
		{
			name: "reasoning goes to the provider that supports it",
			req:  llm.Request{Prompt: "x", Reasoning: &llm.ReasoningConfig{BudgetTokens: 1024}},
			want: "bedrock",
		},
		{
			name: "a prompt too big for the local window goes to the big window",
			req:  llm.Request{Prompt: strings.Repeat("x", (8192+1)*llm.CharsPerToken)},
			want: "bedrock",
		},
		{
			name: "an ordinary prompt stays on the default, which is local and free",
			req:  llm.Request{Prompt: "summarise this diff"},
			want: "ollama",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The default is the LOCAL provider throughout: every reroute below is the
			// constraint gate overruling the configured preference, not a strategy choosing.
			r := build(t, Config{Default: "ollama"}, local("ollama"), hosted("bedrock"))

			res, err := r.Generate(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if res.Provider != tc.want {
				t.Errorf("served by %q, want %q", res.Provider, tc.want)
			}
		})
	}
}

// A capability nobody has is refused, not attempted. This is llm.ErrUnsupported's argument,
// inherited: a model handed tools it does not understand does not refuse — it ignores them
// and answers from memory, fluently and wrongly.
func TestACapabilityNobodyHasIsRefused(t *testing.T) {
	r := build(t, Config{}, local("ollama"), local("ollama-2"))

	_, err := r.Generate(context.Background(), llm.Request{
		Prompt: "x",
		Tools:  []llm.ToolSpec{{Name: "run_workflow"}},
	})
	if !errors.Is(err, llm.ErrNoProvider) {
		t.Fatalf("err = %v, want llm.ErrNoProvider", err)
	}
	// And it must say WHY each provider was excluded. "No provider can serve this request" is
	// a maddening error to receive on its own.
	if !strings.Contains(err.Error(), "ollama") || !strings.Contains(err.Error(), "tools") {
		t.Errorf("the error must name each provider and its reason; got: %v", err)
	}
}

// --- fallback -------------------------------------------------------------------------

func TestFallback(t *testing.T) {
	t.Run("a provider that is down fails over to one that is not", func(t *testing.T) {
		ollama, bedrock := local("ollama"), hosted("bedrock")
		ollama.err = llm.ErrUnavailable

		r := build(t, Config{Fallback: true}, ollama, bedrock)

		res, err := r.Generate(context.Background(), llm.Request{Prompt: "x"})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if res.Provider != "bedrock" {
			t.Errorf("served by %q, want bedrock", res.Provider)
		}
		if ollama.count() != 1 {
			t.Errorf("the primary was called %d times, want exactly 1", ollama.count())
		}
	})

	t.Run("it works in the other direction too", func(t *testing.T) {
		ollama, bedrock := local("ollama"), hosted("bedrock")
		bedrock.err = llm.ErrThrottled

		r := build(t, Config{Providers: []string{"bedrock", "ollama"}, Default: "bedrock", Fallback: true}, ollama, bedrock)

		res, err := r.Generate(context.Background(), llm.Request{Prompt: "x"})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if res.Provider != "ollama" {
			t.Errorf("served by %q, want ollama — Bedrock throttling under load is the single most "+
				"likely reason this platform ever needs to fall back", res.Provider)
		}
	})

	t.Run("disabled, it does not", func(t *testing.T) {
		ollama, bedrock := local("ollama"), hosted("bedrock")
		ollama.err = llm.ErrUnavailable

		r := build(t, Config{Fallback: false}, ollama, bedrock)

		if _, err := r.Generate(context.Background(), llm.Request{Prompt: "x"}); !errors.Is(err, llm.ErrUnavailable) {
			t.Fatalf("err = %v, want the primary's error passed straight through", err)
		}
		if bedrock.count() != 0 {
			t.Error("fallback is disabled and the router fell back anyway")
		}
	})

	t.Run("when everything fails, the error names everything that was tried", func(t *testing.T) {
		ollama, bedrock := local("ollama"), hosted("bedrock")
		ollama.err = llm.ErrUnavailable
		bedrock.err = llm.ErrThrottled

		r := build(t, Config{Fallback: true}, ollama, bedrock)

		_, err := r.Generate(context.Background(), llm.Request{Prompt: "x"})
		if err == nil {
			t.Fatal("want an error")
		}
		for _, want := range []string{"ollama", "bedrock"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error must say what was tried; %q is missing from: %v", want, err)
			}
		}
		// The LAST error still wraps through, so a caller's retry policy can still classify it.
		if !errors.Is(err, llm.ErrThrottled) {
			t.Errorf("the underlying error must survive wrapping; got %v", err)
		}
	})

	// A bad request is not an outage. Asking a second, more expensive model the same
	// malformed question gets the same answer and a bill.
	t.Run("it does not fall over from a failure the next provider would repeat", func(t *testing.T) {
		for _, err := range []error{
			llm.ErrInvalidRequest,
			llm.ErrContextExceeded,
			llm.ErrSchemaViolation,
			llm.ErrEmptyCompletion,
			context.Canceled,
		} {
			ollama, bedrock := local("ollama"), hosted("bedrock")
			ollama.err = err

			r := build(t, Config{Fallback: true}, ollama, bedrock)
			_, got := r.Generate(context.Background(), llm.Request{Prompt: "x"})

			if !errors.Is(got, err) {
				t.Errorf("err = %v, want %v returned as-is", got, err)
			}
			if bedrock.count() != 0 {
				t.Errorf("the router fell over from %v — the second provider would fail the same "+
					"way, more expensively", err)
			}
		}
	})

	// The chain is a subset of the enabled providers with each appearing once, so there is
	// nowhere for a loop to be. This asserts the structure rather than trusting it.
	t.Run("each provider is tried at most once, so a chain cannot loop", func(t *testing.T) {
		ollama, bedrock := local("ollama"), hosted("bedrock")
		ollama.err = llm.ErrUnavailable
		bedrock.err = llm.ErrUnavailable

		r := build(t, Config{Fallback: true}, ollama, bedrock)
		_, _ = r.Generate(context.Background(), llm.Request{Prompt: "x"})

		if ollama.count() != 1 || bedrock.count() != 1 {
			t.Errorf("calls: ollama=%d bedrock=%d — every provider must be tried exactly once",
				ollama.count(), bedrock.count())
		}
	})
}

// --- the two guards ---------------------------------------------------------------------

// A stream that has already sent a token must NOT fail over. The caller holds the beginning
// of an answer; a second provider would hand them the beginning of a different one,
// concatenated onto the first, with no error anywhere.
//
// Note the fake returns a plain ErrUnavailable — NOT ErrStreamBroken. The router must refuse
// to fall over anyway, because it counts what actually reached the caller rather than
// trusting the provider to have wrapped its error correctly.
func TestAStreamThatHasAlreadySpokenIsNeverFailedOver(t *testing.T) {
	ollama, bedrock := local("ollama"), hosted("bedrock")
	ollama.chunks = []string{"The answer ", "is "}
	ollama.err = llm.ErrUnavailable // deliberately NOT ErrStreamBroken

	r := build(t, Config{Fallback: true}, ollama, bedrock)

	var got []string
	_, err := r.Stream(context.Background(), llm.Request{Prompt: "x"}, func(c llm.Chunk) error {
		if c.Content != "" {
			got = append(got, c.Content)
		}
		return nil
	})

	if bedrock.count() != 0 {
		t.Fatal("the router failed over a stream that had ALREADY sent output. The caller now " +
			"has the beginning of one answer followed by the beginning of another, and nothing " +
			"downstream will ever know")
	}
	if !errors.Is(err, llm.ErrStreamBroken) {
		t.Errorf("err = %v, want it classified as llm.ErrStreamBroken — the router must say that "+
			"output escaped, whatever the provider called it", err)
	}
	if strings.Join(got, "") != "The answer is " {
		t.Errorf("chunks = %q, want only what the first provider sent", got)
	}
}

// A stream that fails BEFORE saying anything is safe to fail over: the caller has seen
// nothing, so nothing is lost.
func TestAStreamThatHasNotSpokenYetIsSafeToFailOver(t *testing.T) {
	ollama, bedrock := local("ollama"), hosted("bedrock")
	ollama.err = llm.ErrUnavailable // no chunks

	r := build(t, Config{Fallback: true}, ollama, bedrock)

	var got string
	res, err := r.Stream(context.Background(), llm.Request{Prompt: "x"}, func(c llm.Chunk) error {
		got += c.Content
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Provider != "bedrock" || got != "answered by bedrock" {
		t.Errorf("served by %q (%q), want a clean take-over by bedrock", res.Provider, got)
	}
}

// THE subtle one, and the bug that would otherwise have shipped.
//
// llm.Service.Converse runs a tool loop by calling Generate once per turn, replaying the
// whole conversation each time. To the router those turns are separate requests. So the
// obvious router sends turn 1 to Bedrock and, when Bedrock hiccups, turn 4 to Ollama —
// carrying Claude's SIGNED reasoning block and Bedrock's tool-call IDs to a provider that
// has never seen either.
//
// A conversation is pinned to the provider that started it, and it does not fall over.
func TestAConversationInProgressNeverChangesProvider(t *testing.T) {
	// The conversation so far: the model asked for a tool, and its reasoning is signed.
	inFlight := llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "run the release workflow"},
			{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "toolu_01ABC", Name: "run_workflow"}},
				Reasoning: &llm.ReasoningBlock{Text: "I should run it", Signature: "sig-from-bedrock"},
			},
			{Role: llm.RoleUser, ToolResults: []llm.ToolResult{{ID: "toolu_01ABC", Content: "ok"}}},
		},
		Tools: []llm.ToolSpec{{Name: "run_workflow"}},
	}

	t.Run("it is not failed over when the provider dies mid-conversation", func(t *testing.T) {
		bedrock, bedrock2 := hosted("bedrock"), hosted("bedrock-2")
		bedrock.err = llm.ErrUnavailable

		r := build(t, Config{Fallback: true}, bedrock, bedrock2)

		_, err := r.Generate(context.Background(), inFlight)
		if err == nil {
			t.Fatal("want an error: the conversation's state cannot migrate, so there is nothing " +
				"to fall over TO")
		}
		if bedrock2.count() != 0 {
			t.Fatal("the router moved a live tool-using conversation to another provider. The " +
				"replayed messages carry the FIRST provider's signed reasoning block and its " +
				"tool-call IDs — the second provider has never seen either, and cannot verify or " +
				"even parse them")
		}
		// And the error has to explain itself, or the next person to read it will conclude the
		// fallback is broken and "fix" it.
		if !strings.Contains(err.Error(), "conversation") {
			t.Errorf("the error must say WHY it did not fall over; got: %v", err)
		}
	})

	t.Run("a fresh request with the same tools still routes and still falls over", func(t *testing.T) {
		// The control. Tools alone do not pin anything — it is the conversation HISTORY that
		// carries provider state. A first turn has none, so it routes and fails over normally.
		bedrock, bedrock2 := hosted("bedrock"), hosted("bedrock-2")
		bedrock.err = llm.ErrUnavailable

		r := build(t, Config{Fallback: true}, bedrock, bedrock2)

		res, err := r.Generate(context.Background(), llm.Request{
			Prompt: "run the release workflow",
			Tools:  []llm.ToolSpec{{Name: "run_workflow"}},
		})
		if err != nil {
			t.Fatalf("a FIRST turn carries no provider state and must fall over normally: %v", err)
		}
		if res.Provider != "bedrock-2" {
			t.Errorf("served by %q, want bedrock-2", res.Provider)
		}
	})
}

// --- the manual override ----------------------------------------------------------------

func TestAPinnedRequestGoesWhereItWasTold(t *testing.T) {
	t.Run("it overrides the strategy", func(t *testing.T) {
		r := build(t, Config{Default: "ollama"}, local("ollama"), hosted("bedrock"))

		res, err := r.Generate(context.Background(), llm.Request{Prompt: "x", Provider: "bedrock"})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if res.Provider != "bedrock" {
			t.Errorf("served by %q, want bedrock", res.Provider)
		}
	})

	// An override that gets silently overridden is not an override. Someone who names a
	// provider has a reason, and would rather have an error than a surprise.
	t.Run("it does NOT fall over", func(t *testing.T) {
		ollama, bedrock := local("ollama"), hosted("bedrock")
		bedrock.err = llm.ErrUnavailable

		r := build(t, Config{Fallback: true}, ollama, bedrock)

		_, err := r.Generate(context.Background(), llm.Request{Prompt: "x", Provider: "bedrock"})
		if err == nil {
			t.Fatal("want an error")
		}
		if ollama.count() != 0 {
			t.Fatal("the router served a pinned request from a different provider")
		}
		if !strings.Contains(err.Error(), "pinned") {
			t.Errorf("the error must explain that fallback was declined on purpose; got: %v", err)
		}
	})

	t.Run("it is refused, not rerouted, when the pinned provider cannot do the job", func(t *testing.T) {
		r := build(t, Config{}, local("ollama"), hosted("bedrock"))

		_, err := r.Generate(context.Background(), llm.Request{
			Prompt:   "x",
			Provider: "ollama",
			Tools:    []llm.ToolSpec{{Name: "run_workflow"}},
		})
		if !errors.Is(err, llm.ErrNoProvider) {
			t.Fatalf("err = %v, want a refusal rather than a quiet reroute to bedrock", err)
		}
	})

	t.Run("an unknown provider says so", func(t *testing.T) {
		r := build(t, Config{}, local("ollama"))

		_, err := r.Generate(context.Background(), llm.Request{Prompt: "x", Provider: "gpt-9"})
		if !errors.Is(err, llm.ErrInvalidRequest) {
			t.Fatalf("err = %v, want llm.ErrInvalidRequest", err)
		}
	})
}

// --- health ------------------------------------------------------------------------------

// The point of the health tracker, stated as a test: a provider known to be failing is not
// tried first on the next request. Without this, fallback WORKS and every request pays the
// dead provider's full timeout — which on Bedrock's defaults is two minutes, three times.
func TestAFailingProviderIsDemotedOnTheNextRequest(t *testing.T) {
	ollama, bedrock := local("ollama"), hosted("bedrock")
	ollama.err = llm.ErrUnavailable

	r := build(t, Config{Fallback: true, HealthThreshold: 1}, ollama, bedrock)

	// First request: the router does not know yet, so it pays the cost of finding out.
	if _, err := r.Generate(context.Background(), llm.Request{Prompt: "x"}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if ollama.count() != 1 {
		t.Fatalf("the primary should have been tried once; got %d", ollama.count())
	}

	// Second request: it remembers. The dead provider is not tried at all.
	res, err := r.Generate(context.Background(), llm.Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Provider != "bedrock" {
		t.Errorf("served by %q, want bedrock", res.Provider)
	}
	if ollama.count() != 1 {
		t.Errorf("the failing provider was tried again (%d calls). Every request would pay its "+
			"full timeout to rediscover an outage the router already knew about", ollama.count())
	}
}

// A demoted provider is still in the chain. If everything looks unhealthy — a DNS blip that
// failed one request to each — the router must still try, rather than refuse all traffic
// because its circuit breaker has decided the world has ended.
func TestAnUnhealthyProviderIsDemotedButNeverRemoved(t *testing.T) {
	ollama, bedrock := local("ollama"), hosted("bedrock")
	ollama.err = llm.ErrUnavailable
	bedrock.err = llm.ErrUnavailable

	r := build(t, Config{Fallback: true, HealthThreshold: 1}, ollama, bedrock)

	// Condemn them both.
	_, _ = r.Generate(context.Background(), llm.Request{Prompt: "x"})
	for _, s := range r.Snapshot() {
		if s.Healthy {
			t.Fatalf("%s should be unhealthy by now", s.Provider)
		}
	}

	// Now let one recover, and ask again. If unhealthy meant "removed", this would refuse.
	bedrock.err = nil
	res, err := r.Generate(context.Background(), llm.Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("a fleet where everything looks unhealthy must still TRY, not refuse: %v", err)
	}
	if res.Provider != "bedrock" {
		t.Errorf("served by %q, want bedrock", res.Provider)
	}
}

// A provider recovers after the cooldown, and one request is allowed through to find out.
func TestAProviderIsGivenAnotherChanceAfterTheCooldown(t *testing.T) {
	clock := time.Now()
	ollama, bedrock := local("ollama"), hosted("bedrock")
	ollama.err = llm.ErrUnavailable

	providers := map[string]llm.Provider{"ollama": ollama, "bedrock": bedrock}
	cfg := Config{
		Providers: []string{"ollama", "bedrock"}, Default: "ollama", Strategy: StrategyFixed,
		Fallback: true, HealthThreshold: 1, HealthCooldown: 30 * time.Second,
	}
	r, err := New(providers, cfg, discard(), WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _ = r.Generate(context.Background(), llm.Request{Prompt: "x"}) // condemns ollama
	before := ollama.count()

	// Still inside the cooldown: not tried.
	clock = clock.Add(10 * time.Second)
	_, _ = r.Generate(context.Background(), llm.Request{Prompt: "x"})
	if ollama.count() != before {
		t.Error("a provider inside its cooldown must not be tried")
	}

	// Past it, and recovered. The half-open probe is a REAL request — the only test that
	// proves anything.
	clock = clock.Add(31 * time.Second)
	ollama.err = nil

	res, err := r.Generate(context.Background(), llm.Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Provider != "ollama" {
		t.Errorf("served by %q, want ollama — it should be back in service after the cooldown", res.Provider)
	}
}

// A missing model must fail over WITHOUT condemning the provider. Ollama is perfectly well:
// it answered immediately and told us the truth. Demoting it for every other request because
// one caller asked for a model it does not have is a circuit breaker tripped by a
// misaddressed letter.
func TestAskingForAModelAProviderDoesNotHaveDoesNotCondemnIt(t *testing.T) {
	ollama, bedrock := local("ollama"), hosted("bedrock")
	ollama.err = llm.ErrModelNotFound

	r := build(t, Config{Fallback: true, HealthThreshold: 1}, ollama, bedrock)

	res, err := r.Generate(context.Background(), llm.Request{Prompt: "x", Model: "claude"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Provider != "bedrock" {
		t.Errorf("served by %q, want bedrock — the model is not on the local provider", res.Provider)
	}

	for _, s := range r.Snapshot() {
		if s.Provider == "ollama" && !s.Healthy {
			t.Error("ollama was marked UNHEALTHY for not having a model. It is running, it " +
				"answered instantly, and it told us the truth — and it is now demoted for every " +
				"other request in the platform")
		}
	}
}

// --- models -------------------------------------------------------------------------------

func TestModelsAreTaggedWithWhereTheyLive(t *testing.T) {
	r := build(t, Config{}, local("ollama"), hosted("bedrock"))

	models, err := r.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	for _, m := range models {
		if m.Provider == "" {
			t.Errorf("model %q has no provider — \"llama3.2 and Claude are both available\" is a "+
				"useless sentence if it does not say where either of them is", m.Name)
		}
	}
}

// An inventory call is not an inference. "Bedrock is down, so I will not tell you what is on
// the GPU you are paying for" is not a useful answer.
func TestListingModelsSurvivesOneProviderBeingDown(t *testing.T) {
	ollama, bedrock := local("ollama"), hosted("bedrock")
	bedrock.modelsErr = llm.ErrUnavailable

	r := build(t, Config{}, ollama, bedrock)

	models, err := r.Models(context.Background())
	if err != nil {
		t.Fatalf("one provider being down must not fail the whole listing: %v", err)
	}
	if len(models) != 1 || models[0].Provider != "ollama" {
		t.Errorf("models = %+v, want just the local one", models)
	}

	// But if EVERYTHING is down, the fleet really is down, and saying so is correct.
	ollama.modelsErr = llm.ErrUnavailable
	if _, err := r.Models(context.Background()); err == nil {
		t.Error("want an error when no provider can be reached at all")
	}
}

// --- the active probe ---------------------------------------------------------------------

func TestCheckProbesEveryProviderWithoutGeneratingAToken(t *testing.T) {
	ollama, bedrock := local("ollama"), hosted("bedrock")
	bedrock.modelsErr = llm.ErrUnauthorized

	r := build(t, Config{HealthThreshold: 1}, ollama, bedrock)

	statuses := r.Check(context.Background())
	if len(statuses) != 2 {
		t.Fatalf("got %d statuses, want one per provider", len(statuses))
	}

	byName := map[string]Status{}
	for _, s := range statuses {
		byName[s.Provider] = s
	}
	if !byName["ollama"].Healthy || byName["ollama"].Models != 1 {
		t.Errorf("ollama = %+v, want healthy with its models listed", byName["ollama"])
	}
	if byName["bedrock"].Healthy {
		t.Error("bedrock rejected our credentials and must not be reported healthy")
	}
	if byName["bedrock"].Error == "" {
		t.Error("an unhealthy provider must say why")
	}

	// The probe listed models. It did not generate anything — which is what makes it safe to
	// put on a health endpoint.
	if ollama.count() != 1 {
		t.Errorf("the probe made %d calls, want exactly 1 (a Models listing)", ollama.count())
	}
}
