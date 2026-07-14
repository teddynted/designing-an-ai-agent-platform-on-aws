# Spot Interruption Diagrams — Milestone 3

> **Milestone 3 — EC2 Spot Instances.**
> These diagrams describe the interruption handling defined in
> [`infra/cloudformation/08-spot.yaml`](../../infra/cloudformation/08-spot.yaml)
> (the account-side handlers) and
> [`03-compute.yaml`](../../infra/cloudformation/03-compute.yaml) (the on-host
> drain agent), with the Go handlers in [`infra/lambda`](../../infra/lambda).
> They accompany the blog post,
> [Reducing AI Infrastructure Costs with EC2 Spot Instances](../blog/reducing-ai-infrastructure-costs-with-ec2-spot-instances.md),
> and the operational reference, [SPOT.md](../../infra/SPOT.md).

> **This is a snapshot of Milestone 3.** It is kept as it was written — the record of a
> decision at a point in time. For what is deployed **today**, see
> **[The Platform As Built](current-architecture.md)**, the living diagram.

Five diagrams. They share the vocabulary and colour key of the
[Milestone 2 diagrams](infrastructure-diagrams.md) (compute = orange,
storage = green).

## 1. The two halves of interruption handling

The single most important thing to see here is that **the reclaim decision leaves
AWS along two independent paths, and only one of them can save your work.**

The left path never leaves the instance: the drain agent sees the notice in the
instance metadata service and has about two minutes to stop the workload and get
its output into S3. The right path never touches the instance: EventBridge
delivers the same fact to Lambdas that count it, log it, and tell the rest of the
platform.

A Lambda in the account cannot flush a half-written file on a disk it cannot
reach. That is why both halves exist.

```mermaid
flowchart TB
    reclaim["AWS reclaims the capacity"]

    subgraph instance["On the instance — saves the work"]
        imds["Instance metadata<br/>spot/instance-action"]
        drain["spot-drain<br/>(systemd, polls every 5s)"]
        work["Workload<br/>Ollama · n8n · agents"]
    end

    subgraph account["In the account — tells the platform"]
        default["Default event bus"]
        rules["5 EventBridge rules"]
        l1["Lambda<br/>spot-interruption"]
        l2["Lambda<br/>spot-statechange"]
        bus["Platform event bus<br/>(future subscribers)"]
    end

    s3["S3 artifact bucket<br/>durable"]
    cw["CloudWatch<br/>logs + metrics"]

    reclaim -->|"~2 min notice"| imds
    reclaim -->|"event"| default
    imds --> drain
    drain -->|"1 · stop the workload"| work
    drain -->|"2 · save the output"| s3
    drain -->|"3 · leave a marker"| s3
    default --> rules
    rules --> l1 & l2
    l1 & l2 -->|"re-publish"| bus
    l1 & l2 -.-> cw
    drain -.-> cw

    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef store fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    class default,rules,l1,l2,bus,drain,work aws
    class s3,cw,imds store
```

## 2. The interruption timeline

Two minutes, spent. The window opens when AWS writes the notice; it closes when
the instance is gone, whether or not anything finished.

Note what the Lambda path is *not* doing: it is not on the critical path of
saving anything. It runs in parallel, and it would be equally correct if it ran a
minute later.

```mermaid
sequenceDiagram
    autonumber
    participant AWS as EC2 / Spot market
    participant IMDS as Instance metadata
    participant Agent as spot-drain (on the instance)
    participant Work as Workload
    participant S3
    participant EB as EventBridge
    participant L as Lambda (interruption)
    participant CW as CloudWatch

    loop every 5s, for the whole healthy life of the instance
        Agent->>IMDS: GET spot/instance-action
        IMDS-->>Agent: 404 — nothing to do
    end

    Note over AWS: capacity is reclaimed

    AWS->>IMDS: notice (T+0s · ~120s left)
    AWS->>EB: EC2 Spot Instance Interruption Warning

    par The half that saves the work
        Agent->>IMDS: GET spot/instance-action
        IMDS-->>Agent: {"action":"terminate"}
        Agent->>Work: systemctl stop
        Work-->>Agent: stopped (nothing is writing now)
        Agent->>S3: sync artifacts/
        Agent->>S3: put interruption.json
        Note over Agent: exits 0 — and stays exited
    and The half that tells the platform
        EB->>L: invoke
        L->>AWS: DescribeInstances (is this ours?)
        L->>CW: PutMetricData InterruptionWarnings
        L->>EB: PutEvents → platform bus
    end

    AWS->>Work: instance terminated (T+120s)
    Note over S3: the work survives the instance
```

## 3. Event routing

Five rules, two functions, one metric each. Everything is on the **default** bus,
because that is the only bus AWS services publish to — a rule matching
`source: aws.ec2` on a custom bus is valid, deploys cleanly, and never fires.

The rules cannot filter by tag (EC2's events carry none), so they match every
instance in the region and the **handlers** decide ownership by reading the
instance's `Project` and `Environment` tags.

```mermaid
flowchart LR
    subgraph src["aws.ec2 → default event bus"]
        e1["EC2 Spot Instance<br/>Interruption Warning"]
        e2["EC2 Instance<br/>Rebalance Recommendation"]
        e3["State-change:<br/>running"]
        e4["State-change:<br/>stopped"]
        e5["State-change:<br/>terminated"]
    end

    subgraph fn["Lambda"]
        l1["spot-interruption"]
        l2["spot-statechange"]
    end

    subgraph out["Outputs"]
        m1["InterruptionWarnings"]
        m2["RebalanceRecommendations"]
        m3["InstancesLaunched"]
        m4["InstancesStopped"]
        m5["InstancesTerminated"]
        bus["Platform bus<br/>(re-published)"]
    end

    e1 --> l1
    e2 --> l1
    e3 --> l2
    e4 --> l2
    e5 --> l2

    l1 -->|"owned?"| m1 & m2
    l2 -->|"owned?"| m3 & m4 & m5
    l1 & l2 --> bus

    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef store fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    class e1,e2,e3,e4,e5,l1,l2,bus aws
    class m1,m2,m3,m4,m5 store
```

## 4. Does this workload belong on Spot?

The decision is not about how important the work is. It is about **what it costs
to lose two minutes of it** — and whether anything is left behind that cannot be
rebuilt.

```mermaid
flowchart TB
    start(["A workload needs compute"]) --> state{"Does it hold state<br/>you cannot rebuild?"}
    state -->|yes| od1["On-Demand.<br/>n8n, databases, the control plane."]
    state -->|no| drain{"Can it stop cleanly<br/>in under 2 minutes?"}
    drain -->|no| od2["On-Demand,<br/>or make it checkpoint."]
    drain -->|yes| user{"Is a user waiting<br/>on this request?"}
    user -->|yes| fallback{"Is there a managed<br/>fallback behind it?"}
    fallback -->|no| od3["On-Demand.<br/>An interruption is an outage."]
    fallback -->|yes| spot1["Spot, with the fallback.<br/>Hybrid routing (M10)."]
    user -->|no| spot2["Spot.<br/>Batch inference, embeddings,<br/>indexing, blog generation."]

    classDef good fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    classDef bad fill:#D13212,stroke:#7D1D0C,color:#FFFFFF
    class spot1,spot2 good
    class od1,od2,od3 bad
```

The pattern it encodes: **the plane that does the work goes on Spot; the plane
that remembers the work does not.**

## 5. What the discount buys, and what it costs

Spot is the same hardware, the same network, the same AMI. You are renting
capacity AWS has already built and cannot currently sell, on the condition that
it can have it back. The discount is the price of that condition.

```mermaid
flowchart LR
    subgraph od["On-Demand"]
        od1["~$0.166/hr · t3.xlarge"]
        od2["~$120/month"]
        od3["Never interrupted"]
    end

    subgraph sp["Spot — the default"]
        sp1["~$0.05/hr · t3.xlarge"]
        sp2["~$36/month"]
        sp3["~2 minutes' notice,<br/>then it is gone"]
    end

    subgraph pay["What an interruption actually costs"]
        p1["The work in flight<br/>(redone)"]
        p2["The cold start of the<br/>replacement (M4 shrinks this)"]
    end

    od --> |"–70%"| sp
    sp --> pay

    classDef good fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    classDef warn fill:#FF9900,stroke:#232F3E,color:#232F3E
    class sp1,sp2,od1,od2,od3,sp3 warn
    class p1,p2 good
```

On a `t3.xlarge` that is ~$84/month. On a `g5.xlarge` GPU it is ~$510/month, and
on a `g5.12xlarge` ~$2,850/month — which is where the discount stops being a
rounding error and starts deciding whether a project is affordable at all.

The trade is only good when the right-hand box is small. A 20-minute batch job
interrupted once a week is a trivially good deal; a 30-hour fine-tune that cannot
checkpoint is a terrible one. See
[When not to use Spot](../../infra/SPOT.md#when-not-to-use-spot).
