# ADR-0009: Treat the OpenClaw Gateway as an EFS-backed singleton with fast recovery

**Status:** Accepted
**Date:** 2026-07-09

## Context

OpenClaw runs as a long-lived **Gateway process** that its documentation describes as *"the single source of truth for sessions, routing, and channel connections."* One Gateway serves every configured channel plugin simultaneously. Configuration and workspace live on disk under `~/.openclaw`. The Control UI binds to `127.0.0.1:18789`.

Two properties of that state determine everything:

1. **Channel connections are device-links, not tokens.** A WhatsApp or Signal pairing is a device registration held by the chat platform. Two Gateway processes sharing one pairing is undefined behaviour, not load balancing.
2. **Losing the state is not recoverable by automation.** No backup restores a device-link that the chat platform has invalidated. Recovery is a human, holding a phone, scanning a QR code.

This is an unusual shape for a cloud component: **the most valuable state in the platform cannot be reconstructed from a backup by a machine.**

Meanwhile, EBS volumes are AZ-bound. An EBS-backed Gateway that loses its AZ cannot be relaunched elsewhere without a cross-AZ snapshot restore — slow, and lossy back to the last snapshot.

## Decision

Treat the Gateway as a **singleton with fast, lossless recovery**, not as a highly available service.

- **ASG `min = max = 1`**, spanning private-app subnets in **both** AZs. AZ failure relaunches the instance in the surviving AZ.
- **On-Demand**, never Spot ([ADR-0005](0005-spot-only-for-stateless-workloads.md)).
- **State on EFS**, not EBS. EFS is regional; the replacement instance in a different AZ mounts the same file system.
- **Golden AMI** keeps relaunch to ~60–90 s; total RTO ~3–5 min including health checks and EFS mount.
- **Zero ingress.** Channels are outbound-initiated; the Control UI is loopback-bound. The security group has no inbound rules. Administration is SSM Session Manager plus an SSM port-forward to `18789` ([05 §5.4](../architecture/05-network-and-boundaries.md)).
- **Protect the state as unrecoverable:** EFS Backup, `DeletionPolicy: Retain`, cross-region AWS Backup copy, and an SCP denying `elasticfilesystem:DeleteFileSystem` in prod.

**Publish the resulting SLO honestly: ~99.5% conversational availability**, not 99.9% ([10 §10.4](../architecture/10-operations.md)).

## Consequences

**Positive**

- **RPO ≈ 0.** Nothing durable lives on the instance.
- **Cross-AZ recovery without a restore.** This is the whole reason EFS is here. It converts an AZ failure from "unrecoverable-by-automation state loss" into "a 3–5 minute relaunch."
- Zero ingress means the agent runtime has **no internet-reachable attack surface**. The residual attack surface is entirely semantic — what the model is persuaded to do with reach it legitimately has ([ADR-0010](0010-agent-sandbox-containment.md)).
- No SSH, no bastion, no port 22, no public IP.
- The scaling path — shard by channel/tenant — is designed for: EFS access points, IAM roles, and the SSM namespace are already per-component and extend to per-shard cleanly ([07 §7.3](../architecture/07-scalability-and-ha.md)).

**Negative**

- **~3–5 minute conversational outage** on instance or AZ failure. In-flight agent turns are lost. This caps the conversational SLO at ~99.5% and no amount of AWS configuration raises it.
- **No horizontal scaling.** Conversational concurrency is bounded by one process. This is the first ceiling the platform will hit.
- **No rolling deploys.** Updating the Gateway is stop-replace-start, in a maintenance window ([06 §6.5](../architecture/06-deployment.md)).
- EFS is slower per operation and more expensive per GB than EBS.
- ⚠ **The EFS choice rests on an untested assumption (A1):** that OpenClaw's state directory behaves correctly on EFS — no SQLite locking pathologies, tolerable latency. **If false, the entire cross-AZ HA story collapses** and we fall back to EBS + AZ-pinning + frequent snapshots, worsening both RTO and RPO. This is the **highest-priority validation task before implementation** and should be tested before any CloudFormation is written ([12 — Risks](../architecture/12-risks-assumptions-constraints.md)).
- The Gateway holds the host Docker socket to spawn sandboxes, which is equivalent to root on the host. The Gateway process is therefore itself a trust boundary, not merely a supervisor of one.

## Alternatives considered

**Active-active Gateways behind a load balancer.** The reflex answer, and it does not work. Chat channel device-links cannot be shared between processes, and OpenClaw explicitly holds sessions and routing in a single source of truth. Two of them is zero of them. Not a configuration problem — a property of how the chat platforms authenticate a client.

**EBS-backed singleton, pinned to one AZ, with frequent snapshots.** Cheaper and faster per I/O. Rejected as the primary design: an AZ failure then requires a cross-AZ snapshot restore, giving a worse RTO and a non-zero RPO on state whose loss demands manual re-pairing. **This is the documented fallback if assumption A1 fails.**

**Spot with fast recovery.** Rejected: interruptions would be frequent, and each one is a multi-minute conversational outage. The instance is one of three On-Demand instances in the platform; the saving is not worth the availability.

**Stateless Gateway with session state externalised to Postgres/Redis.** The correct long-term answer, and it removes the singleton constraint entirely. Rejected for now: it means forking OpenClaw or substantially restructuring it, and the channel device-link problem persists regardless — the pairing lives in the chat platform, not in our datastore. Externalising sessions solves concurrency, not channel ownership. Sharding solves both, and is cheaper.

**Run the Gateway on Fargate with EFS.** Removes instance management. Rejected: the Gateway spawns sibling Docker containers via the host Docker socket, which Fargate does not expose. Feasible only if sandboxing moves to a different mechanism.
