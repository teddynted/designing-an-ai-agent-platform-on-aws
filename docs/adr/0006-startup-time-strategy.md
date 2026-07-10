# ADR-0006: Minimise EC2 startup time with baked AMIs and pre-staged snapshots, not warm pools

**Status:** Accepted
**Date:** 2026-07-09

## Context

The brief requires minimising EC2 startup time and mandates custom AMIs. Startup time matters most for the fleet that scales from zero: **Ollama GPU inference**. A naive GPU node boots, installs drivers, pulls a container image, then downloads tens of gigabytes of model weights over the internet — several minutes before it serves a token.

AWS's canonical answer to slow boots is **ASG warm pools**: pre-initialised, stopped instances that resume in seconds.

**Warm pools do not support Spot Instances.** As of November 2025 they support mixed-instances policies for *On-Demand* instance types only. The fleet with the worst cold start — Ollama on Spot GPUs — is precisely the fleet that cannot use the best tool for it.

This constraint is the single most shaping fact in the startup-time design, and a design that assumed otherwise would fail at implementation.

## Decision

Attack startup time in layers, accepting that the residual cold start for Spot GPUs is **~2–4 minutes** and architecting around it rather than pretending it away.

| Layer | Technique | Saves | Applies to |
|---|---|---|---|
| 1 | **Golden AMI** — OS, drivers, Docker, container images pre-pulled | 60–120 s | all |
| 2 | **Model weights baked into the AMI** | minutes | `ollama-gpu` |
| 3 | **Pre-staged EBS snapshot** for large/rarer weights, attached at boot | minutes | `ollama-gpu` |
| 4 | **EBS Fast Snapshot Restore** on that snapshot — removes lazy-load penalty | tens of seconds | `ollama-gpu`, **selectively** |
| 5 | **Minimal user-data** — fetch config from SSM, start containers. Zero installs | 30–60 s | all |
| 6 | **S3 Gateway Endpoint** for remaining artifact pulls | seconds + NAT cost | all |
| 7 | **Warm pools** | 1–3 min | **On-Demand fleets only** |

Three AMIs, built by EC2 Image Builder, not one: `n8n-worker`, `ollama-gpu`, `agent-gateway`. AMI IDs are published to SSM Parameter Store and consumed by launch templates.

**Architect around the residual.** Because 2–4 minutes cannot be eliminated for Spot GPUs:

- **SQS buffers** bulk inference requests so the cold start is absorbed by a queue, not by a caller ([04 §4.4](../architecture/04-flows.md)).
- **Interactive traffic routes to Bedrock**, which has no cold start at all ([ADR-0004](0004-inference-routing-policy.md)).

The cold start is thus made *invisible* rather than *small*. That is the real decision here.

## Consequences

**Positive**

- Cold start drops from "download 40 GB and install CUDA" to "boot and start a container."
- Weights come from S3 via a free Gateway Endpoint rather than the internet through a metered NAT — faster, cheaper, and no supply-chain exposure at boot.
- Three narrow AMIs avoid forcing a 40 GB GPU image onto every n8n worker.
- Immutable infrastructure: instances are never patched, only replaced.
- Warm pools remain available as a **cost/latency dial** if we later add an On-Demand inference baseline. This is a lever we have not pulled, not one we have lost.

**Negative**

- **Baked weights make AMIs large and slow to build.** A model upgrade is an AMI rebuild (~30–60 min), not a `docker pull`. Model iteration slows down. This is the central trade.
- Three AMI pipelines to maintain, patch, and test.
- AMI storage and snapshot costs, which grow with model count.
- ⚠ **Fast Snapshot Restore bills ≈ $0.75/DSU-hour, per snapshot, per AZ — continuously while enabled** (≈ $540/month each). Enabled on three snapshots across three AZs, it costs more than the GPU instances it accelerates. **Enable on one hot snapshot, in launch AZs only, and alarm on the FSR-enabled snapshot count.** This is a cold-start accelerator with a standing cost, and it is an easy way to build a bill nobody can explain ([09 §9.6](../architecture/09-cost.md)).
- Residual 2–4 minute cold start remains. It is hidden by SQS and Bedrock routing, not removed. A workload that is both latency-sensitive and requires self-hosted models has no good answer here.
- Cold-start times in this ADR are **estimates**. Measure them in Milestone 2.

## Alternatives considered

**Warm pools for the GPU fleet.** **Not available.** Warm pools do not support Spot. Could be used with an On-Demand GPU fleet, at 3–5× the instance cost — which trades the platform's largest cost saving for two minutes of latency. Rejected, but it is the right lever if scale-up latency ever becomes intolerable.

**Pull weights from S3 at boot, nothing baked.** Simpler AMIs, faster to rebuild, trivial model upgrades. Rejected on time: tens of gigabytes over the S3 endpoint still costs minutes on every scale-up event, and scale-up is frequent by design ([ADR-0012](0012-scale-inference-to-zero.md)). It is the correct approach for **rare or large models** that do not justify AMI space — hence layer 3.

**Keep one GPU instance always warm.** ~$250/mo, and eliminates cold start for the first concurrent request only. Rejected as the default: it forfeits scale-to-zero, the platform's largest cost lever, to solve a problem that SQS buffering and Bedrock routing already solve. Reasonable as a future dial if bulk latency SLOs tighten.

**Instance store (NVMe) for weights.** Fast, free with the instance, ephemeral. Attractive on `g5`/`g6`, which have local NVMe. Rejected as the primary mechanism because it must be *populated* at boot from somewhere — which is the original problem. Useful as a hot cache tier alongside baked weights.

**Container image with weights, pulled from ECR at boot.** Same size problem as S3, plus ECR pull throughput limits. Layer 1 pre-pulls the *runtime* image precisely to avoid this on the hot path.
