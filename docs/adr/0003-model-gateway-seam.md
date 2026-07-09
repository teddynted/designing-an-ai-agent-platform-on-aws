# ADR-0003: Introduce a Model Gateway seam with an OpenAI-compatible contract

**Status:** Accepted
**Date:** 2026-07-09

## Context

The brief requires two things that pull in the same direction:

- Support **both managed (Bedrock) and self-hosted (Ollama)** inference.
- Allow **future AI providers to be added with minimal architectural changes**.

The obvious implementation is for each caller to use the provider's SDK directly: n8n has native Bedrock nodes, OpenClaw takes a provider API key. This works on day one and is free.

It fails on day two hundred. Provider choice becomes embedded in every workflow, every agent config, and every prompt. Adding a provider means editing all of them. Worse, three platform-level concerns have nowhere to live:

- **Cost governance.** An autonomous agent in a retry loop is a financial incident ([09 — Cost](../architecture/09-cost.md)). Enforcing a per-agent token budget requires a chokepoint. Without one, the budget must be re-implemented in every caller, which means it will be missing from one of them.
- **Fallback.** Ollama runs on Spot GPUs. Spot GPU capacity genuinely disappears. Without a shared fallback path, every caller implements its own, or the platform's availability becomes a function of GPU spot capacity.
- **Observability.** Answering "what did this agent run cost" requires token metering in one place.

## Decision

All inference goes through a **Model Gateway**: a stateless service exposing a single **OpenAI-compatible** `POST /v1/chat/completions` endpoint, fronting every provider.

It owns:
- **Routing policy** (data in SSM Parameter Store, not code) — see [ADR-0004](0004-inference-routing-policy.md)
- **Provider adapters** — Bedrock, Ollama, and future providers
- **Fallback** between providers
- **Per-agent token budgets and the circuit-breaker**
- **Token and cost metering**, dimensioned by `agent_run_id`

**The contract is defined in Milestone 1; the router is built later.** Until it exists, callers speak the same contract directly to Bedrock. Defining the seam early costs nothing; retrofitting it after forty workflows hold provider-specific configuration is a migration.

**Why OpenAI-compatible specifically:** Ollama already speaks it natively, Bedrock can be fronted by it, and essentially every LLM library and tool targets it. Choosing a *de facto* standard rather than a bespoke interface is what makes adapters cheap and makes the seam free to those who do not use it.

## Consequences

**Positive**

- **Adding a provider is an adapter plus a config change.** No workflow, agent, or prompt changes. This is the brief's extensibility requirement, satisfied concretely rather than aspirationally.
- **Bedrock becomes Ollama's availability backstop.** This is the decision's most valuable second-order effect: it converts a Spot GPU capacity shortfall from an *outage* into a *higher bill*. Without this seam, running production inference on Spot GPUs would be trading availability for cost — a bad trade. With it, [ADR-0005](0005-spot-only-for-stateless-workloads.md) and [ADR-0012](0012-scale-inference-to-zero.md) become safe.
- Per-agent budget enforcement has exactly one home, so it cannot be forgotten in one caller.
- Cost-per-outcome becomes measurable, which makes the Bedrock/Ollama crossover an empirical question rather than an argument.
- Model upgrades are a parameter edit, not a deploy.

**Negative**

- **A new single point of failure on the hot path.** Mitigated by running it stateless across two AZs, and by the fact that callers can speak the identical contract directly to Bedrock if it is down. Not eliminated. This is the real price.
- One extra network hop (single-digit ms, internal) on every inference call.
- One more component to build, deploy, monitor, and patch.
- The OpenAI contract is a **lowest common denominator**. Provider-specific capabilities — Bedrock Guardrails configuration, prompt caching hints, extended thinking parameters — do not map cleanly. Expect an escape hatch (pass-through fields), and expect it to leak provider specifics back into callers. This tension does not fully resolve.
- Streaming, tool-calling, and multimodal semantics differ across providers. Adapters absorb the difference, imperfectly.

## Alternatives considered

**Direct SDK calls from each caller.** Rejected: provider choice diffuses into every workflow; no chokepoint for budgets, fallback, or metering. Cheapest today, most expensive at the first provider change or the first runaway agent.

**LiteLLM or a comparable off-the-shelf proxy.** A strong option, and a likely *implementation* of this ADR rather than an alternative to it. This ADR decides that the seam exists and what contract it speaks. Whether to build the adapter layer or adopt LiteLLM is a Milestone 3 implementation decision, deliberately left open.

**Bedrock only; drop Ollama.** Simplest, and eliminates GPU cold starts, Spot capacity risk, and an entire fleet. Rejected because the brief requires self-hosted models, and because data-residency and high-volume-cost cases are real. But note: **if those cases do not materialise, this is the right architecture**, and the seam makes removing Ollama a config change rather than a rewrite. The seam earns its keep in both directions.

**Route in n8n with a workflow.** Rejected: OpenClaw is a caller too and does not execute n8n workflows. The chokepoint must sit below both runtimes, not inside one of them.
