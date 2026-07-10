# 9. Cost Optimisation

> ⚠ **All figures below are illustrative order-of-magnitude estimates** for `us-east-1`, used to compare *architectural options* rather than to forecast a bill. AWS pricing changes; Bedrock token pricing and batch discounts were **not independently re-verified during this milestone**. Re-cost before committing. The *ratios* are the durable content here, not the absolute numbers.

## 9.1 The cost shape of an agent platform

Agent platforms have an unusual cost profile, and designing for the wrong one is the common mistake.

1. **Idle cost dominates for most of the platform's life.** An agent platform spends most of its hours waiting for a message. A design that is cheap under load but expensive at rest loses.
2. **Token cost dominates under load** — and it is *unbounded*, because an agent decides how many tokens to spend. No other AWS workload lets the workload choose its own bill.
3. **Cost anomalies and security anomalies are the same event.** A runaway agent and a compromised agent both look like a spend spike.

So the strategy is: **drive idle cost to near zero, make token cost attributable and capped, and let load cost scale linearly.**

## 9.2 The seven levers, ranked by impact

| # | Lever | Mechanism | Saving |
|---|---|---|---|
| 1 | **Scale inference to zero** | Ollama ASG desired=0 when idle | ~100% of GPU cost while idle |
| 2 | **Spot for all interruptible compute** | Workers + GPU inference | 60–90% vs On-Demand |
| 3 | **Route by latency tolerance** | Bulk → Ollama; interactive → Bedrock | Varies; see §9.4 |
| 4 | **S3 Gateway Endpoint** | Model weights bypass NAT | Removes GB-scale NAT charges |
| 5 | **Graviton** for control-plane EC2 | `m7g`/`c7g` | ~20% vs x86 |
| 6 | **Right-size NAT** | 1 NAT in dev, per-AZ in prod | ~$33/mo per NAT avoided |
| 7 | **gp3 over gp2**, log lifecycle to S3/Glacier | | ~20% storage, ~70% log retention |

Levers 1 and 2 compound: a GPU fleet that is both Spot-priced and scaled to zero costs roughly **5–10%** of an always-on On-Demand fleet.

## 9.3 Illustrative monthly cost

**Dev** (single AZ, everything scaled to zero when idle):

| Item | ~$/mo |
|---|---|
| NAT Gateway (single) | 33 |
| n8n `main` + Gateway (`m7g.large` ×2, On-Demand) | 120 |
| n8n workers (Spot, min 0, light use) | 10 |
| Ollama GPU (Spot, scale-to-zero, ~20 h/mo) | 8 |
| EBS, S3, EFS, secrets | 20 |
| ALB | 18 |
| Bedrock tokens (light) | 30 |
| **Total** | **~240** |

**Prod** (2 AZ, moderate load):

| Item | ~$/mo | Note |
|---|---|---|
| NAT Gateway ×2 | 66 | HA; the price of AZ-independent egress |
| n8n `main` (`m7g.large`, On-Demand) | 60 | |
| OpenClaw Gateway (`m7g.xlarge`, On-Demand) | 120 | Singleton, cannot be Spot |
| n8n workers (Spot, avg 3 × `m7g.large`) | 55 | ~70% off On-Demand |
| Model Gateway (Fargate ×2) | 40 | |
| Ollama GPU (Spot `g5.xlarge`, ~200 h/mo) | 70 | vs ~$200 On-Demand, vs ~$730 always-on |
| RDS Postgres Multi-AZ (`t4g.medium`) | 95 | |
| ElastiCache (`t4g.micro` ×2) | 30 | |
| EFS (small, Standard) | 15 | |
| EBS + snapshots + AMIs | 40 | |
| ALB ×2 (public + internal) | 36 | |
| VPC interface endpoints (~5) | 36 | Pays for itself vs NAT on Bedrock traffic |
| CloudWatch, CloudTrail, GuardDuty | 60 | |
| **Infrastructure subtotal** | **~720** | |
| **Bedrock tokens** | **50 – 5,000+** | **Unbounded. The real variable.** |

Read that last row carefully. **Infrastructure is a rounding error next to token spend at any serious volume.** Optimising a $36/month ALB while an agent burns $3,000/month in tokens is misallocated effort. The highest-leverage cost work in this platform is §9.5, not §9.2.

## 9.4 Bedrock vs. Ollama: where the crossover actually is

The self-hosted path is not automatically cheaper. It has a floor.

- A `g5.xlarge` Spot GPU at roughly $0.35/hour costs ~$250/month **if run continuously** — regardless of whether it serves one token or a billion.
- Bedrock costs **zero at idle** and scales linearly with tokens.

Therefore:

```
Low / spiky volume   ->  Bedrock wins decisively (no idle cost, no ops)
High sustained volume ->  Ollama wins (fixed cost amortised across many tokens)
Crossover             ->  where monthly token spend on Bedrock exceeds the
                          amortised cost of the GPU hours needed to serve them
```

The crossover moves with model size, batch efficiency, and Spot price. **Instrument it rather than assume it.** The Model Gateway's token metering exists partly to answer this question with data instead of intuition.

Ollama is *also* chosen for reasons that are not cost — data residency, model choice, no per-token metering, no provider dependency. Those can justify it below the crossover. Say which reason applies; do not let "it's cheaper" go unexamined.

Additional Bedrock-side levers, in order of impact:

1. **Model selection.** Routing classification and extraction to a small fast model instead of a frontier model is typically an order-of-magnitude saving. Bigger than every infrastructure lever combined.
2. **Prompt caching** for long, stable system prompts and tool schemas — which agent loops have by construction, on every turn.
3. **Batch inference** for latency-tolerant bulk work.
4. **Context hygiene.** Agent loops resend history each turn; cost grows quadratically with conversation length unless trimmed or summarised.

> ⚠ Prompt-caching and batch-inference discount rates were not verified in this milestone. Confirm current rates before relying on them.

## 9.5 Governing the unbounded cost

The only cost in this platform that can grow without an engineer's involvement is token spend. It gets the strongest controls.

| Control | Where | Behaviour |
|---|---|---|
| **Per-agent token budget** | Model Gateway | Hard cap per run and per day |
| **Circuit breaker** | Model Gateway | Budget exceeded → refuse, emit `agent.budget.exceeded`, alarm |
| **Max iterations per agent run** | OpenClaw config | Bounds reasoning loops |
| **Cost attribution by `agent_run_id`** | CloudWatch EMF | `$/run`, `tokens/run`, `$/successful outcome` |
| **AWS Budgets + Cost Anomaly Detection** | Account | Catches what the above misses |
| **Cost allocation tags** | All resources | `platform`, `environment`, `component`, `agent` |

**The circuit breaker is a production requirement, not a nice-to-have.** An agent that retries a failing tool call in a loop, each iteration resending a growing context to a frontier model, can spend thousands of dollars overnight with nobody logged in. This is the platform's most likely expensive incident — more likely than a security breach, and it needs a hard stop rather than an alarm somebody reads on Monday.

The metric to steer by is **cost per successful outcome**, not cost per token. An agent that costs 3× more per run but succeeds twice as often on the first attempt is cheaper. Token-level optimisation without an outcome denominator optimises the wrong thing.

## 9.6 Cost traps

Specific, expensive, and easy to walk into:

| Trap | Cost | Avoidance |
|---|---|---|
| **EBS Fast Snapshot Restore left enabled** | **≈$0.75/DSU-hour per snapshot per AZ ≈ $540/mo each** | Enable on **one** snapshot, only in launch AZs. Alarm on FSR-enabled count. This can silently exceed the GPU spend it accelerates. |
| **Model weights pulled through NAT** | $0.045/GB, tens of GB per launch, every scale-up | S3 Gateway Endpoint (free). |
| **Bedrock traffic through NAT** | Per-GB on every token | `bedrock-runtime` interface endpoint. |
| **GPU fleet that never scales to zero** | ~$250/mo per idle Spot GPU | Idle-timeout scale-down; alarm on `desired > 0 && queue == 0`. |
| **`lowest-price` Spot allocation** | Interruption churn → wasted partial work | `capacity-optimized-prioritized`. |
| **Unbounded CloudWatch Logs retention** | Grows forever | Retention policy + lifecycle to S3/Glacier. |
| **Idle Ollama in dev over a weekend** | ~$25 | Scheduled scale-down; dev min=0. |
| **Runaway agent loop** | **Unbounded** | Circuit breaker (§9.5). |

The first and last rows are the two that produce a bill nobody can explain. Alarm on both.

## 9.7 What we chose *not* to optimise

- **Multi-AZ NAT in prod (~$33/mo extra).** Single-NAT saves the money and makes one AZ's egress depend on another AZ's health. Not worth it in prod. Taken in dev.
- **RDS Multi-AZ (~2× single-AZ).** Bought outright. n8n's workflow and credential store is the platform's second-most-painful data-loss event.
- **EFS over EBS for the Gateway.** More expensive per GB and slower. Bought because it converts an unrecoverable-by-automation AZ failure into a 3–5 minute relaunch ([07 — HA](07-scalability-and-ha.md)).
- **Savings Plans / Reserved Instances.** Deliberately deferred: the On-Demand baseline is small (three instances) and the architecture is young. Commit once the baseline is stable — probably Milestone 3. Committing early to a shape you are about to change is how you pay for instances you no longer run.
