# Cost — the reference

**Milestone 15.** What the platform costs to run, why it costs that and not more,
and the guardrails that keep it there. This is the operational companion to the
blog post,
[Cost Optimization Strategies for AI Platforms on AWS](docs/blog/cost-optimization-strategies-for-ai-platforms-on-aws.md),
and the diagrams in
[cost-optimization-diagrams.md](docs/architecture/cost-optimization-diagrams.md).

> **The one idea.** The cheapest resource is the one that is switched off, and the
> cheapest inference is the token you never send to a paid API. Almost every number
> below follows from those two sentences: **Spot** and a **scheduler** keep the
> compute off when idle; a **local model** answers the ordinary request for the
> price of electricity, and a **paid model** is the exception, not the default. This
> milestone adds no new way to spend money — it adds the guardrails that tell you
> when an optimisation has silently regressed.

---

## Contents

- [The shape of the bill](#the-shape-of-the-bill)
- [The five levers that already exist](#the-five-levers-that-already-exist)
- [Service-by-service](#service-by-service)
- [Ollama vs. Amazon Bedrock](#ollama-vs-amazon-bedrock)
- [The guardrails this milestone adds](#the-guardrails-this-milestone-adds)
- [What this milestone tuned](#what-this-milestone-tuned)
- [Estimated monthly cost](#estimated-monthly-cost)
- [Highest-spend areas, ranked](#highest-spend-areas-ranked)
- [Deploy it](#deploy-it)
- [Well-Architected](#well-architected)
- [Future improvements](#future-improvements)

> **On the numbers.** Every dollar figure here is a us-east-1 list price at the time
> of writing, meant to teach the *shape* of the bill, not to be quoted back to
> finance. Spot prices float, Bedrock prices change, and your usage is your own.
> Verify with the [AWS Pricing Calculator](https://calculator.aws) before you plan
> against these.

---

## The shape of the bill

An AI agent platform has an unusual cost profile: its expensive component does
nothing most of the time and then, briefly, does everything. A traditional web
service is sized for steady traffic; this one is sized for a burst — an agent run,
a batch of inference — surrounded by long idle. The whole cost strategy follows
from refusing to pay for the idle.

Three properties of *this* platform's architecture make that possible, and they
were decided in earlier milestones, not this one:

- **The compute is disposable and stateless** (Milestones 2–4). Nothing of value
  lives only on the instance, so the instance can be stopped, interrupted, or
  replaced without ceremony.
- **There is no always-on network tax.** The platform reaches the internet through
  an internet gateway and a tightened egress allow-list, **not a NAT gateway** — a
  deliberate omission that alone saves roughly **\$32/month** in gateway hours plus
  per-GB processing, the single most common surprise line on an AWS bill.
- **Inference has a free tier of its own**: a local model on the instance the
  platform already pays for. A request answered by Ollama adds no marginal cost.

## The five levers that already exist

This milestone is mostly documentation because the expensive decisions were made
correctly upstream. The levers, and where they live:

| Lever | Saves by | Milestone |
| --- | --- | --- |
| **EC2 Spot** | ~70% off On-Demand for the same instance, because the workload tolerates interruption | 3 ([SPOT.md](infra/SPOT.md)) |
| **Instance scheduler** | Stopping the instance when no one is using it — you pay for hours, not calendar months | 3 ([07-scheduler](infra/cloudformation/07-scheduler.yaml)) |
| **Custom AMI** | Booting in **2.5s instead of 76s**, so "stop it when idle" costs seconds to undo, not minutes — which is what makes aggressive scheduling practical | 4 ([AMI.md](infra/AMI.md)) |
| **Local-first routing** | Answering the ordinary request on Ollama, so Bedrock tokens are spent only when they buy something | 10 ([ROUTING.md](ROUTING.md)) |
| **arm64 everything** | Graviton Lambdas and instances cost ~20% less than x86 at equal performance | 2–15 |

The custom AMI is worth dwelling on as a *cost* control, not just a speed one. A
platform that takes 76 seconds to become useful cannot be stopped between requests —
the restart penalty makes "leave it running" the rational choice, and an always-on
t3.xlarge is ~\$120/month On-Demand whether or not anyone uses it. Cutting the boot
to 2.5 seconds is what lets the scheduler stop the box overnight and on weekends
without anyone noticing, and *that* is where the money is.

## Service-by-service

| Service | What it costs | The control |
| --- | --- | --- |
| **EC2 (t3.xlarge)** | On-Demand ~\$0.166/hr; **Spot typically ~\$0.05/hr**. The platform's largest single line. | Spot + scheduler + fast-boot AMI. Pay for hours used, at a third of the sticker. |
| **EBS (gp3, 30 GB root)** | ~\$0.08/GB-month → **~\$2.40/month**, billed whether the instance runs or is stopped. | gp3 (cheaper and faster than gp2); a right-sized 30 GB root; snapshots pruned by the AMI pipeline. |
| **Custom AMI snapshots** | ~\$0.05/GB-month per retained image. | `make ami-prune KEEP=3` deletes old images **and their snapshots** — the snapshot is where the cost hides. |
| **Lambda** | Effectively **\$0** here: a handful of low-frequency, 128 MB, arm64 functions well inside the free tier. | 128 MB memory, `provided.al2023` custom runtime (fast cold start), arm64. |
| **CloudWatch** | 3 dashboards (**first 3 free**), 13 alarms (**first 10 free**, then \$0.10 each), logs at \$0.50/GB ingest + \$0.03/GB-month store. | 14-day default log retention; EMF metrics that cost nothing the logs did not; alarms kept near the free-tier line. |
| **Amazon Bedrock** | Per-token, **on demand, \$0 when unused**. The one truly variable line. | Local-first routing; model selection; prompt discipline (below). |
| **S3** | \$0.023/GB-month Standard; requests fractions of a cent. Tiny for generated artifacts. | Intelligent-Tiering, lifecycle expiry of old versions, TLS-only, no public access. |
| **CloudTrail + KMS** (M14) | First management trail **free**; ~\$1/month for the trail's KMS key + modest S3/requests. | One trail, S3 data events **off** by default (they are billed per event). |
| **SNS, EventBridge, Budgets** | Rounding error: free tiers cover the platform's volume; Budgets is free for the first two. | — |
| **NAT gateway** | **\$0 — there isn't one.** | Internet gateway + egress allow-list + S3 gateway endpoint instead. |

## Ollama vs. Amazon Bedrock

The router (Milestone 10) already chooses between a local model on Ollama and a
managed model on Bedrock. Framed as a cost decision, the two are not competitors so
much as two different *cost structures*, and the right one depends on the request.

|  | **Ollama (local)** | **Amazon Bedrock (managed)** |
| --- | --- | --- |
| **Cost model** | Fixed: the EC2 hour you are already paying for. Marginal cost per request ≈ **\$0**. | Variable: per input + output token. Marginal cost per request > 0, no idle cost. |
| **Cheapest when** | The instance is already up and the volume is steady — amortise the hour over many requests. | Volume is spiky or rare — pay per token, nothing between bursts. |
| **Break-even intuition** | A busy hour of local inference costs one Spot-hour (~\$0.05) no matter how many requests. | The same hour on Bedrock costs per token — cheap for a few requests, dear for thousands. |
| **Hidden costs** | The idle instance (mitigated by the scheduler); model storage on EBS. | None fixed; but a runaway loop can run the bill up fast — hence anomaly detection. |
| **Also buys you** | Data never leaves the network; no per-request quota. | Frontier models too large to self-host; zero ops; elastic scale. |

The cost-optimal policy the platform encodes: **prefer Ollama when a suitable local
model exists and the instance is available; fall back to Bedrock when it is not,
when a larger model is genuinely required, or when configuration demands it.** The
expensive path is a deliberate exception, and Bedrock's cost, being per-token and
per-request, is best managed at the *prompt* — a shorter system prompt, a smaller
model where it suffices (Haiku-class before Sonnet-class), and not re-asking a
question already answered — rather than at the infrastructure layer.

## The guardrails this milestone adds

Optimisations regress silently. A scheduler misconfiguration leaves the instance
running all weekend; a loop bug hammers Bedrock overnight; someone attaches a NAT
gateway "just to test." None of these trip a functional alarm — the platform works
fine, it just costs more. So this milestone's actual *code* is a set of smoke
detectors, in [`12-cost.yaml`](infra/cloudformation/12-cost.yaml):

- **An AWS Budget** — a monthly cost ceiling with three notifications: an early
  warning at 80% of actual spend, a 100%-of-actual notice, and — the one that
  matters — a **forecasted** breach, which fires while there is still month left to
  act. Free for the first two budgets.
- **Cost Anomaly Detection** — per-**service** monitoring that flags spend which
  suddenly looks nothing like its own history. This is the control that most
  directly matches the platform's failure modes: it names the service that got
  expensive (EC2 did not stop, Bedrock will not stop) rather than only the total.
  Free; requires Cost Explorer enabled on the account.
- **A CloudWatch billing alarm** — a coarse account-wide backstop on estimated
  charges, in the same console as every other alarm. (The `AWS/Billing` metric
  lives only in us-east-1, so the stack is meant for that region.)

All three publish to **one** SNS topic, separate from the monitoring and security
topics, because "you are about to overspend" is a calmer, different conversation —
often for a different person — than "the platform is down."

## What this milestone tuned

Beyond the guardrails, two low-risk, high-justification changes to existing infra:

- **The events dispatch Lambda moved to arm64** ([`05-events.yaml`](infra/cloudformation/05-events.yaml)).
  It was the one function still defaulting to x86_64; Graviton is ~20% cheaper at
  equal performance, and it now matches every other Lambda in the platform.
- **The artifact bucket adopts S3 Intelligent-Tiering** ([`04-storage.yaml`](infra/cloudformation/04-storage.yaml)).
  The correct default for generated artifacts of unknown access pattern: S3 moves
  objects between access tiers automatically with **no retrieval fees**, and objects
  under 128 KB are never auto-tiered and never incur the monitoring fee — so the
  rule can only save money, never add it.

Everything else was already right, and this document says so rather than inventing
churn to look busy. The Lambdas were already 128 MB / custom-runtime / arm64; the
log retention was already a sane 14 days; there is already no NAT gateway. A cost
review that changes everything it touches is not a cost review, it is a rewrite.

## Estimated monthly cost

Three illustrative profiles. Assumptions are stated because they are the whole
answer — the same platform is \$25 or \$400 a month depending only on how long the
instance runs and how much Bedrock it calls.

### Development — ~\$25–35/month

*One engineer. Instance scheduled off overnight and on weekends (~8h/weekday on
Spot). Inference is almost entirely local; Bedrock barely touched.*

| Line | Est. |
| --- | --- |
| EC2 t3.xlarge Spot (~176 h × \$0.05) | ~\$9 |
| EBS 30 GB gp3 (24×7) | ~\$2.40 |
| AMI snapshots (KEEP=3) | ~\$1.50 |
| CloudWatch (within/near free tier) | ~\$3 |
| CloudTrail KMS key + S3 | ~\$1.50 |
| S3 / SNS / Lambda / EventBridge | <\$1 |
| Bedrock (occasional) | ~\$1–5 |
| **Total** | **~\$25–35** |

### Small production — ~\$70–130/month

*Low but real traffic. Instance up ~12h/day every day, Spot. A meaningful minority
of requests fall back to Bedrock (Haiku-class).*

| Line | Est. |
| --- | --- |
| EC2 t3.xlarge Spot (~360 h × \$0.05) | ~\$18 |
| EBS + AMI snapshots | ~\$5 |
| CloudWatch (more logs, all alarms) | ~\$10–20 |
| Bedrock (steady, small-model) | ~\$25–70 |
| CloudTrail / S3 / misc | ~\$5–10 |
| **Total** | **~\$70–130** |

### Medium production — ~\$250–450/month

*Sustained use. Instance effectively 24×7 (On-Demand or persistent Spot). Regular
Bedrock use including larger models. More log volume, longer retention.*

| Line | Est. |
| --- | --- |
| EC2 t3.xlarge ~24×7 (Spot→On-Demand mix) | ~\$60–120 |
| EBS + snapshots | ~\$8 |
| CloudWatch (logs, dashboards, alarms) | ~\$25–50 |
| Bedrock (larger models, higher volume) | ~\$120–250 |
| CloudTrail / S3 / SNS / misc | ~\$10–20 |
| **Total** | **~\$250–450** |

> **GPU note.** These assume CPU inference on `t3.xlarge`, which is what the platform
> deploys today. Real-time inference on larger models eventually wants a GPU
> instance (e.g. `g5.xlarge`, ~\$1/hr On-Demand, ~\$0.30 Spot). At 24×7 that is a
> \$220–730/month line by itself, which is exactly why the scheduler and Spot matter
> more, not less, as the instance gets more expensive — and why Bedrock, with zero
> idle cost, wins outright for spiky GPU-class workloads.

## Highest-spend areas, ranked

Where to look first when the budget alarm fires, in order:

1. **EC2 instance-hours.** Almost always the answer. Check the scheduler is
   stopping the instance and that nothing left it running. A GPU instance makes this
   dominant.
2. **Bedrock tokens.** The variable line. A loop that will not stop, or a switch to
   a larger model, shows here — and is what Cost Anomaly Detection is tuned to catch.
3. **CloudWatch logs and alarms.** Grows quietly with retention and log verbosity.
   The 11th alarm and every dashboard past the third start costing.
4. **EBS + snapshots.** Fixed and small, but paid even while stopped; unpruned AMIs
   accumulate.
5. **Everything else.** Rounding error by design.

## Deploy it

The cost stack is pure CloudFormation — no Go to build — and independent of every
other stack.

```sh
# Deploy the budget, anomaly detection, and billing alarm.
make -C infra cost COST_EMAIL=you@example.com BUDGET_LIMIT=50
# then confirm the SNS subscription in your inbox — nothing is delivered until you do

# If Cost Explorer is not yet enabled on the account, skip anomaly detection:
make -C infra cost COST_EMAIL=you@example.com ENABLE_ANOMALY=false
```

Cost Anomaly Detection requires **Cost Explorer** to be enabled once, for free, in
the Billing console. The billing alarm requires deployment in **us-east-1** (the
project default), where the `AWS/Billing` metric is published.

## Well-Architected

Mapped to the Cost Optimization pillar.

| Principle | How the platform honours it |
| --- | --- |
| **Adopt a consumption model** | Spot + scheduler mean you pay for instance-hours actually used; Bedrock and Lambda are pay-per-request with no idle cost. |
| **Measure overall efficiency** | Budgets, Cost Anomaly Detection, and a billing alarm make spend visible and attributable per service. |
| **Stop spending on undifferentiated heavy lifting** | Managed services (Lambda, Bedrock, EventBridge, SNS) with generous free tiers replace self-run infrastructure; no NAT gateway. |
| **Analyze and attribute expenditure** | Every resource is tagged `Project`/`Environment`/`Milestone`; anomaly detection breaks spend down by service. |
| **Select the right pricing model** | Spot for interruptible compute; arm64/Graviton for a flat discount; local inference to avoid per-token cost where it does not buy anything. |
| **Right-size** | 128 MB Lambdas, a 30 GB root, `t3.xlarge` for CPU inference — each matched to its load, not over-provisioned "to be safe." |

## Future improvements

- **Inference-triggered lifecycle** — start the instance on demand when a request
  arrives and stop it after an idle timeout, so it is up *only* while working,
  rather than on a fixed clock. A natural extension of the scheduler.
- **EC2 Savings Plans / Reserved capacity** — once a steady baseline of instance
  usage is established, a 1-year Compute Savings Plan trades commitment for a
  further ~30% off the Spot-ineligible portion.
- **Bedrock prompt caching and batch** — cache long, stable system prompts, and use
  Bedrock's batch mode for latency-tolerant work at a lower per-token rate.
- **S3 Storage Lens and Cost Explorer reports** — turn the anomaly signal into a
  standing weekly report, so trends are seen before they become alarms.
- **Per-environment budgets** — separate dev/staging/prod ceilings once the platform
  runs in more than one environment.
