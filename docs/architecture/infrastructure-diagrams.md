# Infrastructure Diagrams — Milestone 2

> **Milestone 2 — CloudFormation Infrastructure.**
> These diagrams describe infrastructure defined in
> [`infra/cloudformation`](../../infra/cloudformation) and validated with
> `cfn-lint`. It has **not been deployed** to a live account as part of this
> milestone. They accompany the blog post,
> [Provisioning an AI Agent Platform with CloudFormation](../blog/provisioning-an-ai-agent-platform-with-cloudformation.md).

Four diagrams: two AWS-style service views (hand-authored SVG, in the same
version-controlled approach as [Milestone 1](aws-architecture.svg)), and two
Mermaid flow views. All share one vocabulary and one colour key
(compute = orange, integration = pink, management = rose, storage = green,
networking = purple, external = grey).

## 1. Infrastructure Overview

Every resource this milestone provisions, and how it fits inside the AWS Cloud →
Region → VPC → subnet nesting. The EC2 Spot instance and its encrypted, disposable
root volume sit inside a default-deny security group; the regional services
(EventBridge, Lambda, CloudWatch, S3) sit outside the VPC.

![Infrastructure overview: an internet gateway fronts a VPC public subnet containing a default-deny security group, inside which an EC2 Spot instance has an attached encrypted root EBS volume; the instance writes artifacts to S3 and logs to CloudWatch, and an EventBridge bus invokes a placeholder Lambda that also logs to CloudWatch.](infrastructure-overview.svg)

## 2. Network Topology

The addressing, routing, and security boundary in detail: the VPC CIDR, the
public subnet CIDR, the public route table (with its default route to the
internet gateway), and the security group — no inbound, all outbound, managed
through SSM.

![Network topology: a 10.20.0.0/16 VPC with a 10.20.1.0/24 public subnet in one availability zone; a public route table sends 0.0.0.0/0 to the internet gateway and keeps 10.20.0.0/16 local; the instance security group has no inbound rules and allows all outbound, with management via SSM Session Manager and IMDSv2 required.](network-topology.svg)

## 3. CloudFormation Resource Relationships

The six stacks, the order CloudFormation provisions them in, and the cross-stack
imports that link them. Networking and IAM come first because compute depends on
both; storage and observability are independent leaves.

```mermaid
flowchart TB
    cfn["CloudFormation"]

    subgraph net["01 · Networking"]
        v["VPC · subnet · IGW<br/>route table · security group"]
    end
    subgraph iam["02 · IAM"]
        r["EC2 role + instance profile<br/>Lambda role"]
    end
    subgraph comp["03 · Compute"]
        c["Launch template<br/>EC2 Spot + encrypted root EBS"]
    end
    subgraph sto["04 · Storage"]
        s["S3 artifact bucket"]
    end
    subgraph evt["05 · Events"]
        e["EventBridge bus + rule<br/>placeholder Lambda"]
    end
    subgraph obs["06 · Observability"]
        o["CloudWatch log groups"]
    end

    cfn --> net
    cfn --> iam
    cfn --> comp
    cfn --> sto
    cfn --> evt
    cfn --> obs

    comp -.->|imports subnet, SG,<br/>instance profile| net
    comp -.->|imports| iam
    evt -.->|imports Lambda role| iam
```

**Legend.** Solid arrows: CloudFormation provisions the stack. Dotted arrows:
one stack imports an exported value from another, which also fixes the deploy
order. Storage and observability import nothing.

## 4. Infrastructure Deployment Flow

What happens when an operator runs the deploy. Each stack is applied in
dependency order; the run ends with a foundation that later milestones extend.

```mermaid
flowchart TB
    dev(["Developer"]) --> deploy["aws cloudformation deploy<br/>(make deploy)"]
    deploy --> n["Provision networking · 01"]
    n --> i["Provision IAM · 02"]
    i --> c["Provision compute · 03"]
    c --> s["Provision storage · 04"]
    s --> e["Provision events · 05"]
    e --> o["Provision monitoring · 06"]
    o --> ready(["Infrastructure ready"])
```

**Legend.** A single linear path: networking → IAM → compute → storage → events
→ monitoring. The order matters only where a stack imports another's exports
(compute and events); the rest could deploy in any order, but a fixed sequence
keeps the process predictable and re-runnable.

## Consistency note

These diagrams, the [templates](../../infra/cloudformation), and the
[blog post](../blog/provisioning-an-ai-agent-platform-with-cloudformation.md)
use the same stack numbering (01–06) and the same resource names. The Milestone 1
[platform diagrams](diagrams.md) show where this foundation sits in the larger
three-plane architecture.
