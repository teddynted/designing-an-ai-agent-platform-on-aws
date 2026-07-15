// Package router chooses a provider per request — and is itself an [llm.Provider].
//
// That one sentence is the whole design, and everything below is a consequence of it.
// Milestone 7 predicted it, in internal/providers, before there was anything to route:
//
//	It is not a router. It picks a provider from configuration, once, at start-up, and then
//	gets out of the way. Choosing a provider *per request* — by cost, by latency, by whether
//	this repository's source may leave the VPC — is Milestone 10, and it will implement
//	llm.Provider itself and sit exactly where a single provider sits today. Nothing above
//	will notice.
//
// Nothing above noticed. [llm.Service] does not know it exists; neither does the tool loop,
// the prompt catalogue, the CLI, or n8n. A Router is handed to `llm.NewService` in the
// place an Ollama client used to go, and the platform cannot tell the difference — which is
// the only test of a provider abstraction that means anything, and it is not one you can
// pass by writing an interface. You pass it by discovering, at the point where you finally
// have two of something, that you do not have to change anything.
//
// # What it is made of
//
//	CONSTRAIN  →  which providers CAN serve this request at all?      (a hard gate)
//	   ↓
//	SELECT     →  of those, which one SHOULD?                          (the strategy)
//	   ↓
//	ORDER      →  and if it fails, who is next?                        (health + fallback)
//	   ↓
//	EXECUTE    →  try them, in order, at most once each
//
// The order matters. Constraints are applied FIRST, and to every request, before any
// strategy or configuration setting is consulted — because a strategy is an opinion about
// which provider is *better*, and a constraint is a statement about which providers are
// *permissible*. Letting a preference overrule a constraint is how a router configured for
// cost ends up sending a private repository to a hosted model during an outage.
//
// # It never learns which providers exist
//
// This package imports internal/llm and the standard library. It does not import
// internal/ollama or internal/bedrock, it contains the strings "ollama" and "bedrock"
// nowhere outside a comment, and internal/architecture_test.go fails the build if that
// stops being true.
//
// It is handed a `map[string]llm.Provider` by internal/providers — still the only package
// in the repository that knows the catalogue — and it routes between whatever it is given.
// So the answer to "how do I add Amazon Nova, or Mistral, or an OpenAI provider?" is: write
// a thing that implements llm.Provider, and add one case to internal/providers. The router
// is not in the list of files you touch, and neither is anything else.
//
// That is the claim this milestone is really making, and it is cheap to make and expensive
// to be wrong about, so it is a test rather than a paragraph.
//
// # The two things that must never fail over
//
// A router's failure mode is not "it picked the wrong provider". It is that a *retry*
// happens in a place where retrying is not safe — and Milestone 9 built two such places
// while nobody was looking:
//
//   - **A stream that has emitted a token.** The caller has half an answer. Falling over
//     sends them a second beginning, silently concatenated onto the first.
//   - **A conversation in which a tool has run.** The world has moved. An n8n workflow is
//     running; a pull request exists. "Try Bedrock instead" means doing it again.
//
// And Milestone 9 built a third that is unique to routing, which is the one that would have
// shipped: **a tool-using conversation cannot change provider mid-flight at all**, even
// between turns, even when nothing has failed. See [continuation].
//
// So the router refuses to fall over in all three cases, and it refuses *structurally* —
// see [Router.Stream] and [continuation] — rather than by trusting a provider to have
// returned the right error.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// Name is what the router calls itself in a log. Every line [llm.Service] writes will say
// `provider=router`, which is why [llm.Response.Provider] exists to say who actually
// answered.
const Name = "router"

// Router selects a provider per request and calls it. It implements [llm.Provider].
type Router struct {
	providers map[string]llm.Provider
	order     []string // the configured preference order; the map has no order and this must
	cfg       Config
	strategy  Strategy
	health    *Health
	log       *slog.Logger
	now       func() time.Time
}

// Option customises a Router.
type Option func(*Router)

// WithClock replaces the clock. Tests use it; nothing else should.
func WithClock(now func() time.Time) Option {
	return func(r *Router) { r.now = now }
}

// New builds a router over providers that have already been built.
//
// It takes an interface map, not a configuration: constructing an Ollama client is
// internal/providers' job, and a router that could build one would be a router that knew
// what one was.
func New(providers map[string]llm.Provider, cfg Config, log *slog.Logger, opts ...Option) (*Router, error) {
	strategy, err := NewStrategy(cfg)
	if err != nil {
		return nil, err
	}

	order := make([]string, 0, len(cfg.Providers))
	for _, name := range cfg.Providers {
		provider, built := providers[name]
		if !built || provider == nil {
			return nil, fmt.Errorf("%w: %s enables %q but no such provider was built",
				ErrConfig, EnvProviders, name)
		}
		order = append(order, name)
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("%w: a router with no providers", ErrConfig)
	}

	r := &Router{
		providers: providers,
		order:     order,
		cfg:       cfg,
		strategy:  strategy,
		log:       log,
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	r.health = NewHealth(cfg.HealthThreshold, cfg.HealthCooldown, r.now)

	log.Info("router built",
		"providers", order,
		"strategy", strategy.Name(),
		"default", cfg.Default,
		"fallback", cfg.Fallback,
		"rules", len(cfg.Rules),
	)
	return r, nil
}

// Name identifies the router. See [Name].
func (r *Router) Name() string { return Name }

// Providers returns the enabled providers in preference order.
func (r *Router) Providers() []string { return append([]string(nil), r.order...) }

// Config returns the routing table.
func (r *Router) Config() Config { return r.cfg }

// Snapshot reports what real traffic has learned about each provider, without calling
// anything. [Router.Check] is the active version.
func (r *Router) Snapshot() []Status { return r.health.Snapshot(r.order) }

// Capabilities describes the fleet — and the rule for combining several providers into one
// answer is not "merge them". It is:
//
//	a CAPABILITY is a union       — the router can do it if ANY provider can
//	a GUARANTEE   is an intersection — the router promises it only if EVERY provider does
//
// Getting that backwards is a security bug, not a rounding error, so it is worth being
// concrete about which fields are which.
//
// Tools, Reasoning, StructuredOutput, Streaming and MaxContextTokens are capabilities. If
// Bedrock can use tools, the router can use tools — it will route a tool request there —
// and reporting the intersection would mean that enabling a small local model *removed* the
// platform's ability to use tools, which is an absurd thing for adding a provider to do.
//
// [llm.Capabilities.Local] is a guarantee, and it is the opposite. It means *the prompt
// does not leave the network*. A router holding Ollama and Bedrock cannot promise that,
// because it might well send this one to Bedrock — so it is true only when every enabled
// provider is local. A router that reported Local because one of its providers happened to
// be would be a router that lied about the only thing anybody was relying on it for.
func (r *Router) Capabilities() llm.Capabilities {
	caps := llm.Capabilities{Local: true} // a guarantee: AND-ed away by the first hosted provider

	for _, name := range r.order {
		c := r.providers[name].Capabilities()

		caps.Local = caps.Local && c.Local

		caps.Streaming = caps.Streaming || c.Streaming
		caps.Tools = caps.Tools || c.Tools
		caps.StructuredOutput = caps.StructuredOutput || c.StructuredOutput
		caps.Reasoning = caps.Reasoning || c.Reasoning

		if c.MaxContextTokens > caps.MaxContextTokens {
			caps.MaxContextTokens = c.MaxContextTokens
		}

		// Cost is the awkward one, because the honest answer — "it depends where this
		// request goes" — is not a number. So report the WORST price in the fleet.
		//
		// It is used by the tool loop's cost budget (LoopPolicy.MaxCostUSD), and there the
		// two ways of being wrong are not symmetric: over-estimating stops a conversation
		// early, and under-estimating lets it run past a budget somebody set precisely
		// because they did not want to find out what it cost. Pessimism is the safe
		// direction, exactly as it is for llm.CharsPerToken.
		if c.CostPer1MInputTokensUSD > caps.CostPer1MInputTokensUSD {
			caps.CostPer1MInputTokensUSD = c.CostPer1MInputTokensUSD
		}
		if c.CostPer1MOutputTokensUSD > caps.CostPer1MOutputTokensUSD {
			caps.CostPer1MOutputTokensUSD = c.CostPer1MOutputTokensUSD
		}
	}
	return caps
}

// Models lists every model on every provider, each tagged with where it lives.
//
// A provider that cannot be reached is logged and SKIPPED, not fatal. This is an inventory
// call, and "Bedrock is down, so I will not tell you what is on the GPU you are paying for"
// is not a useful way to answer it. It fails only when nothing at all responded — at which
// point the fleet really is down and saying so is the correct answer.
func (r *Router) Models(ctx context.Context) ([]llm.Model, error) {
	var (
		all    []llm.Model
		failed []string
		last   error
	)

	for _, name := range r.order {
		models, err := r.providers[name].Models(ctx)
		if err != nil {
			r.log.Warn("a provider could not be listed; skipping it",
				"provider", name, "error", err, "errorKind", llm.Kind(err))
			failed = append(failed, name)
			last = err
			if unhealthy(err) {
				r.health.Failed(name)
			}
			continue
		}
		r.health.Succeeded(name)

		for _, m := range models {
			m.Provider = name // the router is the only thing that knows, and the only thing that can say
			all = append(all, m)
		}
	}

	if len(failed) == len(r.order) {
		return nil, fmt.Errorf("no provider could be reached (%s): %w", strings.Join(failed, ", "), last)
	}
	return all, nil
}

// Generate routes one inference and returns the whole completion.
func (r *Router) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	plan, err := r.plan(req, false)
	if err != nil {
		return llm.Response{}, err
	}

	// Generate is atomic from the caller's point of view: nothing has been handed back, so
	// a failed attempt has left no trace and the next provider may safely start from the
	// top. Stream is the one where that stops being true.
	safe := func() bool { return true }

	return r.execute(req, plan, safe, func(p llm.Provider) (llm.Response, error) {
		return p.Generate(ctx, req)
	})
}

// Stream routes one inference, handing chunks to the sink as they arrive.
//
// # The guard, and why it does not trust the provider
//
// A stream may be failed over only if the caller has seen NOTHING. Once a token has
// escaped, the caller holds the beginning of an answer, and a second provider would hand
// them the beginning of a different one — two half-answers, concatenated, with no error
// anywhere and no way for the caller to know.
//
// [llm.ErrStreamBroken] exists to say exactly this, and a correct provider returns it. The
// router does not rely on that. It wraps the sink and counts what actually reached the
// caller, and it will not fail over if the count is non-zero **whatever error came back** —
// because the thing on the other side of this decision is a silently corrupted answer, and
// "the provider will tell us" is not a good enough reason to accept that risk from code we
// might not have written. A new provider added in a year is one forgotten error wrapping
// away from this bug, and the guard is three lines.
func (r *Router) Stream(ctx context.Context, req llm.Request, sink llm.Sink) (llm.Response, error) {
	plan, err := r.plan(req, true)
	if err != nil {
		return llm.Response{}, err
	}

	delivered := 0
	counting := func(c llm.Chunk) error {
		if c.Content != "" {
			delivered++
		}
		return sink(c)
	}
	safe := func() bool { return delivered == 0 }

	return r.execute(req, plan, safe, func(p llm.Provider) (llm.Response, error) {
		return p.Stream(ctx, req, counting)
	})
}

// plan is the routing decision: everything that happens before a provider is called.
//
// It is pure, apart from reading the health state — no network, no model, no tokens. So the
// question "why did this request go there?" is answered by a function you can call in a
// test with a struct literal, which is the difference between a routing layer you can
// reason about and one you can only observe.
type plan struct {
	chain    []string // in the order they will be tried; each name appears at most once
	decision Decision
	pinned   bool   // a pin does not fall over, whatever Config.Fallback says
	pinnedBy string // why: "override" or "conversation"
	excluded []string
}

func (r *Router) plan(req llm.Request, streaming bool) (plan, error) {
	log := r.requestLog(req)

	// 1. CONSTRAIN. Before any preference is consulted.
	eligible, excluded := r.eligible(req, streaming)
	if len(eligible) == 0 {
		err := r.refuse(req, excluded)
		log.Error("no provider can serve this request",
			"error", err, "errorKind", llm.Kind(err), "excluded", reasons(excluded))
		return plan{}, err
	}

	// 2a. A PIN beats every strategy. An override that could be overridden is not one.
	if name := strings.ToLower(strings.TrimSpace(req.Provider)); name != "" {
		if _, enabled := r.providers[name]; !enabled {
			err := fmt.Errorf("%w: the request asks for provider %q, which is not enabled (%s = %s)",
				llm.ErrInvalidRequest, name, EnvProviders, strings.Join(r.order, ", "))
			log.Error("the request pinned an unknown provider", "error", err, "errorKind", llm.Kind(err))
			return plan{}, err
		}
		if !eligibleContains(eligible, name) {
			// The caller named a provider that cannot do the job. Refuse — do NOT quietly
			// route it elsewhere. Someone who names a provider has a reason, and the reason
			// is often that the other one is not allowed to see this prompt.
			err := fmt.Errorf("%w: the request asks for provider %q, which cannot serve it: %s",
				llm.ErrNoProvider, name, reason(excluded, name))
			log.Error("the pinned provider cannot serve this request",
				"error", err, "errorKind", llm.Kind(err), "provider", name)
			return plan{}, err
		}
		return plan{
			chain:    []string{name},
			decision: Decision{Provider: name, Reason: "the request pinned this provider"},
			pinned:   true,
			pinnedBy: "override",
			excluded: reasons(excluded),
		}, nil
	}

	// 2b. A CONTINUATION is pinned too — see [continuation]. It is the subtlest rule here.
	decision := r.strategy.Select(req, eligible)

	if continuation(req) {
		return plan{
			chain:    []string{decision.Provider},
			decision: decision,
			pinned:   true,
			pinnedBy: "conversation",
			excluded: reasons(excluded),
		}, nil
	}

	// 3. ORDER. The chosen provider, then whoever else could take over.
	return plan{
		chain:    r.chain(decision.Provider, eligible),
		decision: decision,
		excluded: reasons(excluded),
	}, nil
}

// exclusion is a provider that cannot serve a request, and why.
//
// The "why" is the whole point. "No provider can serve this request" is a maddening error
// to receive; "ollama: has no tool support; bedrock: the prompt does not fit" is one you
// can act on without reading the router's source.
type exclusion struct {
	provider string
	reason   string
}

// eligible applies the constraints. It is the gate, and it runs before anything else.
//
// Each of these is here because the alternative failure is silent. That is the thread
// running through the whole package, and it is inherited: [llm.ErrContextExceeded] and
// [llm.ErrUnsupported] both exist because a model asked to do something it cannot does not
// refuse — it produces a confident, plausible, wrong answer. A router multiplies that
// hazard, because it is choosing on the caller's behalf and the caller cannot see what it
// chose. So it refuses on their behalf too.
func (r *Router) eligible(req llm.Request, streaming bool) ([]Candidate, []exclusion) {
	var (
		ok  []Candidate
		out []exclusion
	)

	// Estimated once, not per provider. Pessimistic on purpose — see llm.CharsPerToken.
	estimated := len(req.Input()) / llm.CharsPerToken

	for _, name := range r.order {
		caps := r.providers[name].Capabilities()

		switch {
		// THE CONSTRAINT. First, so that it is visibly not one consideration among several.
		//
		// A request that says the prompt must not leave the network is not expressing a
		// preference about latency or price, and there is no configuration setting, routing
		// strategy or outage that may overrule it. If this empties the candidate list, the
		// request is refused, and being refused is the correct outcome: the alternative is
		// somebody's private source code in a third party's service because a GPU was busy.
		case req.RequireLocal && !caps.Local:
			out = append(out, exclusion{name, "the request requires local inference and this provider is hosted (the prompt would leave the network)"})

		case len(req.Tools) > 0 && !caps.Tools:
			out = append(out, exclusion{name, "the request has tools and this provider cannot call them (it would ignore them and answer from memory)"})

		case req.Reasoning != nil && !caps.Reasoning:
			out = append(out, exclusion{name, "the request asks for extended reasoning and this provider does not support it"})

		case streaming && !caps.Streaming:
			out = append(out, exclusion{name, "the request is streaming and this provider cannot stream"})

		case caps.MaxContextTokens > 0 && estimated > caps.MaxContextTokens:
			// Not a preference either. A prompt that does not fit is not served slowly by
			// this provider; it is served WRONG — the beginning is dropped and the model
			// answers confidently from what is left.
			//
			// Note what this quietly buys: a 100k-token prompt is routed to the provider
			// with the window to hold it, automatically, because the other one is not
			// eligible. That is context-aware routing, and it falls out of the constraint
			// gate rather than being a feature anybody had to build.
			out = append(out, exclusion{name, fmt.Sprintf(
				"the prompt is ~%d tokens and this provider's window is %d", estimated, caps.MaxContextTokens)})

		default:
			ok = append(ok, Candidate{
				Name:         name,
				Capabilities: caps,
				Healthy:      r.health.peek(name),
			})
		}
	}
	return ok, out
}

// refuse builds the error for a request nothing can serve, and it names every provider and
// every reason.
//
// It wraps [llm.ErrNoProvider] — which is NOT [llm.ErrUnavailable], and the distinction is
// the one that gets somebody out of bed for the right reason. Nothing is broken. The
// platform is being asked for something it has not been configured to do, and it will keep
// being unable to do it until a human changes an environment variable. A retry loop that
// mistook this for an outage would ask a fleet of providers the same impossible question
// forever.
func (r *Router) refuse(req llm.Request, excluded []exclusion) error {
	var b strings.Builder
	for i, e := range excluded {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %s", e.provider, e.reason)
	}

	// The one worth spelling out, because the fix is not obvious and the temptation to
	// "fix" it by removing the constraint is considerable.
	if req.RequireLocal {
		return fmt.Errorf("%w: this request requires LOCAL inference and no enabled provider is "+
			"local (%s). It has been refused rather than sent to a hosted provider — that is the "+
			"point of the constraint, and an outage is not a reason to relax it. Enable a local "+
			"provider (%s), or, if this prompt genuinely may leave the network, do not set it",
			llm.ErrNoProvider, b.String(), EnvProviders)
	}

	return fmt.Errorf("%w: none of the enabled providers can serve it (%s). This is a "+
		"configuration problem, not an outage — nothing here will start working on its own",
		llm.ErrNoProvider, b.String())
}

// chain is the order the providers will be tried in.
//
// # It cannot loop, structurally
//
// The brief asks for fallback that avoids infinite loops. Rather than a depth counter or a
// visited set — both of which are guards you can forget to check — the chain is built as a
// SUBSET of the enabled providers, each appearing exactly once, and the executor walks it
// forwards and stops. There is nowhere for a loop to be, and no invariant anybody has to
// remember to maintain. (ConfigFromEnv rejects a duplicate in LLM_ROUTER_PROVIDERS for this
// reason and no other.)
//
// # Health reorders it, and that is not the same as falling back
//
// A provider that is currently failing goes BEHIND one that is not, even if the strategy
// preferred it. Nothing has failed yet when that happens — it is not a fallback, it is a
// demotion, and it is the entire reason [Health] exists: without it the router pays the
// primary's full timeout on every request to rediscover an outage it already knows about.
//
// The demoted provider stays in the chain. See [Health] on why a circuit breaker that
// removes providers is a circuit breaker that can take the platform down by itself.
func (r *Router) chain(chosen string, eligible []Candidate) []string {
	if !r.cfg.Fallback {
		// Fallback disabled: you get what you asked for, including its timeout. This is a
		// legitimate thing to want — a cost-controlled platform may prefer a clean failure
		// to a silent and more expensive success.
		return []string{chosen}
	}

	var healthy, unhealthy []string
	chosenHealthy := false

	for _, c := range eligible {
		if c.Name == chosen {
			chosenHealthy = c.Healthy
			continue
		}
		if c.Healthy {
			healthy = append(healthy, c.Name)
		} else {
			unhealthy = append(unhealthy, c.Name)
		}
	}

	chain := make([]string, 0, len(eligible))
	if chosenHealthy || len(healthy) == 0 {
		chain = append(chain, chosen)
		chain = append(chain, healthy...)
	} else {
		// The strategy's pick is known to be failing and something else is not. Try the
		// working one first — but keep the pick in the chain, behind it.
		chain = append(chain, healthy...)
		chain = append(chain, chosen)
	}
	return append(chain, unhealthy...)
}

// execute walks the chain.
//
// `safe` reports whether it is still safe to move to the next provider. For Generate it is
// always true. For Stream it is false the instant a token has reached the caller, and that
// is checked BEFORE the error is classified, because no error is a good enough reason to
// send someone a second beginning.
func (r *Router) execute(
	req llm.Request,
	p plan,
	safe func() bool,
	call func(llm.Provider) (llm.Response, error),
) (llm.Response, error) {
	log := r.requestLog(req).With(
		"provider", p.decision.Provider,
		"reason", p.decision.Reason,
		"chain", p.chain,
	)
	if p.pinned {
		log = log.With("pinned", p.pinnedBy)
	}
	log.Info("route selected",
		"fallbackEnabled", r.cfg.Fallback && !p.pinned,
		"excluded", p.excluded,
	)

	var (
		attempted []string
		last      error
	)

	for i, name := range p.chain {
		provider := r.providers[name]

		start := r.now()
		res, err := call(provider)
		elapsed := r.now().Sub(start)

		attempted = append(attempted, name)

		// Whoever answered — or failed to — is stamped on the response, including on the
		// error path. A failed inference whose log line cannot say WHICH provider failed is
		// a log line that cannot be alerted on.
		res.Provider = name

		if err == nil {
			r.health.Succeeded(name)
			log.Info("route completed",
				"servedBy", name,
				"model", res.Model,
				"durationMs", elapsed.Milliseconds(),
				"attempts", res.Attempts,
				"fallback", len(attempted) > 1,
				"attempted", attempted,
			)
			return res, nil
		}

		switch {
		case errors.Is(err, context.Canceled):
			// The caller gave up. That says nothing about the provider either way, and
			// recording it would let a burst of impatient users take one out of rotation.
		case unhealthy(err):
			r.health.Failed(name)
		default:
			// It responded. It is up. The only thing we learned is that models are models —
			// or that we sent it a prompt it could not use.
			r.health.Succeeded(name)
		}

		failOver := canFailOver(err)
		last = err
		lastProvider := i == len(p.chain)-1

		switch {
		case !safe():
			// THE GUARD. Tokens have already reached the caller. Whatever went wrong, a
			// second provider now would hand them the beginning of a different answer,
			// concatenated onto the first, and nothing downstream would ever know.
			log.Error("the stream broke after output had already been sent; NOT falling over",
				"provider", name, "error", err, "errorKind", llm.Kind(err),
				"durationMs", elapsed.Milliseconds())
			return res, fmt.Errorf("%w: %s had already sent output: %w", llm.ErrStreamBroken, name, err)

		case p.pinned:
			log.Error("the route is pinned; NOT falling over",
				"provider", name, "pinned", p.pinnedBy, "error", err, "errorKind", llm.Kind(err),
				"durationMs", elapsed.Milliseconds())
			return res, r.pinnedError(p, name, err)

		case !r.cfg.Fallback:
			log.Error("the provider failed and fallback is disabled",
				"provider", name, "error", err, "errorKind", llm.Kind(err),
				"durationMs", elapsed.Milliseconds())
			return res, err

		case !failOver:
			// Deterministic. Asking a different model the same malformed question gets the
			// same answer more expensively.
			log.Error("the provider answered and the request was the problem; NOT falling over",
				"provider", name, "error", err, "errorKind", llm.Kind(err),
				"durationMs", elapsed.Milliseconds())
			return res, err

		case lastProvider:
			log.Error("every provider failed",
				"provider", name, "error", err, "errorKind", llm.Kind(err),
				"attempted", attempted, "durationMs", elapsed.Milliseconds())
			return res, fmt.Errorf("every provider failed (tried %s): %w",
				strings.Join(attempted, ", "), err)

		default:
			log.Warn("the provider failed; falling over to the next one",
				"provider", name,
				"next", p.chain[i+1],
				"error", err,
				"errorKind", llm.Kind(err),
				"durationMs", elapsed.Milliseconds(),
			)
		}
	}

	// Unreachable: the loop returns on the last provider. Present because "unreachable" is
	// a claim, and a nil response with a nil error would be a nil dereference upstairs.
	return llm.Response{}, fmt.Errorf("%w: the chain was empty: %w", llm.ErrNoProvider, last)
}

// pinnedError explains a failure that was NOT failed over, and says why it was not.
//
// The error has to say this. A caller looking at "bedrock is unavailable" on a platform that
// visibly has a working Ollama will reasonably conclude the fallback is broken — and the
// answer is that fallback was declined on purpose, for a reason they chose.
func (r *Router) pinnedError(p plan, name string, err error) error {
	switch p.pinnedBy {
	case "conversation":
		return fmt.Errorf("%s failed mid-conversation and the router did not fall over: a "+
			"tool-using conversation carries provider-specific state (tool-call IDs, and Claude's "+
			"signed reasoning blocks) that another provider cannot read. Retry the conversation "+
			"from the beginning — if no Write tool has run, that is safe: %w", name, err)
	default:
		return fmt.Errorf("%s failed and the request had pinned it, so the router did not fall "+
			"over (drop Request.Provider to allow routing): %w", name, err)
	}
}

// continuation reports whether a request carries state that BELONGS to one provider.
//
// # The bug this prevents, which would have been extremely hard to find
//
// [llm.Service.Converse] runs a tool loop by calling Generate once per turn, replaying the
// whole conversation each time. From the router's point of view those turns are separate,
// independent requests. Nothing in the type system connects them. So the obvious router —
// route each request, fall over when one fails — will happily send turn 1 to Bedrock and
// turn 4 to Ollama.
//
// That is not a slightly-degraded answer. It is incoherent, in three separate ways:
//
//   - **Claude's reasoning is signed.** [llm.ReasoningBlock] carries an opaque,
//     provider-issued Signature which Bedrock demands back, verbatim, on the next turn.
//     Ollama has never seen it, cannot verify it, and will not produce one — so the turn
//     after the switch has no valid reasoning to replay and the conversation cannot be
//     continued at all.
//   - **Tool-call IDs are the provider's.** The results we hand back are keyed to IDs the
//     first provider invented. To the second they are references to calls it never made.
//   - **The models are different models.** Half a chain of thought from a frontier model,
//     handed to a 7B local one to finish, produces something that reads like reasoning and
//     is not.
//
// So a conversation is pinned to whichever provider the strategy picks, health is ignored
// for it, and it does not fall over. If that provider dies mid-conversation, the request
// FAILS — and failing is right: the conversation's state cannot migrate, so there is nothing
// to fail over TO. The caller retries from the top, which is safe exactly when no Write tool
// has run, and [llm.ErrEffectsCommitted] is how Milestone 9 already tells them which case
// they are in.
//
// # Why it is detected rather than declared
//
// The alternative was a `Request.ConversationID`, set by the tool loop. That would mean
// internal/llm — which must not know that routing exists — carrying a field whose only
// purpose is to inform a router. Detection needs nothing: a request whose history contains
// an assistant turn with tool calls or reasoning IS a continuation, and it is one whether or
// not anybody remembered to say so. A rule that cannot be forgotten beats a field that can.
func continuation(req llm.Request) bool {
	for _, m := range req.Messages {
		if m.Role != llm.RoleAssistant {
			continue
		}
		if len(m.ToolCalls) > 0 || m.Reasoning != nil {
			return true
		}
	}
	return false
}

// canFailOver reports whether another provider may be given the same request.
//
//	YES — this provider did not do its job. Somebody else might.
//	  unavailable · timeout · throttled · stalled · retries exhausted
//	  unauthorized · model access denied · model not found
//
//	NO — this provider did its job and the news was bad. Nobody else will do better.
//	  invalid request · context exceeded · unsupported · schema violation
//	  empty completion · invalid response · tool loop · effects committed · cancelled
//
// # The two judgement calls
//
// **Unauthorized and access-denied fail over**, and that is arguable. They are
// misconfigurations: a missing IAM policy, or the Bedrock per-model access grant nobody
// remembers to request. Failing over means the platform keeps working and the mistake hides.
//
// It is still right. The alternative is a platform that goes down — with a perfectly good
// local model sitting idle — because somebody has not clicked a button in the Bedrock
// console. Availability wins, and the mistake is not allowed to hide: these are logged at
// ERROR, they trip the health breaker, and `llm route` shows the provider as unhealthy with
// the reason attached. Fallback buys time; it is not a fix, and nothing here pretends it is.
//
// **A cancelled context is not a failure of anything.** The user pressed Ctrl-C, or n8n gave
// up. Failing over would start a fresh inference against a context that is already dead.
func canFailOver(err error) bool {
	if err == nil {
		return false
	}

	// Before everything: a cancellation is the caller's decision, not the provider's failure.
	// It must be checked first because a cancelled request often surfaces AS a timeout.
	if errors.Is(err, context.Canceled) {
		return false
	}

	switch {
	case errors.Is(err, llm.ErrEffectsCommitted), errors.Is(err, llm.ErrStreamBroken):
		// Terminal, both of them, and the executor guards these separately. Listed here so
		// that this function is a complete statement of the rule rather than half of one.
		return false

	case errors.Is(err, llm.ErrUnavailable),
		errors.Is(err, llm.ErrTimeout),
		errors.Is(err, llm.ErrThrottled),
		errors.Is(err, llm.ErrStalled),
		errors.Is(err, llm.ErrUnauthorized),
		errors.Is(err, llm.ErrModelAccessDenied),
		errors.Is(err, llm.ErrModelNotFound),
		errors.Is(err, llm.ErrRetriesExhausted):
		return true

	default:
		// Everything else is the request's fault or the model's: invalid, too big,
		// unsupported, schema-violating, empty. A second provider produces the same outcome
		// and a second bill.
		return false
	}
}

// unhealthy reports whether a failure says the PROVIDER is broken.
//
// It is *nearly* the same question as [canFailOver], and the one place the two answers
// diverge is the reason they are two functions.
//
// [llm.ErrModelNotFound] means "this provider does not have that model". It should fail over
// — a request naming a Claude model, sent to Ollama, genuinely does belong at Bedrock, and
// routing it there is a small free win that falls out of the constraint gate. But it must
// NOT count against Ollama's health, because Ollama is perfectly well. It is running, it
// answered immediately, and it told us the truth.
//
// Conflating the two would mean that one request for a model Ollama does not have takes
// Ollama out of rotation for every OTHER request — a circuit breaker tripped by a
// misaddressed letter, demoting a provider that never failed at anything. It is exactly the
// kind of bug a router acquires by having one predicate where the domain has two questions.
func unhealthy(err error) bool {
	return canFailOver(err) && !errors.Is(err, llm.ErrModelNotFound)
}

func (r *Router) requestLog(req llm.Request) *slog.Logger {
	return r.log.With(
		"correlationId", req.CorrelationID,
		"workflowExecutionId", req.WorkflowExecutionID,
		"purpose", string(req.Purpose),
		"strategy", r.strategy.Name(),
	)
}

func eligibleContains(candidates []Candidate, name string) bool {
	for _, c := range candidates {
		if c.Name == name {
			return true
		}
	}
	return false
}

// reason finds why one provider was excluded.
func reason(excluded []exclusion, name string) string {
	for _, e := range excluded {
		if e.provider == name {
			return e.reason
		}
	}
	return "unknown"
}

// reasons renders the exclusions for a log line, sorted so that two identical routing
// decisions produce two identical log lines — which is what makes them countable.
func reasons(excluded []exclusion) []string {
	out := make([]string, 0, len(excluded))
	for _, e := range excluded {
		out = append(out, e.provider+": "+e.reason)
	}
	sort.Strings(out)
	return out
}
