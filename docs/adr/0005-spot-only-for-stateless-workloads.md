# ADR-0005: Use Spot only for stateless, interruptible workloads

**Status:** Accepted
**Date:** 2026-07-09

## Context

The brief mandates EC2 Spot Instances and the minimisation of operational cost. Spot offers 60–90% off On-Demand pricing in exchange for one property: AWS may reclaim the instance with **two minutes' notice**.

Two minutes is a generous budget for a worker that can requeue its job. It is an impossible budget for a process holding a WhatsApp device-link, because restoring that link requires a human to scan a QR code.

The temptation — "Spot everywhere, we'll handle interruptions" — treats a two-minute drain as a universal solvent. It is not. Some state cannot be handed off in two minutes, or at all.

## Decision

**A workload may run on Spot if and only if it can be terminated with two minutes' notice and lose nothing that cannot be automatically reconstructed.**

Applied:

| Workload | Spot? | Why |
|---|---|---|
| n8n workers | **Yes** | Stateless. Job requeues; another worker retries |
| Ollama GPU inference | **Yes** | Stateless. Request retries, or falls back to Bedrock |
| Model Gateway | Yes (stateless) | Runs On-Demand/Fargate for latency stability, not correctness |
| n8n `main` | **No** | HTTP ingress point. A webhook already accepted cannot be un-accepted |
| **OpenClaw Gateway** | **No** | Holds channel device-links. Loss requires **manual re-pairing** |
| RDS / ElastiCache | N/A | Managed |

Spot fleets are configured with:

- **Mixed instances policy**, 6–10 instance types across ≥2 families and all AZs. A single-instance-type Spot ASG is a single point of capacity failure wearing an ASG costume.
- **`capacity-optimized-prioritized`** allocation, **not `lowest-price`**.
- **Capacity Rebalance enabled** — proactive replacement on the *rebalance recommendation*, which arrives earlier than the interruption warning.
- A `spot-drain` Lambda on the interruption warning: deregister from the target group, stop pulling work, requeue in-flight jobs ([04 §4.5](../architecture/04-flows.md)).

## Consequences

**Positive**

- 60–90% savings on the **majority of the platform's compute** — GPU inference and workflow execution — which is where the compute actually is. The three On-Demand instances are a small, fixed base.
- Interruption handling is genuinely automatic. Nothing on Spot needs a human when reclaimed.
- Instance-type and AZ diversification improves capacity availability as a side effect of pursuing cost.
- Forces workloads to be honest about their state. A component that "just needs a little local state" is a component that has not been designed.

**Negative**

- **Workflow steps must be idempotent or compensating.** A requeued job that already sent an email sends it twice. This is a constraint the platform *imposes on its tenants* rather than a property it provides — and it is enforced by convention, not by the runtime. If tenants cannot meet it (assumption A3, [12 — Risks](../architecture/12-risks-assumptions-constraints.md)), workers must move to On-Demand and costs rise materially.
- Long inference requests exceeding two minutes cannot checkpoint; they are simply retried or rerouted. Acceptable only because inference is stateless and idempotent.
- `capacity-optimized-prioritized` costs slightly more per hour than `lowest-price`. It buys fewer interruptions, which is cheaper overall once you count the wasted partial work — but the line item looks worse.
- GPU Spot pools are shallower than general-purpose pools. Capacity failure is a routine event, not an exceptional one, and the design must expect it.
- **This ADR is only safe because of [ADR-0004](0004-inference-routing-policy.md).** Bedrock is the fallback when Spot GPU capacity is unavailable. Without that backstop, Spot inference would trade availability for cost. The two decisions must be adopted or rejected together.

## Alternatives considered

**Spot everywhere, including the Gateway.** Rejected. The interruption path ends with a human scanning a QR code to re-pair WhatsApp. There is no amount of automation on the AWS side that fixes this, because the state lives in the chat platform's device registry. Two minutes is not the problem; the problem is that the recovery is not an AWS operation at all.

**On-Demand everywhere.** Rejected: pays a 3–5× premium on the GPU fleet — the platform's largest interruptible cost — for a guarantee that inference does not need. Fails the brief's cost principle for no availability benefit, given the Bedrock backstop.

**`lowest-price` allocation strategy.** Rejected, and worth naming explicitly because it looks obviously right. The cheapest Spot pool is frequently cheapest *because* it is about to be reclaimed. `lowest-price` therefore maximises interruption churn, and churn wastes partial work, re-pulls weights, and re-warms caches. `capacity-optimized-prioritized` targets the deepest pools while respecting a preference order.

**Spot Blocks / defined-duration Spot.** No longer available for new workloads, and would not help: it caps duration rather than removing interruption for stateful processes.

**EC2 hibernation on interruption for the Gateway.** Rejected: hibernation is not guaranteed to complete within the interruption window, does not work across all instance types, and does not survive the instance being reclaimed. It converts a certain failure into an unreliable one.
