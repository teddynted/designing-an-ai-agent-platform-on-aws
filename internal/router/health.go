package router

import (
	"context"
	"sync"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// Health tracks which providers are working.
//
// # Why this exists, which is not the reason you would guess
//
// Fallback works perfectly well without it. If Bedrock is down, the router calls Bedrock,
// Bedrock fails, the router calls Ollama, and the caller gets an answer. Nothing is
// incorrect. The platform is simply *unusable*, and the arithmetic is worth doing:
//
//	BEDROCK_TIMEOUT   = 2m       (the default)
//	BEDROCK_RETRY_ATTEMPTS = 3   (the default)
//
// A provider that is down does not fail fast. It fails slowly, three times, and only then
// does the router learn what it could have known from the previous request. Every single
// inference pays several minutes to rediscover an outage that is already several hours old
// — and it pays that *before* it starts the work, so a ten-second summarisation now takes
// six minutes. The fallback is functioning; the platform is down anyway.
//
// So Health is not a nicety bolted onto the side of the router. It is the thing that makes
// fallback *affordable*: it remembers, so that the cost of an outage is paid a couple of
// times rather than on every request until somebody notices.
//
// # Demoted, never removed
//
// An unhealthy provider is moved to the BACK of the chain. It is never taken out of it.
//
// That is a deliberate refusal of the obvious design, and it is there because of the way
// circuit breakers actually kill systems. A breaker that removes providers has a state —
// "everything is unhealthy" — in which it refuses all traffic. And that state is reachable
// from something as ordinary as a DNS blip that fails one request to each provider at the
// same moment: two failures, both breakers open, and now a router with two working models
// behind it is returning errors to everybody because it has decided, on the strength of two
// timeouts, that the world has ended.
//
// The failure mode of demotion is that a request occasionally goes to a provider that is
// still broken and is slow. The failure mode of removal is total outage caused by the
// component whose entire job is preventing one. Those are not comparable, so: demote.
//
// # It observes; it does not poll
//
// The state comes from real requests — the ones the platform was making anyway. There is no
// background goroutine calling Bedrock every ten seconds to ask if it is well: that costs
// money on an idle platform, tells you nothing about whether the request you are ABOUT to
// make will work, and is one more thing to leak. [Router.Check] exists for the moments when
// an active probe is genuinely what you want (start-up, a CLI, a health endpoint), and it
// is called by a human or a liveness check rather than by a timer.
type Health struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu    sync.Mutex
	state map[string]*state
}

type state struct {
	failures int
	downAt   time.Time
	// probing marks the single request allowed through during a half-open window, so that
	// a recovering provider is retried by ONE request rather than by all of them at once —
	// which would be a thundering herd aimed squarely at the thing that just came back up.
	probing bool
}

// NewHealth builds a tracker. A threshold below 1 means one failed request condemns a
// provider, which on a busy platform is a blip taking out the primary.
func NewHealth(threshold int, cooldown time.Duration, now func() time.Time) *Health {
	if threshold < 1 {
		threshold = DefaultHealthThreshold
	}
	if now == nil {
		now = time.Now
	}
	return &Health{
		threshold: threshold,
		cooldown:  cooldown,
		now:       now,
		state:     map[string]*state{},
	}
}

// Healthy reports whether a provider should be preferred. It is false only while the
// provider is inside its cooldown after enough consecutive failures.
//
// Calling it has a side effect, and it has to: the first caller after a cooldown expires is
// given permission to probe, and the rest are not. Without that, the moment the window
// opened every in-flight request would rush a provider that has been down for a minute.
func (h *Health) Healthy(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	st, known := h.state[name]
	if !known || st.failures < h.threshold {
		return true
	}

	if h.now().Sub(st.downAt) < h.cooldown {
		return false
	}

	// The cooldown has expired: half-open. Exactly one caller gets to find out whether the
	// provider is back, and it does so by carrying a real request — which is the only test
	// that actually proves anything.
	if st.probing {
		return false
	}
	st.probing = true
	return true
}

// Succeeded records that a provider answered. It clears the failure count outright rather
// than decrementing it: a provider that has just done the job is working, and making it
// serve N more requests to earn back its reputation would keep it demoted long after it
// recovered.
func (h *Health) Succeeded(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, name)
}

// Failed records a provider-level failure.
//
// Only the router calls this, and only for errors that are the PROVIDER's fault — see
// [providerAtFault]. A model that returned JSON not matching a schema is not an unhealthy
// provider; it is a language model being a language model, and taking Bedrock out of
// rotation because Claude miscounted a field would be a circuit breaker with an opinion
// about prompt engineering.
func (h *Health) Failed(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	st, known := h.state[name]
	if !known {
		st = &state{}
		h.state[name] = st
	}
	st.failures++
	st.probing = false

	// The clock starts at the failure that CROSSED the threshold, and is restarted by every
	// failure after it. A provider that keeps failing keeps its cooldown; it does not
	// quietly become eligible again because enough time has passed since the first one.
	if st.failures >= h.threshold {
		st.downAt = h.now()
	}
}

// peek reports health WITHOUT claiming the half-open probe slot.
//
// [Health.Healthy] has a side effect — it hands exactly one caller permission to test a
// recovering provider — so anything that merely wants to LOOK at the state must not call
// it. Asking a provider how it is doing must not consume the single request that was going
// to find out.
func (h *Health) peek(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recovered(name)
}

// recovered is the health rule itself. The caller holds the lock.
func (h *Health) recovered(name string) bool {
	st, known := h.state[name]
	if !known || st.failures < h.threshold {
		return true
	}
	return h.now().Sub(st.downAt) >= h.cooldown
}

// Status is what a provider looks like from the outside, for a CLI or a health endpoint.
type Status struct {
	Provider string
	Healthy  bool

	// Failures is the CONSECUTIVE count. It resets to zero on any success, so a non-zero
	// value means "failing right now", not "has ever failed".
	Failures int

	// Latency is how long an active probe took. Zero unless [Router.Check] ran one.
	Latency time.Duration

	// Error is why the probe failed. It is a string rather than an error because this is a
	// value to be printed and marshalled, not one to be matched on.
	Error string

	// Models is how many models the probe found. Zero and no error means a provider that is
	// up and has nothing on it — which is a real state, and a confusing one to hit for the
	// first time in production. It is what an Ollama that was never `ollama pull`ed looks
	// like.
	Models int
}

// Snapshot reports the passive state — what real traffic has learned — without calling
// anything.
func (h *Health) Snapshot(names []string) []Status {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]Status, 0, len(names))
	for _, name := range names {
		status := Status{Provider: name, Healthy: h.recovered(name)}
		if st, known := h.state[name]; known {
			status.Failures = st.failures
		}
		out = append(out, status)
	}
	return out
}

// Check actively probes every provider by listing its models, and reports what it found.
//
// This is the "basic health check" of the milestone, and listing models is the right probe
// for a precise reason: it is the cheapest call that exercises the whole path a real
// request uses — DNS, TLS, the endpoint, and, on Bedrock, IAM — WITHOUT generating a single
// token. A probe that ran an inference would work, cost money on every check, and be the
// sort of thing somebody eventually puts on a thirty-second timer.
//
// It does not prove the model will answer. Nothing short of asking it does, and asking it
// is what the next real request will do anyway.
func (r *Router) Check(ctx context.Context) []Status {
	out := make([]Status, 0, len(r.order))

	for _, name := range r.order {
		provider := r.providers[name]

		start := r.now()
		models, err := provider.Models(ctx)
		elapsed := r.now().Sub(start)

		// A probe reports what the probe SAW. Healthy is whether this call reached the
		// provider — not the circuit-breaker's verdict, which needs several consecutive
		// failures to trip and would report a just-refused connection as "healthy" simply
		// because it was only refused once. An operator running `llm route` is asking "is it
		// working right now?", and "it failed but not enough times yet" is not an answer they
		// can use.
		st := Status{Provider: name, Latency: elapsed, Models: len(models), Healthy: err == nil}
		if err != nil {
			st.Error = err.Error()
			// The probe still FEEDS the breaker, so that a failure observed here also demotes
			// the provider for the passive path — if the operator has just been told Bedrock
			// is unreachable, real traffic should not go there to find out again.
			if unhealthy(err) {
				r.health.Failed(name)
			}
		} else {
			r.health.Succeeded(name)
		}

		r.log.Info("provider health probe",
			"provider", name,
			"healthy", st.Healthy,
			"models", st.Models,
			"latencyMs", elapsed.Milliseconds(),
			"error", st.Error,
			"errorKind", llm.Kind(err),
		)
		out = append(out, st)
	}
	return out
}
