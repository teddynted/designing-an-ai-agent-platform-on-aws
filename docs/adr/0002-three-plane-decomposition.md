# ADR-0002: Decompose the platform into control, agent, and inference planes

**Status:** Accepted
**Date:** 2026-07-09

## Context

The brief asks for a platform that is simultaneously **cost-minimising** (Spot instances, minimal operational cost), **production-ready** (highly available), and **stateful enough to run autonomous agents** (OpenClaw owns chat sessions and channel device-links; n8n owns workflow state).

Treated as a single workload, these requirements contradict each other. Spot instances are cheap because they can be reclaimed with two minutes' notice. Conversational sessions and device-linked chat channels cannot be handed off in two minutes — losing them means a human re-scanning a QR code. Any design that puts one compute model under the whole platform must therefore either pay On-Demand prices for GPU inference that would happily tolerate interruption, or run irreplaceable state on instances that will be reclaimed.

That is a false choice, and it comes from the wrong decomposition.

## Decision

Decompose the platform by **statefulness and interruption tolerance** into three planes, each with its own compute model:

| Plane | State | Interruptible | Compute |
|---|---|---|---|
| **Control** — how the platform manages itself | None | N/A | Lambda + EventBridge |
| **Agent** — where agents think and act | **Durable, some unrecoverable** | **No** | On-Demand EC2 (+ Spot for stateless workers) |
| **Inference** — where tokens are produced | None (weights are read-only artifacts) | **Yes** | **Spot** EC2 + Bedrock |

The governing rule: **Spot belongs where interruption is free. State belongs where it survives.**

The planes communicate over explicit contracts — EventBridge events, queue messages, and the Model Gateway's HTTP interface — never by sharing a host, a filesystem, or a database.

## Consequences

**Positive**

- Cost optimisation and high availability stop competing. The expensive-to-interrupt components (three instances) run On-Demand; the interruption-tolerant components (GPU inference, workflow workers — the bulk of the compute) run on Spot at 60–90% off.
- Each plane scales on its own axis. GPU capacity does not scale with conversation count.
- The inference plane can scale to **zero** because it holds nothing ([ADR-0012](0012-scale-inference-to-zero.md)).
- The control plane can heal the agent plane precisely because it does not live on it. A Lambda draining a dying Spot worker is not itself on that worker.
- Blast radii align with the planes, so the network and IAM boundaries fall out naturally ([05 — Boundaries](../architecture/05-network-and-boundaries.md)).

**Negative**

- Three deployment models, three failure models, three mental models. This is more to learn than "it all runs on EC2."
- Cross-plane calls add network hops and latency versus co-locating everything on one host.
- The boundaries must be enforced. The path of least resistance — running a quick script on the Gateway instead of dispatching it to a worker — erodes the decomposition one convenience at a time.
- Requires that the agent plane's *own* stateless parts (n8n workers) be separated from its stateful parts, which is why n8n must run in queue mode ([ADR-0008](0008-n8n-queue-mode-managed-datastores.md)). Without that, the agent plane could not use Spot at all.

## Alternatives considered

**One EC2 fleet running everything (n8n, OpenClaw, Ollama) on Spot.** Cheapest on paper. Rejected: every Spot reclamation destroys chat sessions and channel pairings. A platform that loses its WhatsApp pairing weekly is not production-ready at any price.

**One EC2 fleet running everything On-Demand.** Simple and reliable. Rejected: pays On-Demand prices for GPU inference, the single largest and most interruption-tolerant cost line. Fails the brief's cost-minimisation principle for no benefit — the inference workload does not want the guarantee it is paying for.

**Kubernetes (EKS) with node pools per workload class.** This *is* the same decomposition, expressed in Kubernetes. Rejected for now on total-system size: it adds a control plane, a networking model, and an expertise requirement to a platform whose agent plane is two long-lived processes and one worker pool. The decomposition is the valuable part; EKS is one way to express it, and a heavier one. Revisit past roughly a dozen services ([11 — Extensibility](../architecture/11-extensibility.md)).

**Serverless-only (Lambda + Bedrock).** Genuinely attractive: no instances, no Spot, no AMIs. Rejected as infeasible — Lambda has a 15-minute ceiling and no GPU, so it can host neither Ollama nor a long-running agent loop. It also cannot host OpenClaw's persistent outbound channel connections. Lambda is the right answer for the control plane and the wrong one for the other two.
