# Workflow Orchestration Diagrams — Milestone 5

> **Milestone 5 — Self-hosted n8n Integration.**
> These diagrams describe the integration in
> [`internal/workflow`](../../internal/workflow) and
> [`internal/n8n`](../../internal/n8n). They accompany the blog post,
> [Using n8n as the Workflow Engine for AI Automation](../blog/using-n8n-as-the-workflow-engine-for-ai-automation.md),
> and the integration reference, [WORKFLOWS.md](../../WORKFLOWS.md).
>
> **n8n itself is not deployed here.** Its infrastructure lives in the
> `self-hosted-n8n-on-aws` repository. These diagrams stop at the boundary, and
> the boundary is the point.

> **This is a snapshot of Milestone 5.** It is kept as it was written — the record of a
> decision at a point in time. For what is deployed **today**, see
> **[The Platform As Built](current-architecture.md)**, the living diagram.

Five diagrams, sharing the colour key of the earlier sets (compute = orange,
storage = green, external = grey, failure = red).

## 1. The request flow

The chain the brief asks for, with the two things that make it more than a
function call: the **interface** in the middle, and the **boundary** at the end.

```mermaid
flowchart TB
    gh(["GitHub webhook"]) --> app["Platform<br/>webhook handler"]
    app --> svc["workflow.Service"]

    subgraph service["What the Service does for EVERY engine"]
        direction TB
        v["validate — a request that cannot<br/>succeed never leaves the process"]
        c["correlate — derive a stable ID<br/>from the event"]
        t["time + log — structured, always"]
        v --> c --> t
    end

    svc --- service
    svc --> eng{{"workflow.Engine<br/>INTERFACE"}}

    eng --> n8nc["n8n.Client"]
    subgraph client["What the engine does"]
        direction TB
        a["authenticate"]
        i["idempotency key"]
        s["sanitise the payload"]
        r["retry what is worth retrying"]
        a --> i --> s --> r
    end
    n8nc --- client

    n8nc -->|"HTTPS"| boundary

    subgraph boundary["self-hosted-n8n-on-aws — ANOTHER REPOSITORY"]
        inst["n8n instance"]
        exec["Workflow execution"]
        inst --> exec
    end

    t -.-> cw["CloudWatch Logs"]

    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef store fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    classDef ext fill:#E8E8E8,stroke:#666,color:#232F3E
    class app,svc,eng,n8nc,v,c,t,a,i,s,r aws
    class cw store
    class gh,inst,exec ext
```

The division is deliberate. **The Service does what must be identical for every
engine** — if each engine logged in its own shape, no dashboard could span them,
and if each invented its own correlation, a GitHub delivery could not be followed
across the platform. **The engine does what is specific and dirty** — speak HTTP,
survive a flaky network — and is free to be replaced without taking the
observability with it.

## 2. The seam

Why there is an interface at all, rather than an HTTP call from the handler.

```mermaid
flowchart LR
    subgraph without["Without the seam"]
        h1["webhook handler"] -->|"n8n URL,<br/>n8n auth,<br/>n8n retries,<br/>n8n response shape"| x1["n8n"]
        h2["scheduler"] -->|"…the same, again"| x1
        h3["agent"] -->|"…and again"| x1
    end

    subgraph with["With the seam"]
        y1["webhook handler"] --> s["workflow.Service"]
        y2["scheduler"] --> s
        y3["agent"] --> s
        s --> e{{"Engine"}}
        e --> n["n8n"]
        e -.-> f["Step Functions?<br/>Temporal?<br/>a queue?"]
    end

    classDef bad fill:#D13212,stroke:#7D1D0C,color:#FFFFFF
    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef future fill:#FFF,stroke:#999,stroke-dasharray: 5 5,color:#666
    class h1,h2,h3 bad
    class y1,y2,y3,s,e,n aws
    class f future
```

On the left, replacing n8n — or running two engines during a migration — means
touching every caller, and every caller has its own subtly different retry policy.
On the right it is one implementation of one interface.

The test that the seam is real: **`internal/workflow` does not import
`internal/n8n`.** If that dependency ever points the other way, the abstraction is
a decoration.

## 3. Why a retry is not free

The hard problem of this milestone, and the reason for the idempotency key.

```mermaid
sequenceDiagram
    autonumber
    participant P as Platform
    participant N as n8n
    participant G as GitHub

    P->>N: POST /webhook/blog<br/>X-Idempotency-Key: blog:delivery-123
    N->>N: workflow starts…
    Note over N: it opens a pull request

    N--xP: the response is lost<br/>(timeout, LB reset, deploy)

    Note over P: A timeout says NO ANSWER ARRIVED.<br/>It says NOTHING about whether<br/>the request did.

    P->>N: retry — SAME key: blog:delivery-123

    alt the workflow checks the key ✅
        N->>N: seen it → return the first result
        N-->>P: accepted
        Note over G: one pull request
    else the workflow ignores the key ❌
        N->>N: run it all again
        N->>G: open a SECOND pull request
        Note over G: two pull requests,<br/>and a confused human
    end
```

The key is **derived from the event ID**, never generated — so the same GitHub
delivery, retried by us or replayed by an operator next week, produces the same
key. A random key here would defeat the entire purpose.

**The transport is at-least-once. Only the workflow can make the execution
effectively-once**, and only if it actually checks. This repository cannot enforce
that, which is exactly why it is written down in bold in
[WORKFLOWS.md](../../WORKFLOWS.md#the-one-hard-problem-a-retry-is-not-free).

## 4. What is retried, and what is not

Retrying the wrong thing is worse than not retrying at all.

```mermaid
flowchart TB
    resp(["n8n answered (or did not)"]) --> k{"What kind of failure?"}

    k -->|"connection refused<br/>DNS · TLS reset"| un["ErrUnavailable"]
    k -->|"no answer in time"| to["ErrTimeout"]
    k -->|"429 · 5xx"| un
    k -->|"401 · 403"| au["ErrUnauthorized"]
    k -->|"404"| uw["ErrUnknownWorkflow"]
    k -->|"400"| ir["ErrInvalidRequest"]
    k -->|"200 + HTML<br/>200 + broken JSON"| iv["ErrInvalidResponse"]
    k -->|"200 + {status:error}"| wf["ErrWorkflowFailed"]

    un --> retry["RETRY<br/>exponential + full jitter<br/>honour Retry-After"]:::good
    to --> retry

    au --> stop["DO NOT RETRY"]:::bad
    uw --> stop
    ir --> stop
    iv --> stop
    wf --> stop2["DO NOT RETRY —<br/>it RAN. Retrying runs it AGAIN."]:::bad

    classDef good fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    classDef bad fill:#D13212,stroke:#7D1D0C,color:#FFFFFF
```

The two that catch people:

- **A `200` is not a success.** n8n answers `200` and puts the error *in the body*
  when a workflow throws and a "Respond to Webhook" node catches it. Trusting the
  status code is how a platform cheerfully reports that it triggered workflows into
  a void.
- **`ErrWorkflowFailed` is not retried.** The workflow *ran*. Retrying runs it
  again — and re-running a workflow with side effects is a decision for a human,
  not for an HTTP client.

## 5. Where the secrets are

The GitHub payload is the one thing in this flow the platform did not author.

```mermaid
flowchart LR
    gh["GitHub payload<br/>installation.access_token: ghs_LIVE…"] --> san

    subgraph san["Sanitiser (n8n.Client)"]
        cap["size cap"] --> walk["walk the JSON at any depth"]
        walk --> red["credential-shaped keys →<br/>[REDACTED BY PLATFORM]"]
    end

    san --> wire["what goes on the wire<br/>installation.access_token: [REDACTED]<br/>installation.id: 42 ✓<br/>repository.full_name ✓"]
    wire --> hist["n8n execution history<br/>(a database · backed up ·<br/>readable in the UI)"]

    tok["N8N_TOKEN"] -->|"header only"| wire
    tok -.->|"NEVER"| logs["logs · errors"]:::bad

    classDef bad fill:#D13212,stroke:#7D1D0C,color:#FFFFFF
    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef store fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    class cap,walk,red,wire aws
    class hist store
```

"We are only passing it on" is exactly how secrets travel. A forwarded payload
lands in **n8n's execution history** — a database, which gets backed up, and which
anyone with UI access can read. The platform gains nothing by forwarding a
credential, and it is the platform's job not to widen the blast radius of someone
else's mistake.

The structure survives the redaction. A sanitiser that guts the payload is one
nobody keeps enabled.
