# The Platform As Built

> **This is the living diagram.** It shows what is **actually deployed**, not what
> is planned. Every milestone updates this file; if it disagrees with the code, the
> file is wrong.
>
> **Last updated:** Milestone 5 — Self-hosted n8n Integration.
> **Deployed:** eight CloudFormation stacks + an image pipeline, in `dev`, plus the
> workflow-orchestration integration layer.
> **Not deployed by this repository:** n8n itself (it lives in
> [`self-hosted-n8n-on-aws`](../../README.md#related-repositories)), and any AI
> workload. See [What is not built](#what-is-not-built).

The other diagram sets are *snapshots* — each one froze at the milestone that wrote
it, and they are kept that way on purpose, as the record of a decision:

| | Scope |
| --- | --- |
| [diagrams.md](diagrams.md) | **M1** — the target architecture. Aspirational, and still mostly unbuilt. |
| [infrastructure-diagrams.md](infrastructure-diagrams.md) | **M2** — the CloudFormation foundation. |
| [spot-diagrams.md](spot-diagrams.md) | **M3** — Spot interruption handling. |
| [ami-diagrams.md](ami-diagrams.md) | **M4** — the custom AMI pipeline. |
| [n8n-diagrams.md](n8n-diagrams.md) | **M5** — the workflow-orchestration integration. |
| **this file** | **Everything, as it exists today.** |

## 1. Runtime architecture

The AWS service view — the same hand-authored, version-controlled SVG approach as
the [Milestone 1](aws-architecture.svg) and [Milestone 2](infrastructure-overview.svg)
diagrams, with the same nesting (Cloud → Region → VPC → subnet → security group) and
the same colour key.

Note how little of it is an "AI platform" yet. This is a foundation with no workload
on it, and the legend says so out loud rather than leaving you to infer it.

![The platform as built after Milestone 5: an internet gateway fronts a VPC public subnet whose default-deny security group contains an EC2 Spot instance launched from a custom AMI, with an encrypted root volume deleted on termination; the instance saves artifacts and drained work to S3 and ships its boot and drain logs to CloudWatch; EC2 lifecycle events land on the account default event bus where five EventBridge rules invoke two Go Lambdas that count them and re-publish onto the platform event bus; operators reach the instance only through SSM Session Manager and there is no inbound access; beneath the AWS account, drawn outside it, sits self-hosted n8n, deployed by a separate repository, which the platform triggers over HTTPS with a token, an idempotency key and a sanitised payload; no AI workload is deployed.](platform-as-built.svg)

The same thing as a flow view — useful for seeing the two independent paths out of
an interruption (the instance saves its own work; the account merely watches):

```mermaid
flowchart TB
    subgraph aws["AWS account · us-east-1"]
        subgraph vpc["VPC 10.20.0.0/16 · 01-network"]
            subgraph subnet["Public subnet · no NAT gateway"]
                subgraph sg["Security group — NO inbound rules"]
                    ec2["EC2 Spot instance · c5.xlarge<br/>booted from custom AMI (M4)<br/>─────────────<br/>Docker · Go · Node · Python<br/>spot-drain agent (M3)<br/>CloudWatch agent"]
                    ebs["Encrypted root EBS<br/>DeleteOnTermination: true"]
                end
            end
            igw["Internet gateway"]
        end

        subgraph regional["Regional services (outside the VPC)"]
            defbus["DEFAULT event bus<br/>where aws.ec2 events land"]
            rules["5 EventBridge rules (M3)"]
            l1["Lambda · spot-interruption"]
            l2["Lambda · spot-statechange"]
            platbus["Platform event bus · 05-events<br/>(the seam for later milestones)"]
            dispatch["Lambda · dispatch (placeholder)"]
            s3["S3 artifact bucket<br/>versioned · encrypted · TLS-only"]
            cw["CloudWatch<br/>logs + Spot metrics"]
            ami["Custom AMI + snapshot<br/>aiap-platform-v1.0.0"]
        end
    end

    ssm(["Operator → SSM Session Manager<br/>(no SSH, no key pair)"])

    wf["workflow.Service → Engine (M5)<br/>Go, in THIS repository.<br/>Not an AWS resource — a library,<br/>with no caller deployed yet (M12)"]
    n8n["self-hosted n8n<br/>ANOTHER REPOSITORY<br/>self-hosted-n8n-on-aws"]

    ec2 --- ebs
    subnet --> igw
    ssm -.-> ec2
    ami -.->|"launched from"| ec2
    ec2 -->|"artifacts + drained work"| s3
    ec2 -->|"boot log · drain log"| cw
    defbus --> rules --> l1 & l2
    l1 & l2 -->|"re-publish"| platbus --> dispatch
    l1 & l2 --> cw
    wf -->|"HTTPS + token · idempotency key<br/>· sanitised payload"| n8n
    wf -.->|"structured logs"| cw

    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef store fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    classDef ext fill:#E8E8E8,stroke:#666,color:#232F3E
    class ec2,l1,l2,dispatch,rules,defbus,platbus,wf aws
    class s3,cw,ami,ebs store
    class ssm,n8n ext
```

**The three facts this diagram is really carrying:**

1. **Nothing durable lives on the instance.** The root volume is deleted on
   termination, so anything that must survive goes to S3 — which is why the drain
   agent (M3) exists at all.
2. **The instance is reachable by nobody.** No inbound rules, no SSH key. Operators
   arrive through SSM Session Manager.
3. **n8n is drawn outside the account on purpose.** Milestone 5 added an
   *integration*, not infrastructure — it creates **no AWS resources**. The engine is
   deployed, versioned and backed up by
   [`self-hosted-n8n-on-aws`](../../README.md#related-repositories); this repository
   owns only the contract with it. And that integration has **no caller deployed yet**:
   the webhook handler is Milestone 12, so today it is exercised by
   [`cmd/workflow`](../../cmd/workflow) and its tests.

## 2. The stacks, and the one thing that is not a stack

```mermaid
flowchart LR
    subgraph core["make deploy — AWS CLI only"]
        n["01 · network"] --> i["02 · iam"] --> c["03 · compute"]
        i --> e["05 · events"]
        s["04 · storage"]
        o["06 · observability"]
    end

    subgraph addons["add-ons — need the Go toolchain"]
        sch["07 · scheduler<br/>(optional)"]
        sp["08 · spot"]
    end

    subgraph pipeline["NOT CloudFormation — a script"]
        build["make ami<br/>build-ami.sh"]
        img["Custom AMI<br/>(tagged, versioned)"]
    end

    e --> sp
    s -.->|"lambda zips"| sp
    s -.->|"lambda zips"| sch
    build --> img
    img -->|"AmiId parameter"| c

    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef store fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    class n,i,c,e,s,o,sch,sp aws
    class build,img store
```

**Why the AMI is not a stack.** CloudFormation has no resource type that *builds* an
image. It **consumes** one — that is the compute stack's `AmiId` parameter. Building
is a pipeline concern, consuming is an infrastructure concern, and **the AMI ID is
the interface between them.** Keeping that seam clean is why `03-compute` neither
knows nor cares how its image was made.

**And why n8n is not on this map at all.** It is neither a stack nor a script here —
it is *another repository's deployment*. Milestone 5 added the integration
([`internal/workflow`](../../internal/workflow), [`internal/n8n`](../../internal/n8n))
and **zero AWS resources**. If an n8n stack ever appears in `infra/cloudformation`,
the boundary this repository committed to has failed:

> *If a change affects more than one component, it belongs in the platform. If it
> affects exactly one, it belongs in that component's repository.*

An n8n version bump affects n8n. The shape of the JSON we send it affects everything
that sends it — so the payload, the auth header, the retry policy and the idempotency
key live here, and the servers do not.

## 3. The life of one instance

This is the diagram that ties the milestones together. Read it as one continuous
story: an instance is *built* (M4), *bought cheaply* (M3), *used*, *taken away*
(M3), and *replaced* — and no step requires a human.

```mermaid
sequenceDiagram
    autonumber
    participant Ops as make ami / deploy
    participant EC2
    participant I as Instance
    participant S3
    participant EB as EventBridge
    participant L as Lambda

    Note over Ops,I: M4 — the image is built once, on a throwaway On-Demand builder
    Ops->>EC2: build + tag AMI (~4.5 min, ~$0.01)

    Note over Ops,I: M2/M3 — buy it on Spot, ~70% off
    Ops->>EC2: deploy compute (AmiId = the new image)
    EC2->>I: launch Spot instance
    I->>I: boot in 6.2s — configure only, nothing to install
    Note over I: without the AMI this took 76s —<br/>more than half a Spot eviction window

    Note over I: ...the instance does work...

    Note over EC2,I: M3 — AWS wants the capacity back
    EC2->>I: interruption notice (IMDS) · ~120s left
    EC2->>EB: EC2 Spot Instance Interruption Warning

    par On the instance — saves the work
        I->>I: stop the workload units
        I->>S3: sync artifacts + interruption marker
    and In the account — tells the platform
        EB->>L: invoke
        L->>L: is it ours? (tag check)
        L->>EB: re-publish on the platform bus
        L->>EC2: count it (CloudWatch metric)
    end

    EC2->>I: terminated
    Note over S3: the work survives the instance
```

## 4. What each milestone added

```mermaid
flowchart TB
    m1["M1 · Architecture<br/>design only"] --> m2
    m2["M2 · CloudFormation<br/>VPC · IAM · EC2 · S3 · EventBridge · CloudWatch<br/>the instance is disposable"] --> m3
    m3["M3 · EC2 Spot<br/>~70% off + interruption handling<br/>drain agent + 5 rules + 2 Go Lambdas<br/>disposability is now SAFE"] --> m4
    m4["M4 · Custom AMIs<br/>76s → 6.2s boot · immutable images<br/>disposability is now CHEAP"] --> m5
    m5["M5 · n8n integration<br/>the platform can now ORCHESTRATE<br/>trigger · authenticate · retry · correlate<br/>(n8n itself lives in another repository)"] --> m6

    m6["M6 · OpenClaw<br/>the agent that does the work"]:::next

    classDef done fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    classDef next fill:#E8E8E8,stroke:#666,color:#232F3E,stroke-dasharray: 5 5
    class m1,m2,m3,m4,m5 done
```

The dependency between them is not arbitrary, and it is the argument of the whole
series so far:

- **M2 declared the instance disposable** (`DeleteOnTermination: true`). That was a
  claim, not yet a capability.
- **M3 made disposability safe** — a reclaimed instance no longer loses its work.
- **M4 made disposability cheap** — a replacement boots in seconds, so *replacing*
  becomes a viable strategy rather than a last resort.

Immutable infrastructure needs all three. Any one of them alone is a slogan.

## What is not built

Being explicit, because the [M1 target architecture](diagrams.md) shows a great deal
more than this:

| | Status |
| --- | --- |
| n8n **deployment** | ➡️ Not ours. Owned by [`self-hosted-n8n-on-aws`](../../README.md#related-repositories). This repository owns the **integration** — the contract, not the instance. |
| The webhook handler that calls the integration | ❌ Not built (M12). `cmd/workflow` is the reference caller in the meantime. |
| OpenClaw, Ollama | ❌ Not installed. The compute is still empty. |
| Any model inference | ❌ None. No GPU instance runs (cost + quota). |
| Bedrock / Claude routing | ❌ Not built. |
| Auto Scaling group | ❌ Still **one** instance. The launch template is ready for it (M19). |
| Private subnets / NAT | ❌ Public subnet only, deliberately (no $32/mo NAT). |
| Alarms + dashboards | ❌ Metrics and logs exist; nothing alerts on them (M15). |
| Scheduled AMI rebuilds | ❌ Manual. A baked image gets staler every day. |

The honest summary: **this is a well-built platform that can now ask for work to be
done, but has nothing yet to do it with.** Milestone 5 gave it an orchestrator to
delegate to; Milestone 6 gives it an agent that can actually act.

## Keeping this file current

This file is the one that goes stale fastest, and a stale architecture diagram is
worse than none — it is a confident lie. When a milestone changes what is deployed:

1. Update the **runtime** diagram (§1) — resources that actually exist.
2. Update the **stack map** (§2) if a stack or pipeline is added.
3. Add a node to **what each milestone added** (§4).
4. Move a row out of **What is not built** (§5) when it becomes true.
5. Update the header: *Last updated*, *Deployed*, *Not deployed*.

Leave the per-milestone diagram files alone. They are snapshots of a decision at a
point in time, and rewriting them to match the present would destroy the only record
of why the decision was made.
