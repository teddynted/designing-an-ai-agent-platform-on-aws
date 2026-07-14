# Bedrock Diagrams — Milestone 8

> **Milestone 8 — Amazon Bedrock Integration.**
> These diagrams describe [`internal/bedrock`](../../internal/bedrock) (the second
> provider) and [`internal/providers`](../../internal/providers) (the factory that
> chooses one). They accompany the blog post,
> [Adding Amazon Bedrock to an AI Agent Platform](../blog/adding-amazon-bedrock-to-an-ai-agent-platform.md),
> and the reference, [INFERENCE.md](../../INFERENCE.md).
>
> **Bedrock is not deployed here** — it is an API, and there is nothing to deploy. What
> this repository owns is the provider that calls it, the IAM policy that permits it, and
> the error vocabulary that explains it when it says no.
>
> The interface these diagrams sit behind is [Milestone 7's](ollama-diagrams.md), and it
> did not change.

## Contents

- [1. Switching providers by configuration](#1-switching-providers-by-configuration)
- [2. What the second provider changed](#2-what-the-second-provider-changed)
- [3. Authentication: there is no credential](#3-authentication-there-is-no-credential)
- [4. The two permissions Bedrock needs](#4-the-two-permissions-bedrock-needs)
- [5. Throttling is not an outage](#5-throttling-is-not-an-outage)
- [6. One request, end to end](#6-one-request-end-to-end)
- [7. Where the router will go](#7-where-the-router-will-go)

## 1. Switching providers by configuration

The claim of the milestone, and the reason `internal/providers` exists as a separate
package: **one environment variable, two entirely different inference backends, and not a
single caller that knows which one it got.**

```mermaid
flowchart TB
    subgraph callers["Callers — unchanged since Milestone 7"]
        cli["cmd/llm"]
        future["a workflow step<br/><i>(M9, M10)</i>"]
    end

    svc["llm.Service<br/><b>validate · retry · log · redact</b>"]
    iface{{"llm.Provider<br/><i>the interface</i>"}}

    factory["internal/providers<br/><b>the factory</b><br/><i>reads LLM_PROVIDER</i>"]

    subgraph vendors["Vendor packages — neither knows the other exists"]
        ollama["internal/ollama<br/>Local: true<br/>cost: 0"]
        bedrock["internal/bedrock<br/><b>Local: false</b><br/><b>cost: real</b>"]
    end

    ollamahost[("Ollama on EC2<br/><i>ollama-on-aws</i>")]
    aws[("Amazon Bedrock<br/><i>AWS-managed</i>")]

    cli --> svc
    future -.-> svc
    svc --> iface
    factory -- "builds one" --> iface
    iface -.-> ollama
    iface -.-> bedrock
    ollama -- "HTTP + NDJSON" --> ollamahost
    bedrock -- "Converse<br/>SigV4" --> aws

    classDef seam fill:#fff4e6,stroke:#f59f00,stroke-width:2px
    classDef vendor fill:#e7f5ff,stroke:#1c7ed6
    classDef ext fill:#f1f3f5,stroke:#868e96
    class iface,factory seam
    class ollama,bedrock vendor
    class ollamahost,aws ext
```

`internal/llm` does not import either vendor. `internal/ollama` and `internal/bedrock`
do not import each other. **`internal/providers` is the only package permitted to import
both**, and it is a leaf that nothing else depends on — which is what makes the list of
providers a thing you change in one place.

That is not a convention. `internal/architecture_test.go` walks the import graph with
`go/build` and **fails the build** if any of it stops being true.

## 2. What the second provider changed

The interface held. The **vocabulary** did not — and it could not have, because Milestone
7 designed it against a provider that has no authentication, no quotas and no
entitlements.

```mermaid
flowchart LR
    subgraph m7["Milestone 7 — designed against Ollama alone"]
        direction TB
        u1["ErrUnavailable"]
        t1["ErrTimeout"]
        s1["ErrStalled"]
        b1["ErrStreamBroken"]
        n1["ErrModelNotFound"]
        c1["ErrContextExceeded"]
        e1["ErrEmptyCompletion"]
        i1["ErrInvalidResponse"]
    end

    subgraph m8["Milestone 8 — what a HOSTED provider needs"]
        direction TB
        au["<b>ErrUnauthorized</b><br/><i>Ollama has no auth</i>"]
        ad["<b>ErrModelAccessDenied</b><br/><i>Ollama has no entitlements</i>"]
        th["<b>ErrThrottled</b><br/><i>Ollama has no quotas</i>"]
    end

    m7 == "held, unchanged" ==> iface["llm.Provider<br/><i>not one line changed</i>"]
    m8 == "<b>added</b>" ==> iface

    classDef new fill:#fff4e6,stroke:#f59f00,stroke-width:2px
    classDef old fill:#f1f3f5,stroke:#868e96
    class au,ad,th new
    class u1,t1,s1,b1,n1,c1,e1,i1 old
```

**You cannot design an abstraction from a sample of one.** None of the three additions is
a Bedrock quirk — *every* hosted provider can reject your credentials, refuse you a model,
and throttle you. M7's interface was right; M7's vocabulary was a description of Ollama
wearing an interface's clothes, and the second implementation is the first honest test of
the first one.

Note equally what was **not** added: no `ErrRegionUnsupported`, no
`ErrInferenceProfileRequired`. Those are real Bedrock failures, and they map onto the
existing nouns with a message that names the AWS-specific fix. The vocabulary grew by
what is true of *hosted providers*, not by what is true of *Bedrock*.

## 3. Authentication: there is no credential

The best thing about integrating an AWS service rather than a SaaS one. There is no API
key in the config, in CloudFormation, in Secrets Manager, or in the environment — because
there is no API key.

```mermaid
sequenceDiagram
    autonumber
    participant P as internal/bedrock
    participant SDK as AWS SDK<br/>credential chain
    participant IMDS as EC2 IMDS<br/><i>(on the instance)</i>
    participant STS as AWS STS
    participant BR as Bedrock Runtime

    P->>SDK: Converse(...)
    Note over SDK: no static key is configured,<br/>so the chain walks on
    SDK->>IMDS: who am I?
    IMDS-->>SDK: the instance role
    SDK->>STS: assume it
    STS-->>SDK: <b>temporary</b> credentials<br/><i>rotated by AWS, expire on their own</i>
    SDK->>SDK: sign the request (SigV4)
    SDK->>BR: POST /model/{id}/converse<br/>Authorization: AWS4-HMAC-SHA256…
    BR-->>P: tokens

    Note over P,BR: A credential that does not exist cannot be<br/>leaked, committed, or rotated late.
```

Locally, `aws sso login` produces the same kind of temporary credential and the code path
is **identical**. There is no development mode that authenticates differently — because a
development mode that authenticates differently is a production incident waiting for its
moment.

## 4. The two permissions Bedrock needs

The most common Bedrock failure has nothing to do with your code. There are **two gates**,
they are configured in completely different places, and they **throw the same exception**.

```mermaid
flowchart TB
    req["bedrock:InvokeModel"]

    gate1{"<b>Gate 1 — IAM</b><br/>may this ROLE invoke<br/>this model ARN?"}
    gate2{"<b>Gate 2 — Model access</b><br/>may this ACCOUNT<br/>use this model at all?"}

    ok["✅ tokens"]
    denied["❌ <b>AccessDeniedException</b><br/><i>the same error from both gates</i>"]

    req --> gate1
    gate1 -- "no<br/><i>fix: the IAM policy</i>" --> denied
    gate1 -- yes --> gate2
    gate2 -- "no<br/><i>fix: Bedrock console →<br/>Model access → request it</i>" --> denied
    gate2 -- yes --> ok

    denied --> msg["<b>ErrModelAccessDenied</b><br/>the platform's message names<br/><b>both</b> causes, because AWS<br/>will not tell you which"]

    classDef gate fill:#fff4e6,stroke:#f59f00,stroke-width:2px
    classDef bad fill:#ffe3e3,stroke:#e03131
    classDef good fill:#d3f9d8,stroke:#2f9e44
    class gate1,gate2 gate
    class denied bad
    class ok good
```

Which is why the platform's error says both things at once, rather than "access denied"
and leaving you to re-read an IAM policy that was correct all along.

The IAM policy itself is **empty by default** — `BedrockModelArns` grants nothing until
you name something:

```mermaid
flowchart LR
    role["InstanceRole"]

    subgraph policy["BedrockInvokePolicy — opt-in, model-scoped"]
        invoke["<b>InvokeNamedModels</b><br/>bedrock:InvokeModel<br/>bedrock:InvokeModelWithResponseStream"]
        list["<b>ListModels</b><br/>bedrock:ListFoundationModels<br/><i>read-only, on *</i>"]
    end

    m1["arn:…:foundation-model/<br/>claude-3-5-haiku"]
    m2["arn:…:inference-profile/<br/>us.claude-sonnet-4"]
    region{{"Condition:<br/>aws:RequestedRegion<br/>∈ BedrockRegions"}}

    star["❌ <b>Resource: *</b><br/><i>a licence to invoke the most<br/>expensive model in the catalogue</i>"]

    role --> policy
    invoke --> region
    region --> m1
    region --> m2
    invoke -.->|"never"| star

    classDef bad fill:#ffe3e3,stroke:#e03131,stroke-dasharray: 4 4
    classDef cond fill:#fff4e6,stroke:#f59f00
    class star bad
    class region cond
```

Bedrock is an API that turns permission into money. A wildcard here is the one AWS
wildcard that **bills you** for being wrong, and an instance permitted to call any model
is an instance whose compromise is measured in dollars per minute.

*(An inference profile is a **different resource** from the model it fronts. Newer models
are on-demand only through a `us.`-prefixed profile, and invoking one needs **both** ARNs
— grant only the model and the call fails with a validation error that never mentions
permissions.)*

## 5. Throttling is not an outage

The defining failure of a hosted provider, and the reason it earns its own error kind
instead of being folded into "unavailable".

```mermaid
flowchart TB
    req["Converse"]
    resp{"the response"}

    thr["<b>ThrottlingException</b><br/>ServiceQuotaExceededException"]
    down["connection refused,<br/>5xx, timeout"]

    k1["errorKind: <b>throttled</b><br/><i>“the provider is fine,<br/>and you are over your quota”</i>"]
    k2["errorKind: <b>unavailable</b><br/><i>“Bedrock is actually down”</i>"]

    retry["retry: backoff + <b>full jitter</b><br/>3 attempts<br/><i>safe — inference has no side effects</i>"]

    alarm["🔔 <b>“Bedrock is down”</b> alarm"]
    quota["📈 a graph of <b>demand</b><br/><i>ask for a quota increase,<br/>or route cheap work to Ollama</i>"]

    req --> resp
    resp --> thr --> k1 --> retry
    resp --> down --> k2 --> retry
    k2 --> alarm
    k1 --> quota
    k1 -. "<b>must not</b> page you<br/>for being busy" .-x alarm

    classDef thr fill:#fff4e6,stroke:#f59f00,stroke-width:2px
    classDef bad fill:#ffe3e3,stroke:#e03131
    class thr,k1 thr
    class down,k2,alarm bad
```

If throttling were reported as `ErrUnavailable`, a "Bedrock is down" alarm would fire
every time the platform got **busy** — which is precisely when you least want to be woken
up to look at a service that is working perfectly.

**And the AWS SDK's own retries are switched off** (`WithRetryMaxAttempts(1)`). The SDK
retries throttling by default; so does this integration. Three attempts of three is nine
calls, an `attempts: 3` log line that is a lie, and a duration containing backoff nobody
accounted for. **Two retry layers do not add, they multiply — and they hide each other.**

## 6. One request, end to end

```mermaid
sequenceDiagram
    autonumber
    participant C as caller
    participant S as llm.Service
    participant B as internal/bedrock
    participant BR as Bedrock<br/>(Converse)

    C->>S: Stream(Request{System, Prompt, Purpose, CorrelationID})

    rect rgb(255, 244, 230)
        Note over S: refuse before spending
        S->>S: estimate tokens (pessimistic: 3 chars/token)
        S-->>C: ✋ ErrContextExceeded — the model would silently<br/>truncate, and bill you in full for half a question
    end

    S->>S: log "inference requested"<br/><b>prompt hash + size, never the prompt</b>

    loop up to 3 attempts (backoff + full jitter)
        S->>B: Stream(...)
        B->>BR: ConverseStream — system in its OWN field
        BR-->>B: contentBlockDelta …
        B->>C: chunk (a token)
        Note over B: idle timer RESET on every event —<br/>a slow model is healthy, a silent one is ErrStalled
        BR-->>B: metadata (usage, stopReason)
    end

    B-->>S: Response{content, usage}
    S->>S: log "inference completed"<br/>tokensPerSecond · <b>estimatedCostUsd</b> · finishReason
    S-->>C: Response

    Note over S,BR: Once a token has escaped to the caller, a failure is<br/><b>ErrStreamBroken</b> — terminal. Retrying would hand them<br/>a second beginning glued onto the first.
```

## 7. Where the router will go

Milestone 8 did not build a router. It built **the thing a router needs**: two providers
that answer `Capabilities()` differently, behind one interface.

```mermaid
flowchart TB
    req["a request<br/><i>purpose: diff-summary</i>"]

    router["<b>llm.Router (M10)</b><br/><i>implements llm.Provider itself,<br/>and sits exactly where a single<br/>provider sits today</i>"]

    subgraph facts["what it routes ON — Capabilities, and nothing else"]
        direction LR
        local["<b>Local</b><br/>does the prompt leave?"]
        cost["<b>CostPer1M…</b><br/>what will this cost?"]
        ctx["<b>MaxContextTokens</b><br/>will it even fit?"]
    end

    o["<b>Ollama</b><br/>Local: true · cost: 0<br/><i>idle cost: the whole instance</i>"]
    b["<b>Bedrock</b><br/>Local: false · cost: real<br/><i>idle cost: zero</i>"]
    cl["<b>Claude</b> (M9)"]

    req --> router
    router -.reads.-> facts
    router -->|"a summary,<br/>a private repo"| o
    router -->|"reasoning,<br/>or the GPU was<br/>interrupted (M3)"| b
    router -.-> cl

    classDef fut fill:#f8f9fa,stroke:#adb5bd,stroke-dasharray: 5 5
    classDef seam fill:#fff4e6,stroke:#f59f00,stroke-width:2px
    class cl,router fut
    class local,cost,ctx seam
```

Both fields are true today because M8 forced them to be: `Local` was a lonely `true`
until a provider existed for which it was `false`, and cost was `0` until a provider
existed that charges. **A router built on a `Capabilities` that only one provider had ever
filled in would be routing on fiction.**

The fallback case is the one M3 built for: the Spot GPU vanishes with two minutes'
notice, the local model goes with it, and the platform keeps answering — more expensively,
and without anybody being paged.
