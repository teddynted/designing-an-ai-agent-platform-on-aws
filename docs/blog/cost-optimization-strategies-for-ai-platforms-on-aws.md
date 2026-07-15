# Cost Optimization Strategies for AI Platforms on AWS

> **Milestone 15 — Cost Optimization.**
> This milestone makes the platform cheap to *leave running* — and, more usefully,
> cheap to leave running by accident. It reviews every component's cost, tightens the
> two that had slack (a Lambda still on x86, a bucket with no tiering), and adds the
> FinOps guardrails the platform was missing: an AWS Budget, Cost Anomaly Detection,
> and a billing alarm ([`12-cost.yaml`](../../infra/cloudformation/12-cost.yaml)), plus
> the reference doc ([COST.md](../../COST.md)) and diagrams. It does **not** re-architect
> anything that was already cheap — most of this platform's cost decisions were made
> correctly in earlier milestones, and the honest move is to say so.

*Audience: engineers who have opened an AWS bill expecting \$30 and found \$300, traced
it to one resource nobody remembered leaving on, and resolved — this time for real — to
put a tripwire on the thing before it happens again.*

---

## Contents

- [The bill has an unusual shape](#the-bill-has-an-unusual-shape)
- [The cheapest resource is the one switched off](#the-cheapest-resource-is-the-one-switched-off)
- [The custom AMI is a cost control wearing a performance costume](#the-custom-ami-is-a-cost-control-wearing-a-performance-costume)
- [The most expensive line is the one that is not there](#the-most-expensive-line-is-the-one-that-is-not-there)
- [Ollama vs. Bedrock is a choice between two cost structures](#ollama-vs-bedrock-is-a-choice-between-two-cost-structures)
- [Optimisations regress silently, so build tripwires](#optimisations-regress-silently-so-build-tripwires)
- [The two things that actually needed tuning](#the-two-things-that-actually-needed-tuning)
- [What it costs, honestly](#what-it-costs-honestly)
- [Lessons learned](#lessons-learned)
- [What comes next](#what-comes-next)

## The bill has an unusual shape

Most cost-optimization advice assumes a service sized for steady traffic: right-size
the instances, buy reservations for the baseline, autoscale the peaks. An AI agent
platform breaks that assumption. Its expensive component — the inference compute —
does nothing most of the time and then, briefly, does everything. There is no steady
baseline to reserve; there is a long flat idle punctuated by bursts.

That shape dictates the whole strategy, and it is a liberating one: if the expensive
thing is idle most of the time, **the biggest optimization is not paying for the
idle.** Everything in this milestone is a variation on that sentence. You do not make
the burst cheaper so much as you refuse to pay between bursts.

## The cheapest resource is the one switched off

The platform's largest single line is EC2 instance-hours, and it is attacked three
ways, none of them new to this milestone:

- **Spot** buys the same `t3.xlarge` for roughly a third of the On-Demand price —
  about \$0.05/hour against \$0.166 — in exchange for tolerating interruption, which
  the platform's disposable, stateless compute already does (that was Milestone 3).
- **A scheduler** stops the instance overnight and on weekends. You are billed for
  instance-hours, not calendar months, and an instance nobody is using at 3am is pure
  waste (Milestone 3's `07-scheduler`).
- **arm64** everywhere — Graviton instances and Lambdas — takes ~20% off at equal
  performance, a discount with no trade-off to weigh.

The interesting one is Spot, because engineers often reach for it and then quietly
fall back to On-Demand the first time an interruption bites. The reason it works here
is that the platform did the unglamorous work *first*: a drain agent that flushes
in-flight work to S3 on the two-minute interruption warning, EventBridge rules that
notice, and a replacement that boots fast. **Interruptible is not a limitation you
tolerate to get the discount — it is the property that earns it.** A platform that
cannot survive losing its instance has no business asking for a 70% discount for
promising it can.

## The custom AMI is a cost control wearing a performance costume

Milestone 4 cut the instance's boot time from 76 seconds to 2.5 by baking Ollama, the
CloudWatch agent, and the dependencies into a custom AMI. It was framed as a
performance win. It is at least as much a cost win, and the mechanism is worth making
explicit because it is not obvious.

A scheduler that stops your instance is only usable if starting it back up is cheap —
not in dollars, in *friction*. If the box takes 76 seconds to become useful, "stop it
between uses" is a bad trade: the restart penalty is long enough that the rational
choice is to leave it running, and an always-on `t3.xlarge` is about \$120/month On-
Demand whether or not anyone touches it. Cut the boot to 2.5 seconds and the calculus
flips — stopping the instance overnight costs the next user two and a half seconds,
which no one notices, and saves fourteen instance-hours, which the bill very much
does. **The AMI is what makes aggressive scheduling politically possible**, and the
scheduling is where the money is.

## The most expensive line is the one that is not there

Open a random AWS bill and there is a good chance the biggest surprise is a NAT
gateway: ~\$32/month in hours before it moves a single byte, then per-GB on
everything it does. Multi-AZ, it is three of those.

This platform has none. Its compute reaches the internet through an internet gateway
(free) and a tightened egress allow-list, and reaches S3 over a gateway VPC endpoint
(also free, and on the AWS backbone). This was a security decision in Milestone 14 —
a box running untrusted content should not have an open outbound path — but it is a
first-class cost decision too. The cheapest line item is the one you designed out of
existence, and it never shows up on a bill precisely because it was never there.
Optimizations you can *see* on the bill are the easy half; the ones that matter most
are often invisible, because their entire value is a charge that never appears.

## Ollama vs. Bedrock is a choice between two cost structures

The router from Milestone 10 already picks between a local model on Ollama and a
managed model on Amazon Bedrock. This milestone adds no code to it — but it reframes
the choice, because seen through a cost lens the two providers are not competitors of
different quality, they are **two different cost structures**, and the cheap one
depends entirely on the request.

Ollama's cost is *fixed*: it runs on the EC2 hour you are already paying for, so the
marginal cost of one more local request is essentially zero. A busy hour of local
inference costs one Spot-hour whether it serves ten requests or ten thousand. Its
hidden cost is the idle instance — which is exactly what Spot and the scheduler exist
to contain.

Bedrock's cost is *variable*: per input and output token, nothing between bursts. Its
marginal cost is always positive but its idle cost is zero, which makes it the cheap
option precisely when Ollama is dear — spiky or rare traffic, where amortising an EC2
hour makes no sense, or a model too large to self-host.

So the cost-optimal policy is the one the platform already encodes: **prefer Ollama
when a suitable local model exists and the instance is up; fall back to Bedrock when
it is not, when a larger model is genuinely needed, or when configuration says so.**
And because Bedrock's cost is per-token, it is managed at the *prompt*, not the
infrastructure — a leaner system prompt, a Haiku-class model where it suffices instead
of reflexively reaching for Sonnet-class, and not paying to re-answer a question you
already answered. Infrastructure tuning cannot save you from a wasteful prompt.

## Optimisations regress silently, so build tripwires

Here is the failure mode that actually costs money, and it is not a bad architecture —
it is a good architecture that quietly stopped being followed. The scheduler gets a
bad cron edit and the instance runs all weekend. A loop bug hammers Bedrock overnight.
Someone attaches a NAT gateway "just to test" and forgets. In every case the platform
*works perfectly*. No health check fails, no alarm fires, no page goes out. It just
costs three times what it should, and you find out on the first of the month.

Functional monitoring will never catch this, because nothing is functionally wrong. So
the actual code this milestone ships is a set of financial smoke detectors, in
`12-cost.yaml`:

- **An AWS Budget** with three notifications — an early warning at 80% of spend, a
  100% notice, and the one that earns its keep: a **forecasted** breach, which fires
  while there is still month left to change course rather than after the money is
  gone. Free for the first two budgets.
- **Cost Anomaly Detection**, monitoring per **service**, so the alert says *EC2*
  suddenly looks nothing like itself, or *Bedrock* does — it names the culprit, not
  just the total. This is the control that most precisely matches the platform's
  failure modes, and it is free (given Cost Explorer is enabled).
- **A CloudWatch billing alarm** as a coarse account-wide backstop, in the same
  console as every other alarm.

All three publish to one SNS topic, deliberately separate from the monitoring and
security topics. "You are about to overspend" is a calmer conversation than "the
platform is down," and it often needs to reach a different person — the one who pays
the bill, not the one carrying the pager. Routing cost alerts into the on-call channel
is how they get muted.

```yaml
# The forecasted-breach notification — the one that warns you in time.
- Notification:
    NotificationType: FORECASTED
    ComparisonOperator: GREATER_THAN
    Threshold: 100
    ThresholdType: PERCENTAGE
  Subscribers:
    - SubscriptionType: SNS
      Address: !Ref CostTopic
```

One detail that bit during development, preserved as a comment in the template: it is
tempting to scope the budget to the platform's resources with a tag filter. Do that
and the budget silently reports \$0 until the tag is *activated* as a cost-allocation
tag in the Billing console — a guardrail that guards nothing, and gives no error to
tell you so. For a dedicated project account, an account-wide budget is equivalent and
cannot fail that way. The safest guardrail is the one that cannot silently break.

## The two things that actually needed tuning

A cost review is judged as much by what it leaves alone as by what it changes. The
Lambdas were already 128 MB, arm64, custom-runtime — near-optimal. Log retention was
already a sane 14 days. There was already no NAT gateway. Changing those to look busy
would be churn, and churn in infrastructure is its own cost.

Two things had genuine slack:

- **One Lambda was still x86.** The event dispatcher in `05-events` never specified an
  architecture, so it defaulted to x86_64 while every other function in the platform
  ran arm64. Graviton is ~20% cheaper at equal performance and the code is not
  architecture-specific, so it moved. A small, unambiguous win.
- **The artifact bucket had no tiering.** It now uses S3 Intelligent-Tiering, which is
  the correct default for generated artifacts whose access pattern is unknown: S3
  moves objects between frequent- and infrequent-access tiers automatically, with **no
  retrieval fees**, and objects under 128 KB are never tiered and never charged the
  monitoring fee. It is a rule that can only save money, never add it — which is the
  only kind of default worth setting without knowing the workload.

## What it costs, honestly

Three profiles, because the same platform is \$25 or \$400 a month depending only on
how long the instance runs and how much Bedrock it calls:

| Profile | Assumptions | Estimate |
| --- | --- | --- |
| **Development** | Instance scheduled off nights/weekends (~8h/weekday Spot); inference almost all local | **~\$25–35/mo** |
| **Small production** | Instance ~12h/day Spot; a minority of requests fall back to a small Bedrock model | **~\$70–130/mo** |
| **Medium production** | Instance ~24×7; regular Bedrock incl. larger models; more logs | **~\$250–450/mo** |

Full line-item breakdowns, the Ollama/Bedrock break-even, and a ranked list of where
to look first when the budget alarm fires are in [COST.md](../../COST.md). Every figure
is a us-east-1 list price meant to teach the shape of the bill, not to be quoted at
finance — verify against your own usage.

## Lessons learned

- **The biggest optimization is not paying for idle.** For a bursty workload, hours-
  not-months beats every per-request tweak combined. Spot, the scheduler, and the
  fast-boot AMI are one strategy, not three features.
- **Some cost controls are invisible by design.** The NAT gateway that saves \$32/month
  never appears on the bill, because it was never created. The optimizations you can
  see are the easy ones.
- **Provider choice is a cost-structure choice.** Fixed (Ollama, the EC2 hour) vs.
  variable (Bedrock, per token) — pick by the request's shape, and manage the variable
  one at the prompt.
- **The failure mode that costs money is a silent regression, not an outage.** Build
  financial tripwires — budget, anomaly detection — because functional monitoring will
  never fire on "works fine, costs triple."
- **A cost review should change little.** If it rewrites everything it touches, the
  earlier milestones were wrong, or the review is inventing work. Ours changed a Lambda
  architecture and a bucket lifecycle, and documented why everything else was already
  right.

## What comes next

The natural next step is **inference-triggered lifecycle**: start the instance on
demand when a request arrives and stop it after an idle timeout, so it is up only while
actually working rather than on a fixed clock — the scheduler taken to its logical end.
Beyond that: a **Compute Savings Plan** once a steady usage baseline exists, **Bedrock
prompt caching and batch mode** to cut the per-token line, and **per-environment
budgets** as the platform grows past one environment. Each is a smaller win than the
ones already banked — which is the sign of a cost model that was mostly right from the
start.

The operational reference — the full breakdown, the estimates, the Well-Architected
mapping, and the deploy commands — is [COST.md](../../COST.md).
