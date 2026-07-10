# 12. Assumptions, Constraints, and Risks

The parts of the design that are not yet true, cannot be changed, or might go wrong.

## 12.1 Assumptions

Assumptions are things this design **treats as true without having verified them**. Each has an owner and a validation task. The ones marked ⚠ would force real rework if false.

| # | Assumption | If false | Validate |
|---|---|---|---|
| A1 | ⚠ **OpenClaw's state directory works correctly on EFS** (no SQLite locking issues, tolerable latency) | The cross-AZ HA story collapses. Fall back to EBS + AZ-pinning + frequent snapshots; RTO and RPO both worsen | **Highest priority — before any infrastructure is written.** Mount, run, restart, fail over |
| A2 | ⚠ **Most required chat channels are outbound-initiated** | Zero-ingress property lost for that channel; route it via ALB → n8n → Gateway ([05 §5.4](05-network-and-boundaries.md)) | Per channel, before it is connected |
| A3 | ⚠ **Workflow steps can be made idempotent or compensating** | Spot workers become unsafe; n8n workers move to On-Demand, cost rises materially | When the first workflows land |
| A4 | GPU Spot capacity for `g5`/`g6` is available in ≥2 AZs most of the time | More Bedrock fallback than modelled; token spend rises, availability holds | Observe interruption and launch-failure rates |
| A5 | n8n queue mode isolates all durable state from workers | Workers are not stateless; Spot is unsafe | Read current n8n docs (not verified during design) |
| A6 | Bedrock batch/prompt-caching discounts make bulk routing economics work | Crossover point moves; Ollama justified on residency/control rather than cost | Re-cost against current pricing |
| A7 | A single Gateway process handles expected conversational concurrency | Sharding ([07 §7.3](07-scalability-and-ha.md)) is needed sooner | Load test |
| A8 | Docker sibling-container sandboxing gives adequate isolation | Escalate to Firecracker/microVM or dedicated sandbox instances | Threat review |
| A9 | Bedrock is available in the chosen region with the required models | Region choice changes, or cross-region inference profiles required | Day one |
| A10 | Team is comfortable operating EC2 + ASG + CloudFormation | The EKS-vs-EC2 trade-off ([03](03-aws-services.md)) shifts | Now |

**A1 is the assumption this architecture rests on most heavily.** The choice of EFS is what converts "Gateway state loss requires a human with a phone" from a likely AZ-failure outcome into an unlikely one. It is asserted in [07 §7.4](07-scalability-and-ha.md) and it is **not yet tested**. Test it before writing any other CloudFormation.

## 12.2 Constraints

Constraints are fixed. They shaped the design; they are not open for optimisation.

**Imposed by the brief:**

| Constraint | Consequence |
|---|---|
| CloudFormation for IaC | No Terraform/Pulumi. CDK remains available as a generator |
| Must use EC2 Spot | Drove the stateless/stateful split — the design's central move |
| Must use custom AMIs | Drove the three-AMI Image Builder pipeline |
| Must minimise EC2 startup time | Drove baked weights + pre-staged snapshots. **Collided with the warm-pool/Spot limitation** |
| Must support managed *and* self-hosted models | Drove the Model Gateway seam |
| This repository is design-only | No templates, no workflows. RTOs are *designed*, not measured |

**Imposed by technology:**

| Constraint | Consequence |
|---|---|
| ⚠ **ASG warm pools do not support Spot Instances** | The worst-cold-start fleet (GPU) cannot use the best cold-start tool. Forced baked-AMI + SQS-buffering strategy ([ADR-0006](../adr/0006-startup-time-strategy.md)) |
| **EBS volumes are AZ-bound** | Forced EFS for Gateway state |
| **OpenClaw Gateway is a singleton** (channel device-links) | No active-active. Conversational SLO capped at ~99.5% |
| **Channel pairings are human-recoverable only** | Gateway state loss is the platform's worst survivable failure |
| Docker socket access ≈ root on host | The Gateway process, not just the sandbox, is a trust boundary |
| Spot gives 2 minutes' notice | Nothing on Spot may need >2 min to hand off state |
| Lambda: 15 min, no GPU | Lambda is control plane only |
| Bedrock quotas are per-account, per-model | Request increases before launch, not during |
| EBS FSR bills per snapshot per AZ per hour | A cold-start accelerator with a standing ~$540/mo cost |

## 12.3 Risk register

Scored `likelihood × impact`. Ordered by the product.

| # | Risk | L | I | Mitigation | Residual |
|---|---|---|---|---|---|
| **R1** | **Runaway agent burns unbounded tokens** | **High** | High | Budget circuit-breaker at Model Gateway; max-iteration caps; Cost Anomaly Detection ([09 §9.5](09-cost.md)) | **Low** — but only once the Model Gateway exists. **Until then this risk is unmitigated.** |
| **R2** | **Prompt injection → agent misuses its legitimate authority** | **High** | High | Sandbox with no credentials; IMDS blocked; egress allowlist; tool risk tiers; human approval for privileged tools ([08](08-security.md)) | **Medium.** Contained, not solved. A fully-persuaded agent can still use its auto-approved tools |
| R3 | Gateway EFS/state loss → manual channel re-pairing | Low | High | EFS Backup, `DeletionPolicy: Retain`, cross-region copy, SCP denying deletion, quarterly restore tests | Low |
| R4 | GPU Spot capacity unavailable at scale | Medium | Medium | Instance-type + AZ diversification; **Bedrock fallback via Model Gateway** | **Low — degrades cost, not availability.** This is the design's best-earned mitigation |
| R5 | A1 false: OpenClaw incompatible with EFS | Medium | High | Test first. Fallback: EBS + AZ-pin + frequent snapshots | Medium until tested |
| R6 | Conversational concurrency ceiling reached | Medium | Medium | Vertical scale, then shard ([07 §7.3](07-scalability-and-ha.md)) | Low — the growth path is designed |
| R7 | OpenClaw supply-chain compromise | Medium | High | Pin versions; verify provenance; review deps; never `curl \| bash` in AMI builds; no secrets in sandbox | Medium — fast-moving project, three renames, active impersonation scams during the rename |
| R8 | Model Gateway becomes a hot-path SPOF | Medium | Medium | Stateless, ≥2 AZs; callers can speak the identical contract directly to Bedrock | Low |
| R9 | Region failure | Low | High | **Accepted.** Single-region by design. Cross-region assets staged; RTO hours | **Accepted, documented** |
| R10 | Approval fatigue turns human-in-the-loop into a rubber stamp | Medium | Medium | Keep the privileged tool set small; monitor approval-queue depth and wait time | Medium — a **social** control, and the weakest kind |
| R11 | Bedrock throttling under burst | Medium | Low | Retry/backoff; cross-region inference profiles; raise quotas early | Low |
| R12 | FSR left enabled, silently costing >$500/mo per snapshot | Medium | Low | Alarm on FSR-enabled snapshot count | Low |
| R13 | CloudFormation Exports create un-evolvable coupling | Low | Medium | SSM Parameter Store as the cross-stack contract ([ADR-0007](../adr/0007-cloudformation-stack-layering.md)) | Low — designed out |
| R14 | A3 false: workflows not idempotent → Spot unsafe for workers | Medium | Medium | Platform documents the requirement; move workers to On-Demand if breached | Medium |

### The two risks that define this platform

**R1 and R2 are the platform's characteristic risks** — the ones that exist *because* it runs autonomous agents, not because it runs on AWS. Both are rated High/High. Both are mitigated by controls that live in the Model Gateway and the tool policy, and **neither of those components exists yet.**

That is a defensible position for a design document, but it must be said out loud: **the platform is not safe to point at production data or real credentials until the budget circuit-breaker and the tool-policy sandbox exist.** They are not "hardening"; they are load-bearing. Build them ahead of any workflow that touches something real.

Everything else in this register is ordinary infrastructure risk with ordinary infrastructure mitigations.

## 12.4 What to validate and build first

In priority order, derived from the above:

1. **Validate A1** — OpenClaw on EFS. Mount, run, restart, simulate AZ failure. Every HA claim depends on it. Do this before writing templates.
2. **Build the sandbox boundary** (IMDS block, egress allowlist, no credentials) **before** connecting any real channel. R2 is unmitigated without it.
3. **Build the budget circuit-breaker** before any agent touches a frontier model with a real key. R1 is unmitigated without it.
4. **Stand up the stack layering** (`10-network` → `50-serverless`) with SSM parameters as the cross-stack contract.
5. **Build the three-AMI Image Builder pipeline** and measure real cold-start times. The 2–4 minute figure in [06](06-deployment.md) is an estimate.
6. Confirm A5 (n8n queue mode) and A6 (Bedrock pricing) against current documentation — both were assumed, not verified, during design.
7. Request Bedrock quota increases. They take days.
8. Instrument the Bedrock/Ollama routing split from day one, so the cost crossover in [09 §9.4](09-cost.md) is measured rather than assumed.
