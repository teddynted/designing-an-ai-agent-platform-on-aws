# 7. Scalability and High Availability

## 7.1 Scaling axes

The platform scales along three independent axes. Conflating them produces a fleet that is simultaneously over- and under-provisioned.

| Axis | Driven by | Component | Mechanism | Range |
|---|---|---|---|---|
| **Workflow throughput** | Queue depth | n8n workers | Target tracking on Redis/SQS depth per worker | 0 → N (Spot) |
| **Inference throughput** | Token demand | Ollama | Step scaling on SQS depth; Bedrock absorbs the rest | **0** → N (Spot GPU) |
| **Conversational concurrency** | Active sessions | OpenClaw Gateway | **Vertical only** | 1 instance |

The third axis is the constrained one, and it is the honest limitation of this design. The Gateway is a singleton. It scales by getting bigger, not by getting more numerous.

## 7.2 Scaling the interruptible fleets

**n8n workers.** Target tracking on queue depth per worker. Spot ASG with:

- **Mixed instances policy**, 6–10 instance types across 2+ families (`m7g`, `m7i`, `c7g`, `m6i`…). Diversity is the primary defence against Spot capacity shortfall — a single-type Spot ASG is a single point of capacity failure wearing an ASG costume.
- **`capacity-optimized-prioritized`** allocation. Optimises for the *deepest* Spot pools (lowest interruption probability) while respecting a preference order. `lowest-price` is the trap: it chases the cheapest pool, which is cheapest because it is about to be reclaimed.
- **Capacity Rebalance enabled** — proactive replacement before reclamation.
- `min=1` in prod so a cold queue drains without waiting for a scale-up.

**Ollama GPU.** Same pattern, harder problem. GPU Spot pools are shallower and more AZ-concentrated than general-purpose pools; `g5`/`g6` capacity genuinely disappears. Therefore:

- Diversify across `g5.xlarge`/`g5.2xlarge`/`g6.xlarge`/`g6.2xlarge` **and** across every AZ in the region.
- **Scale to zero** when idle. This is the largest single cost lever in the platform.
- **Accept capacity failure as a routine event.** When the ASG cannot launch, the Model Gateway routes to Bedrock. The request succeeds; the bill goes up.

That last point is what makes GPU Spot defensible. Without a managed fallback, running production inference on Spot GPUs would be trading availability for cost — a bad trade. With Bedrock behind the same interface, a capacity shortfall is a **cost event, not an outage**. The Model Gateway seam ([ADR-0003](../adr/0003-model-gateway-seam.md)) is what converts one into the other.

## 7.3 Scaling the singleton

The OpenClaw Gateway cannot be horizontally scaled without solving distributed ownership of chat sessions and channel device-links. A WhatsApp pairing is a device registration; two processes sharing it is undefined behaviour, not a load-balancing strategy.

Options, in the order they should be reached for:

1. **Vertical scaling.** A Node.js Gateway process handles a large number of concurrent sessions; the work is I/O-bound waiting on inference. This carries the platform a long way.
2. **Offload tool execution.** Sandboxes already run as separate containers; move them to dedicated instances or Fargate as they grow. The Gateway becomes a router, not an executor.
3. **Shard by channel or tenant.** Run *N* Gateways, each owning a disjoint set of channels/accounts. Each remains a singleton for its shard. This is horizontal scaling of the *platform*, not of a Gateway, and it is the correct next step. It requires a routing layer and per-shard EFS access points.
4. **Fork the agent loop out of the Gateway.** Longer-term: keep OpenClaw as a channel adapter and run the agent loop as stateless workers against externalised session state. Substantial work; only justified at scale.

Sharding (3) is the recommended growth path and is designed for: the EFS layout, IAM roles, and SSM parameter namespace are already per-component and would extend to per-shard cleanly.

## 7.4 High availability

Stated honestly, per component, with real recovery targets rather than a blanket "multi-AZ."

| Component | HA model | RTO | RPO | Notes |
|---|---|---|---|---|
| ALB | Active-active, multi-AZ | ~0 | n/a | Managed |
| n8n `main` | ASG `min=max=1` across 2 AZs; state in RDS | **~2–4 min** | 0 | Relaunch, not failover. State is external, so relaunch is safe. |
| n8n workers | N across AZs, Spot | ~0 | 0 | Fungible. Loss of one is invisible. |
| **OpenClaw Gateway** | **ASG `min=max=1`, EFS-backed, multi-AZ subnets** | **~3–5 min** | **~0** | **Fast recovery, not active-active.** See below. |
| Model Gateway | ≥2 tasks across AZs | ~0 | 0 | Stateless |
| Ollama | N across AZs, Spot | ~0 | 0 | Fallback to Bedrock |
| Bedrock | Managed, regional | ~0 | n/a | Throttling is the failure mode, not unavailability |
| RDS Postgres | **Multi-AZ** | ~1–2 min | ~0 | Automatic failover |
| ElastiCache | Multi-AZ, automatic failover | ~1 min | seconds | Queue contents; loss requeues from Postgres execution records |
| EFS | Regional, multi-AZ by design | ~0 | 0 | The reason it was chosen |
| Lambda / EventBridge / SQS | Managed, regional | ~0 | 0 | |

### Why the Gateway's HA is "fast recovery"

This is the design's most important honest admission. Active-active is not available:

- Channel device-links cannot be safely shared between two processes.
- The Gateway is documented as "the single source of truth for sessions, routing, and channel connections." Two sources of truth is no source of truth.

So HA becomes: **make the recovery fast, automatic, and lossless.**

- `min=max=1` ASG spanning private-app subnets in **both** AZs. If an AZ fails, the ASG relaunches in the other.
- State on **EFS**, which is regional. The new instance mounts the same file system from a different AZ. **An EBS volume could not do this** — it is AZ-bound, so an AZ failure would demand a cross-AZ snapshot restore: slower, and lossy back to the last snapshot.
- Golden AMI keeps relaunch at roughly 60–90 seconds; ASG health-check and EFS mount take the total to ~3–5 minutes.
- RPO ≈ 0 because nothing durable lives on the instance.

The residual exposure is a **3–5 minute conversational outage** on instance or AZ failure. In-flight agent turns are lost; users retry. For a chat-driven agent platform that is acceptable, and it should be stated in the SLO ([10 — Operations](10-operations.md)) rather than hidden behind the words "highly available."

### What would break this

If OpenClaw's state proves EFS-incompatible — e.g. it uses SQLite with locking semantics EFS handles poorly, or requires low-latency random I/O — the EFS decision collapses and we fall back to EBS + AZ-pinning + frequent snapshots, trading RPO and cross-AZ RTO. **This is an untested assumption and the highest-priority validation task for Milestone 2.** Tracked in [12 — Risks](12-risks-assumptions-constraints.md).

## 7.5 Failure-mode matrix

| Failure | Blast radius | Automatic response | Human needed? |
|---|---|---|---|
| Spot worker reclaimed | One in-flight execution | Rebalance/drain; job requeued; retried | No |
| **All** GPU Spot capacity gone | Bulk inference latency & cost | Model Gateway → Bedrock | No |
| n8n `main` instance dies | Ingress + scheduling, ~2–4 min | ASG relaunch | No |
| **Gateway instance dies** | **All conversations, ~3–5 min** | ASG relaunch, EFS remount | No |
| **Gateway EFS lost** | **Channel pairings** | None | **Yes — re-pair by hand** |
| AZ failure | Brief degradation | Multi-AZ ASG + RDS failover + EFS | No |
| RDS failover | Workflow writes pause ~1–2 min | Multi-AZ failover | No |
| Redis lost | Queue contents | Requeue from execution records | No |
| Bedrock throttling | Inference latency | Retry w/ backoff; cross-region inference profile | No |
| **Region failure** | **Everything** | None | **Yes — see below** |
| **Agent budget runaway** | **Cost only** | Circuit-breaker at Model Gateway | Investigate |

Two rows have no automatic response, and they are the two worth designing around.

**Gateway EFS loss** is the platform's most damaging survivable failure, because the state cannot be re-derived from anywhere — only a human re-scanning QR codes restores it. Mitigations: EFS Backup enabled, `DeletionPolicy: Retain`, cross-region AWS Backup copy, and an SCP denying `elasticfilesystem:DeleteFileSystem` in prod. Treat it as unrecoverable and defend accordingly.

**Region failure** is out of scope for Milestone 1 and should be stated as such rather than quietly assumed away. The platform is **single-region, multi-AZ**. Cross-region DR is a Milestone 4+ decision with its own cost. What we do now is make it *possible*: AMIs distributed cross-region, EBS/EFS backups copied cross-region, S3 buckets replicated, and all infrastructure defined in CloudFormation so a second region is a stack deployment rather than an archaeology project. Realistic cold-standby RTO: **hours.** Say so.

## 7.6 Load characteristics and where they bite

| Load pattern | Absorbed by | Limit |
|---|---|---|
| Webhook burst | ALB → `main` → queue; 202 immediately | `main` vertical capacity |
| Sustained bulk inference | Ollama Spot scale-out | GPU Spot pool depth |
| Interactive inference spike | Bedrock | Bedrock account quotas — **request increases early** |
| Many concurrent conversations | Gateway vertical scale | **Single Gateway process** — the real ceiling |
| Long-running agent task | Sandbox container | Sandbox timeout + token budget |

The conversational-concurrency ceiling is the first wall this platform will hit, and sharding (§7.3) is the answer. Bedrock account quotas are the second, and they are a support ticket, not an architecture problem — but a support ticket that takes days, so raise it before you need it.
