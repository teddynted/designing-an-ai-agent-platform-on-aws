# 10. Operational Considerations

Operating an agent platform differs from operating a web service in one specific way: **the unit of work is an agent run, not a request.** A run may last minutes, invoke a dozen tools, spend real money, and touch real systems. If your observability is request-shaped, you cannot answer the questions that matter.

## 10.1 The agent run as the primary observable

Every dashboard, log query, and alarm is anchored on `agent_run_id`, minted at the entry point of every flow and propagated everywhere ([04 — Flows](04-flows.md)).

```
agent_run_id
├── trigger            (webhook | schedule | chat message | AWS event)
├── route              (n8n workflow | OpenClaw agent)
├── model calls        [provider, model, prompt/completion tokens, latency, cost]
├── tool calls         [name, risk tier, approved_by, duration, exit code]
├── sandbox containers [image, egress attempts, resource use]
├── outcome            (success | failure | budget_exceeded | timeout | denied)
└── total cost, total duration
```

This structure makes four otherwise-unanswerable questions answerable in a single log query:

- *What did this agent actually do?* (incident forensics)
- *What did it cost?* (unit economics)
- *Where did it stall?* (latency)
- *What did it touch?* (blast-radius assessment after a suspected injection)

Implement with CloudWatch **Embedded Metric Format** — structured logs that emit metrics as a side effect, so cost and outcome dimensions come free with the log line rather than requiring a parallel metrics pipeline.

## 10.2 Metrics that matter

**Platform health** — familiar, necessary, not sufficient:

| Metric | Alarm |
|---|---|
| n8n queue depth, queue age | Depth rising while workers at max |
| Worker Spot interruption rate | Sustained spike → diversify pools |
| ALB 5xx, target health | Any sustained 5xx |
| Gateway process up / channel connections | **Any channel disconnected > 2 min** |
| Ollama scale-up latency | > 5 min (cold-start regression) |
| Bedrock throttles | Rising → request quota increase |
| RDS connections, replica lag, CPU | Standard |

**Agent economics** — the dashboard that does not exist by default and matters most:

| Metric | Why |
|---|---|
| **Cost per successful outcome** | The real efficiency number. Optimise this, not cost per token. |
| Tokens per run (p50/p95/p99) | p99 spikes = reasoning loops |
| Tool calls per run | Rising = the agent is flailing |
| Agent run success rate | Quality regression signal |
| Runs hitting the budget circuit-breaker | **Should be near zero. Non-zero = a bug or an attack.** |
| Approval-queue depth and wait time | Approval fatigue detector ([08 — Security](08-security.md)) |
| Bedrock : Ollama routing split | Validates the crossover assumption in [09 — Cost](09-cost.md) |

**Security signals** are in [08 — Security §8.7](08-security.md). Note that `budget_exceeded` appears in both this section and that one — the same event is the platform's clearest cost alarm *and* one of its clearest compromise signals.

## 10.3 Logging

| Source | Destination | Retention |
|---|---|---|
| Application (n8n, Gateway, Model Gateway) | CloudWatch Logs, structured JSON | 30 d → S3 → Glacier |
| Sandbox container stdout/stderr | CloudWatch, labelled `agent_run_id` | 30 d |
| VPC Flow Logs | S3 | 90 d |
| CloudTrail (org-wide) | `security` account S3 | 1 y + Glacier |
| ALB access logs | S3 | 90 d |

Two rules specific to this platform:

1. **Scrub secrets on the agent output path.** An agent that reads a credential and prints it has exfiltrated it into CloudWatch, where it is now durable, indexed, and readable by anyone with log access.
2. **Treat agent output in logs as untrusted data.** It renders in dashboards and tickets. Log injection and downstream XSS are live concerns, and the payload arrives via a model that was asked politely.

## 10.4 SLOs

Stated with the singleton's limits admitted rather than averaged away.

| Service | SLO | Basis |
|---|---|---|
| Webhook ingress availability | 99.9% | ALB + `main` ASG relaunch (~2–4 min) |
| Workflow execution success (excl. tenant bugs) | 99.5% | Spot retry + idempotency |
| **Conversational availability** | **99.5%** | **Singleton Gateway: ~3–5 min relaunch RTO. ~99.9% is not achievable without sharding.** |
| Inference availability (any provider) | 99.9% | Bedrock backstop |
| Interactive inference p95 latency | < 5 s | Bedrock path, no cold start |
| Bulk inference p95 latency | < 10 min | Includes GPU cold start; SQS-buffered |

**Conversational availability is deliberately 99.5%, not 99.9%.** A single Gateway with a 3–5 minute relaunch cannot support three nines against instance and AZ failure. Publishing 99.9% would be a fiction that survives exactly until the first incident. Raising it requires the sharding work in [07 §7.3](07-scalability-and-ha.md) — a real project, and the SLO is what makes that trade-off visible to whoever must fund it.

## 10.5 Runbooks

As SSM Automation documents where possible, so the fix is executable and audited rather than a wiki page someone follows at 3 a.m.

| Scenario | Response |
|---|---|
| Gateway down | ASG relaunches automatically. Verify EFS mount, then channel reconnection. **If channels do not reconnect, suspect state loss — do not terminate the instance.** |
| **Gateway channel unpaired** | **Manual re-pairing (QR scan). No automation exists. Escalate to a named human.** |
| Spot capacity exhausted (GPU) | Confirm Model Gateway fell back to Bedrock. Watch spend. Consider On-Demand baseline. |
| Runaway agent / budget breach | Circuit-breaker has already stopped it. Identify the run, inspect the tool-call trace, decide bug vs. injection. |
| Suspected prompt injection | Freeze the agent's tool allowlist. Pull `agent_run_id` trace. Check CloudTrail for the Gateway role. Rotate credentials if any privileged tool ran. |
| n8n queue backing up | Check worker health, Spot interruption rate, and per-node latency. Scale workers or diversify pools. |
| Bedrock throttling | Verify retry/backoff. Enable cross-region inference profile. Raise quota. |
| Cold-start regression | Check AMI age, whether weights are still baked, whether FSR was disabled. |
| FSR cost alarm | Confirm which snapshots have FSR enabled; disable unused. |

Two rows have **no automated recovery** and both are called out deliberately: manual channel re-pairing, and deciding whether an anomalous agent run was a bug or an attack. Both require a human, and both should be rehearsed before they happen.

## 10.6 Maintenance

- **Patching:** golden AMI rebuild → SSM parameter update → ASG instance refresh. Never patch a running instance; instances are cattle. The Gateway is the exception that proves it — replacing it is a scheduled, verified operation ([06 §6.5](06-deployment.md)).
- **OpenClaw upgrades:** pin versions. The project moves fast, renamed itself three times in a fortnight, and attracted impersonation scams during the rename. Review changelogs; do not track `latest`; verify artifact provenance.
- **n8n upgrades:** workers first, then `main`. Watch for workflow-schema migrations.
- **Model updates:** new Bedrock model IDs are a routing-policy change in SSM, not a code deploy. This is a benefit of the Model Gateway seam that shows up on an ordinary Tuesday.
- **Drift detection:** scheduled CloudFormation drift detection; investigate every finding. Drift in a platform where agents hold IAM roles is not a tidiness issue.
- **Backup verification:** **restore-test EFS and RDS backups quarterly.** An untested backup is a hypothesis. Given that Gateway state loss is unrecoverable by automation, this is the single most important scheduled maintenance task in the platform.

## 10.7 Operational maturity roadmap

| Milestone | Capability |
|---|---|
| M1 (this) | Architecture; observability *design*; `agent_run_id` contract defined |
| M2 | CloudFormation stacks; CloudWatch dashboards; core alarms; SSM runbooks |
| M3 | Model Gateway with token metering; budget circuit-breaker; cost-per-outcome dashboard |
| M4 | Agent quality metrics; approval workflow; injection-detection signals |
| M5 | Chaos testing: kill the Gateway, kill an AZ, exhaust Spot; verify RTOs match §10.4 |

The RTOs in this document are **designed**, not measured. Milestone 5 is where they become facts. Until then they are claims, and this document should be read as making claims.
