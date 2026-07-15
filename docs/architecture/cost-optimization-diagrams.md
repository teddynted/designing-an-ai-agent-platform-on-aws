# Cost Optimization Diagrams — Milestone 15

> **Milestone 15 — Cost Optimization.**
> These diagrams show where the platform's architecture reduces operational cost,
> and the guardrails in
> [`infra/cloudformation/12-cost.yaml`](../../infra/cloudformation/12-cost.yaml)
> that keep it that way. They accompany the blog post,
> [Cost Optimization Strategies for AI Platforms on AWS](../blog/cost-optimization-strategies-for-ai-platforms-on-aws.md),
> and the reference, [COST.md](../../COST.md).
>
> **The one idea.** The cheapest resource is switched off; the cheapest inference is
> the token never sent to a paid API. Every arrow below serves one of those two.

## Contents

- [1. Cost-optimized infrastructure](#1-cost-optimized-infrastructure)
- [2. AI routing as a cost decision](#2-ai-routing-as-a-cost-decision)
- [3. Instance lifecycle: paying for hours, not months](#3-instance-lifecycle-paying-for-hours-not-months)
- [4. Cost guardrails](#4-cost-guardrails)

## 1. Cost-optimized infrastructure

Each labelled edge is a place the design chooses the cheaper option. The largest
one is invisible: there is **no NAT gateway** — the internet gateway, egress
allow-list, and S3 gateway endpoint replace it for ~\$32/month saved.

```mermaid
flowchart TB
    subgraph vpc["VPC — no NAT gateway (~$32/mo saved)"]
        igw["Internet Gateway<br/>(free) + egress allow-list"]
        s3ep["S3 Gateway Endpoint<br/>(free, on-backbone)"]
        ec2["EC2 t3.xlarge<br/><b>Spot ~$0.05/hr</b> vs $0.166 On-Demand"]
        ami["Custom AMI<br/>boot 2.5s (was 76s)"]
    end

    sched["EventBridge Scheduler<br/>+ Lambda (arm64, 128MB)"]
    cw["CloudWatch<br/>3 dashboards free · 14-day logs"]
    s3["S3 artifacts<br/>Intelligent-Tiering"]

    ami -. "fast boot makes<br/>stop-when-idle practical" .-> ec2
    sched -- "stop overnight/<br/>weekends" --> ec2
    ec2 --> s3ep --> s3
    ec2 --> igw
    ec2 -- "logs/metrics" --> cw

    classDef save fill:#1b5e20,stroke:#2e7d32,color:#fff;
    class ec2,ami,s3ep,igw save;
```

## 2. AI routing as a cost decision

The router (Milestone 10) already chooses a provider per request. Seen as cost, it
is choosing between a **fixed** cost structure (the EC2 hour already paid for) and a
**variable** one (per Bedrock token). Local is preferred; the paid path is a
deliberate, bounded exception.

```mermaid
flowchart TB
    req["Inference request"]
    router{"Router<br/>(internal/router)"}

    subgraph local["Ollama — local (preferred)"]
        cond1{"Suitable local model<br/>AND instance up<br/>AND latency OK?"}
        ollama["Ollama on EC2<br/><b>marginal cost ≈ $0</b><br/>data never leaves network"]
    end

    subgraph managed["Amazon Bedrock — fallback"]
        bedrock["Bedrock<br/><b>per-token, $0 when idle</b><br/>frontier models · elastic"]
    end

    req --> router --> cond1
    cond1 -- yes --> ollama
    cond1 -- "no / larger model<br/>needed / configured" --> bedrock

    classDef pref fill:#1b5e20,stroke:#2e7d32,color:#fff;
    classDef fall fill:#4a148c,stroke:#6a1b9a,color:#fff;
    class ollama pref;
    class bedrock fall;
```

## 3. Instance lifecycle: paying for hours, not months

The most expensive line on the bill is EC2 instance-hours, and the whole strategy is
to minimise them without hurting availability. The custom AMI's 2.5-second boot is
what makes the "stopped" state cheap to leave — a 76-second boot would make
always-on the rational choice.

```mermaid
stateDiagram-v2
    [*] --> Stopped
    Stopped: EC2 Stopped\n(pay only EBS ~$2.40/mo)
    Running: EC2 Running\n(pay Spot ~$0.05/hr)

    Stopped --> Booting: scheduler start\nOR demand (future)
    Booting: Boot from custom AMI\n~2.5s, Ollama pre-installed
    Booting --> Running: health check passes
    Running --> Draining: scheduler stop\nOR Spot interruption\nOR idle timeout (future)
    Draining: Drain agent\nflushes work to S3
    Draining --> Stopped

    note right of Running
        Spot interruption (M3) is handled
        gracefully: drain, then replace.
        Interruptible IS what earns the
        ~70% Spot discount.
    end note
```

## 4. Cost guardrails

The stack this milestone adds. It optimises nothing itself — it is the smoke
detector that tells you an optimisation has silently regressed (an instance that
did not stop, a loop that will not stop calling Bedrock). Three signals, one topic,
one human.

```mermaid
flowchart TB
    subgraph ce["Billing & Cost Management"]
        budget["AWS Budget<br/>monthly ceiling<br/>80% · 100% · <b>forecasted</b>"]
        anomaly["Cost Anomaly Detection<br/>per-SERVICE, names what<br/>got expensive"]
    end
    billing["CloudWatch billing alarm<br/>EstimatedCharges (us-east-1)"]

    topic(["Cost SNS topic<br/>(separate from monitoring/security)"])
    human["Bill payer<br/>(email)"]

    budget -- "SNS (topic policy:<br/>budgets.amazonaws.com)" --> topic
    anomaly -- "SNS (topic policy:<br/>costalerts.amazonaws.com)" --> topic
    billing --> topic
    topic --> human

    classDef guard fill:#e65100,stroke:#ef6c00,color:#fff;
    class budget,anomaly,billing guard;
```
