# Observability Diagrams — Milestone 13

> **Milestone 13 — Monitoring & Observability.**
> These diagrams describe [`internal/observability`](../../internal/observability)
> (the logging, metrics and health library) and
> [`infra/cloudformation/10-monitoring.yaml`](../../infra/cloudformation/10-monitoring.yaml)
> (the dashboards, alarms and metric filters). They accompany the blog post,
> [Monitoring an AI Agent Platform with CloudWatch](../blog/monitoring-an-ai-agent-platform-with-cloudwatch.md),
> and the reference, [OBSERVABILITY.md](../../OBSERVABILITY.md).
>
> **One line, two products.** The platform's structured log line is *also* — when it
> carries an EMF envelope — a metric. Nothing is emitted twice; CloudWatch reads the
> same bytes as a log (searchable) and as a metric (graphable, alarmable).

## Contents

- [1. Monitoring architecture](#1-monitoring-architecture)
- [2. Log flow](#2-log-flow)
- [3. Metrics flow](#3-metrics-flow)
- [4. The three signals of a single line](#4-the-three-signals-of-a-single-line)
- [5. Health: liveness vs readiness](#5-health-liveness-vs-readiness)
- [6. Alarm lifecycle](#6-alarm-lifecycle)
- [7. Where tracing reaches, and where it stops](#7-where-tracing-reaches-and-where-it-stops)

## 1. Monitoring architecture

CloudWatch collects logs and metrics from every component. The sources differ; the
destination is one place an operator looks.

```mermaid
flowchart TB
    subgraph sources["Sources"]
        lambda["AWS Lambda<br/>webhook · spot · scheduler"]
        ec2["EC2 instance<br/>+ CloudWatch agent"]
        oc["OpenClaw<br/>(agent plane)"]
        n8n["n8n<br/>(workflow plane)"]
        app["Platform services<br/>internal/observability"]
    end

    subgraph cw["Amazon CloudWatch"]
        logs["CloudWatch Logs<br/>/aiap/env/*  ·  /aws/lambda/*"]
        metrics["CloudWatch Metrics<br/>AWS/Lambda · AWS/EC2 · CWAgent<br/>aiap/app · aiap/env/logs"]
        filters["Metric filters"]
        dash["Dashboards<br/>infrastructure · ai-platform · application"]
        alarms["Alarms"]
        xray["X-Ray traces"]
    end

    sns["SNS topic"]
    ops(["Operators"])

    lambda -->|"logs + AWS/Lambda metrics"| logs
    lambda -->|"active tracing"| xray
    ec2 -->|"agent: logs + mem/disk"| logs
    ec2 -->|"AWS/EC2 + CWAgent"| metrics
    oc --> logs
    n8n --> logs
    app -->|"structured logs + EMF"| logs

    logs --> filters --> metrics
    logs -->|"EMF extraction"| metrics
    metrics --> dash
    metrics --> alarms
    logs --> dash
    alarms --> sns --> ops
    dash --> ops
    xray --> ops

    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef store fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    class lambda,ec2,app aws
    class logs,metrics,dash store
```

## 2. Log flow

Application → CloudWatch Logs → Dashboards → Alarms → Operators. The metric filter
is the seam that lets a *log* raise an *alarm*.

```mermaid
flowchart TB
    app["Application<br/>structured JSON line"]
    logs["CloudWatch Logs<br/>(centralised, retained)"]
    filters["Metric filters<br/>level=ERROR, msg=workflow failed, ..."]
    metrics["Log-derived metrics<br/>aiap/env/logs"]
    dash["Dashboards"]
    alarms["Alarms"]
    ops(["Operators"])

    app --> logs
    logs --> filters
    filters --> metrics
    metrics --> dash
    metrics --> alarms
    logs -->|"Logs Insights (ad-hoc query)"| ops
    dash --> ops
    alarms --> ops

    classDef store fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    class logs,metrics,dash store
```

## 3. Metrics flow

Infrastructure → CloudWatch Metrics → Dashboards → CloudWatch Alarms. Host and
Lambda metrics arrive as metrics already; nothing has to parse a log to graph them.

```mermaid
flowchart TB
    infra["Infrastructure<br/>EC2 · Lambda · CloudWatch agent"]
    metrics["CloudWatch Metrics<br/>AWS/EC2 · AWS/Lambda · CWAgent"]
    emf["Application (EMF)<br/>aiap/app"]
    dash["Dashboards"]
    alarms["CloudWatch Alarms"]
    sns["SNS → operators"]

    infra --> metrics
    emf --> metrics
    metrics --> dash
    metrics --> alarms
    alarms --> sns

    classDef store fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    class metrics,dash store
```

## 4. The three signals of a single line

The platform emits one structured line. Depending on its shape, CloudWatch derives
up to three things from it — with no duplicate emission and, for EMF, no extra IAM.

```mermaid
flowchart LR
    line["one structured log line<br/>{time, level, service, correlationId, ...}"]

    line -->|"always"| search["a searchable LOG<br/>(Logs Insights)"]
    line -->|"if it matches a filter"| filtered["a METRIC<br/>(metric filter → aiap/env/logs)"]
    line -->|"if it carries an _aws envelope"| emf["a METRIC<br/>(EMF → aiap/app)"]

    classDef ok fill:#0b6,stroke:#083,color:#fff;
    class line ok;
```

## 5. Health: liveness vs readiness

The same dependency check means different things depending on which probe it is
registered under — and getting the two backwards is a classic outage amplifier.

```mermaid
flowchart TB
    subgraph live["/healthz — liveness"]
        lq["Is THIS process healthy?"]
        lr["fail ⇒ RESTART me"]
        lq --> lr
    end

    subgraph ready["/readyz — readiness"]
        rq["Can I do useful work?<br/>(is n8n / OpenClaw reachable?)"]
        rr["fail ⇒ stop routing to me<br/>(503, try elsewhere)"]
        rq --> rr
    end

    warn["A dependency check in liveness<br/>turns an outage into a crash loop:<br/>restarting does not fix n8n."]:::bad
    ready -.->|"put dependency checks HERE"| rq
    live -.->|"NOT here"| warn

    classDef bad fill:#D13212,stroke:#7D1D0C,color:#FFFFFF
```

## 6. Alarm lifecycle

An alarm is a small state machine. The platform sends both the ALARM and the OK
transition to SNS, because "it recovered" is as operationally useful as "it broke".

```mermaid
stateDiagram-v2
    [*] --> InsufficientData
    InsufficientData --> OK: data arrives, within threshold
    InsufficientData --> Alarm: data arrives, breaching
    OK --> Alarm: threshold breached for N periods
    Alarm --> OK: back within threshold
    OK --> InsufficientData: metric stops reporting
    Alarm --> InsufficientData: metric stops reporting

    note right of Alarm
        Notifies SNS → operators.
        A per-instance alarm goes to
        InsufficientData when a Spot
        instance is replaced — the
        ASG milestone fixes that.
    end note
```

## 7. Where tracing reaches, and where it stops

X-Ray traces the platform's own Lambda. It cannot trace into services this
repository does not deploy, and it does not survive the intentional EventBridge
decoupling. The correlation ID carries the story where the trace cannot.

```mermaid
flowchart LR
    gh["GitHub"] -->|"trace starts"| lambda["webhook Lambda<br/>(active tracing)"]
    lambda -->|"segment"| xray["X-Ray"]
    lambda -->|"PutEvents"| bus["EventBridge"]

    bus -. "trace does NOT cross<br/>(different transaction)" .-> n8n["n8n"]
    n8n -. "own deployment<br/>(not instrumented here)" .-> oc["OpenClaw"]

    lambda -->|"correlationId travels in the payload"| bus
    bus -->|"correlationId"| n8n
    n8n -->|"correlationId"| oc

    classDef edge fill:#0b6,stroke:#083,color:#fff;
    classDef ext fill:#E8E8E8,stroke:#666,color:#232F3E;
    class lambda edge;
    class n8n,oc ext;
```
