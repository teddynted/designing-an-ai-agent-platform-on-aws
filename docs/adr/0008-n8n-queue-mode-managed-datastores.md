# ADR-0008: Run n8n in queue mode on managed datastores

**Status:** Accepted
**Date:** 2026-07-09

## Context

n8n's default deployment is a single process holding workflows, credentials, execution history, and the scheduler — with SQLite on local disk. Simple, and entirely incompatible with this platform's requirements:

- It cannot run on **Spot** ([ADR-0005](0005-spot-only-for-stateless-workloads.md)) — reclamation destroys in-flight executions and local state.
- It cannot scale horizontally.
- Its state lives on an instance, which means an instance failure is a data-loss event.

n8n's **queue mode** separates a stateful `main` process (workflows, scheduling, webhook ingress) from **stateless workers** that pull executions from a queue. State moves to Postgres; the queue is Redis-backed.

This separation is what makes n8n compatible with [ADR-0002](0002-three-plane-decomposition.md) at all. Without it, the agent plane has no stateless component and no Spot story.

Neither Postgres nor Redis appears in the brief's technology list. The brief asks that the listed technologies be incorporated "where appropriate" — it does not restrict the design to them. Adding a datastore is nevertheless a decision that should be recorded rather than assumed.

## Decision

**Run n8n in queue mode.**

- `main` — On-Demand EC2, Graviton, behind an ALB across two AZs. Stateless with respect to the instance; all durable state is external.
- `workers` — **Spot** ASG, scaled on queue depth, zero local state.

**Use managed datastores in production:**

| Store | Service | Why |
|---|---|---|
| Workflow/credential/execution state | **RDS for PostgreSQL, Multi-AZ** | Automatic failover, backups, patching. The platform's second-most-painful data-loss surface after Gateway state |
| Execution queue | **ElastiCache for Redis**, Multi-AZ | Managed failover |

**In dev**, run both as containers on the `main` instance. The consequence of losing dev workflow history is an inconvenience, and the saving is real (~$125/mo).

Aurora Serverless v2 is a reasonable substitute for RDS where the workload is spiky; the decision is reversible.

## Consequences

**Positive**

- **Workers become Spot-eligible**, which is the entire point. Workflow execution — the bulk of n8n's compute — runs at 60–90% off.
- Horizontal scaling on queue depth.
- `main` failure is a **relaunch**, not a recovery: state is external, RPO is zero, RTO is ASG replacement time (~2–4 min).
- Backups, failover, patching, and encryption on the datastores are AWS's problem, not a runbook.
- Postgres is available for other platform needs — notably **pgvector for agent memory/RAG**, which avoids introducing a separate vector store in the near term ([11 — Extensibility](../architecture/11-extensibility.md)).

**Negative**

- **Two more managed services to pay for and operate** (~$125/mo in prod). For a small deployment this may exceed the compute it supports. Justified by the Spot savings on workers plus the availability of `main`, but it is a real cost with a real crossover.
- Two more failure modes: RDS failover pauses workflow writes ~1–2 minutes; Redis loss drops queued jobs (recoverable from Postgres execution records).
- Queue mode is operationally more complex than a single process — more components, more configuration, more ways to misconfigure.
- **Requeued executions must be idempotent.** A Spot worker reclaimed mid-execution returns its job to the queue; another worker runs it again. If the workflow already sent an email, it sends it twice. This constraint is inherited by every workflow the platform hosts, and it is enforced by convention rather than by the runtime. It is assumption **A3** in [12 — Risks](../architecture/12-risks-assumptions-constraints.md), and if it proves unworkable, workers move to On-Demand and the cost case for queue mode weakens considerably.
- Steps outside the brief's technology list. Recorded here so the departure is visible.

## Assumption to verify

⚠ This ADR assumes queue mode isolates **all** durable state from workers, and that the broker is Redis. Both were taken from established knowledge of n8n's architecture and were **not re-verified against current documentation during this milestone** (assumption A5). If workers retain any durable local state, they are not Spot-safe and this ADR must be revisited. **Confirm before building the worker ASG.**

Also unresolved: whether dedicated **webhook processor** instances (a further queue-mode split) are warranted at our traffic volume, or whether `main` should handle webhook ingress directly. Deferred to Milestone 2 with real traffic figures.

## Alternatives considered

**n8n single process with SQLite on EBS.** Simplest and cheapest. Rejected: no Spot, no horizontal scaling, and instance failure is data loss. Acceptable only in dev, where it is what we do.

**Self-hosted Postgres and Redis on EC2.** Saves the managed-service premium. Rejected: introduces two stateful, backup-bearing, failover-managing components into a platform explicitly designed to minimise operational surface. We would be hand-building RDS Multi-AZ, badly. In dev, where we accept the risk, we do exactly this — which is the correct place for it.

**SQS instead of Redis for the execution queue.** Attractive — fully managed, already used elsewhere in the platform for inference buffering. Rejected: n8n's queue mode expects a Redis-backed queue. Substituting SQS means forking or wrapping n8n, which trades a $30/mo service for a maintenance burden on someone else's codebase.

**Replace n8n with Step Functions.** Managed, serverless, no datastores, native AWS integration. Rejected: the brief specifies n8n, and n8n's value is its several-hundred-integration library and its visual editor — a different product from Step Functions, aimed at a different author. Step Functions remains the right tool for *control-plane* sagas if the Lambda reactors grow stateful.
