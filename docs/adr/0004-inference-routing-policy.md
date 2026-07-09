# ADR-0004: Route inference by latency tolerance, with Bedrock as the backstop

**Status:** Accepted
**Date:** 2026-07-09

## Context

The platform has two inference providers with opposite cost curves and opposite latency behaviour.

|  | Bedrock | Ollama on Spot GPU |
|---|---|---|
| Idle cost | **Zero** | ~$250/mo per instance if always on |
| Marginal cost per token | Metered, meaningful | ~Zero once the GPU is running |
| Cold start | **None** | **2–4 minutes** (GPU launch + weights) |
| Capacity risk | Throttling | **Spot pool exhaustion** |
| Data residency | Leaves the account (stays in AWS) | Stays in the account |

Given [ADR-0003](0003-model-gateway-seam.md)'s chokepoint, the question is what policy it should apply. "Prefer the self-hosted one because it's cheaper" is the intuitive answer and it is wrong in both directions: self-hosted is *not* cheaper at low volume (idle GPU cost dominates), and it *cannot* serve interactive traffic (cold start).

## Decision

Route on **latency tolerance** first, **data residency** second, and cost third.

```
interactive / low-latency / spiky   ->  Bedrock        (no cold start, zero idle cost)
bulk / async / sustained            ->  Ollama on Spot (marginal token cost ~0 at volume)
data must not leave the account     ->  Ollama         (no fallback permitted)
Ollama capacity unavailable         ->  Bedrock        (fallback)
classification / extraction / routing -> Bedrock, small fast model
```

Bulk requests are buffered in **SQS** so the GPU cold start is absorbed by the queue rather than experienced by a caller ([04 §4.4](../architecture/04-flows.md)).

**Bedrock is the default and the backstop.** Ollama is opt-in per request class.

The policy lives as **data in SSM Parameter Store**, not code, so routing changes are operational rather than architectural.

## Consequences

**Positive**

- **Correct by construction on the latency axis.** Interactive traffic never waits for a GPU to boot, because it never touches one.
- **Spot GPU capacity failure degrades cost, not availability.** The fallback rule is what makes [ADR-0005](0005-spot-only-for-stateless-workloads.md) and [ADR-0012](0012-scale-inference-to-zero.md) safe for production. Remove this rule and running inference on Spot becomes indefensible.
- Scale-to-zero is viable, because nothing latency-sensitive depends on a warm GPU.
- Routing is tunable at runtime. The Bedrock/Ollama split is a dial, not a rebuild.
- The `data_residency: in-account` rule is expressed **without a fallback**, so a capacity shortfall fails the request rather than silently violating the policy. Failing closed is the only correct behaviour for a residency constraint, and encoding it in the routing table makes that explicit rather than implicit.

**Negative**

- Callers must classify their requests (`latency_class`, `data_residency`). A misclassified interactive request lands in a queue behind a cold GPU and times out. This is a real footgun; the default must be `interactive`, so that forgetting to classify is safe.
- Two providers means two sets of model behaviours. The same prompt yields different results on a frontier Bedrock model and an open-weight local model. **Fallback can silently change output quality** — the request succeeds, the answer is worse. Emit a metric on every fallback; do not let it be invisible.
- The cost crossover (§9.4) is assumed, not measured. If bulk volume never materialises, the Ollama fleet is pure cost and Ollama should be justified on residency grounds or removed.
- Policy-as-data means a bad SSM parameter is a production incident with no code review. Version it; validate it in CI.

## Alternatives considered

**Cost-based routing — always take the cheapest provider per request.** Rejected: cheapest-per-token ignores idle GPU cost and cold start. It would route a single interactive request to a scaled-to-zero GPU fleet and make the user wait four minutes to save a fraction of a cent.

**Ollama-first, Bedrock only on failure.** Rejected: makes the common interactive path the cold path. Also maximises GPU hours, which is the expensive resource.

**Bedrock-only.** Simpler, and genuinely the right answer if bulk volume and residency requirements never appear. Rejected because the brief requires self-hosted support — but note [ADR-0003](0003-model-gateway-seam.md) makes this reversible as a config change, not a rewrite. Keep measuring; if Ollama's utilisation stays low, remove it.

**Route by model capability (small tasks → small models) as the primary axis.** Not an alternative — a *complement*, and the largest single cost lever available ([09 §9.4](../architecture/09-cost.md)). Included in the policy as the `classification` rule. It is secondary here only because it does not answer the provider question, which is what this ADR is about.
