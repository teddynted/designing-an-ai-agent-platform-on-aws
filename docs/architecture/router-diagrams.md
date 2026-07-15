# Router Diagrams — Milestone 10

> **Milestone 10 — Hybrid AI Routing.**
> These diagrams describe [`internal/router`](../../internal/router) (the routing layer)
> and the additions to [`internal/providers`](../../internal/providers) (the factory that
> now builds it). They accompany the blog post,
> [Building Hybrid AI Workflows with Ollama and Amazon Bedrock](../blog/building-hybrid-ai-workflows-with-ollama-and-amazon-bedrock.md),
> and the reference, [ROUTING.md](../../ROUTING.md).
>
> **No model is deployed here.** Ollama's GPU belongs to `ollama-on-aws`; Bedrock is AWS's
> to run. This repository owns the layer that **chooses between them** — and, crucially,
> that layer never learns which providers exist.
>
> The interface these diagrams sit behind is [Milestone 7's](ollama-diagrams.md). It did
> not change. The router **is** an `llm.Provider`.

## Contents

- [1. The router is a provider](#1-the-router-is-a-provider)
- [2. High-level architecture](#2-high-level-architecture)
- [3. The request lifecycle](#3-the-request-lifecycle)
- [4. Routing sequence: constrain, select, order, execute](#4-routing-sequence-constrain-select-order-execute)
- [5. Preference vs constraint](#5-preference-vs-constraint)
- [6. The fallback flow](#6-the-fallback-flow)
- [7. Health as a circuit breaker](#7-health-as-a-circuit-breaker)
- [8. The three things that must never fail over](#8-the-three-things-that-must-never-fail-over)
- [9. Capabilities: union vs guarantee](#9-capabilities-union-vs-guarantee)
- [10. Adding a provider](#10-adding-a-provider)

## 1. The router is a provider

The whole milestone in one picture: a `Router` occupies the slot a single client used to,
and it holds clients of the same kind it is. Nothing above the interface changed.

```mermaid
flowchart TB
    subgraph callers["Callers — unchanged since Milestone 7"]
        cli["cmd/llm"]
        loop["llm.Service.Converse<br/>(the tool loop)"]
        n8n["a workflow step"]
    end

    svc["llm.Service<br/><b>validate · context check · correlate · log</b>"]
    iface{{"llm.Provider<br/><i>the seam</i>"}}

    callers --> svc --> iface

    iface -.->|"LLM_PROVIDER=ollama"| ollama["ollama.Client"]
    iface -.->|"LLM_PROVIDER=bedrock"| bedrock["bedrock.Client"]
    iface ==>|"LLM_PROVIDER=router"| router["router.Router<br/><b>also an llm.Provider</b>"]

    router --> ro["ollama.Client"]
    router --> rb["bedrock.Client"]

    classDef pick fill:#0b6,stroke:#083,color:#fff;
    class router pick;
```

## 2. High-level architecture

Where the router sits in the platform, and what stays behind whose boundary. The prompt
leaving the network is the fact the whole design is organised around.

```mermaid
flowchart LR
    gh["GitHub"] --> n8n["n8n<br/><i>orchestration</i>"]
    n8n --> plat["The platform<br/>(this repo)"]

    subgraph vpc["AWS VPC — the network the prompt lives in"]
        plat --> svc["llm.Service"]
        svc --> router["router.Router"]
        router -->|"local · free · private"| ollama["Ollama on EC2 Spot GPU<br/><i>ollama-on-aws</i>"]
    end

    router ==>|"leaves the VPC · billed"| bedrock["Amazon Bedrock<br/>(Claude)"]

    note["RequireLocal requests<br/>never cross this line"] -.-> bedrock

    classDef leave fill:#c33,stroke:#900,color:#fff;
    class bedrock leave;
```

## 3. The request lifecycle

One request, from workflow to completion. OpenClaw and n8n never learn a router exists.

```mermaid
flowchart TB
    wf["Workflow (n8n)"] --> oc["the platform / OpenClaw"]
    oc --> svc["llm.Service<br/>validate · check window · correlate · log"]
    svc --> r["router.Router"]

    r --> c["CONSTRAIN<br/><i>which providers CAN serve this?</i>"]
    c --> s["SELECT<br/><i>which one SHOULD? (strategy)</i>"]
    s --> o["ORDER<br/><i>and if it fails, who is next? (health)</i>"]
    o --> e["EXECUTE<br/><i>try in order, at most once each</i>"]

    e --> prov["selected provider"]
    prov --> inf["model inference"]
    inf --> resp["structured response<br/><b>+ Provider = who answered</b>"]
    resp --> done["workflow completion"]
```

## 4. Routing sequence: constrain, select, order, execute

The interesting path — a `purpose` rule sends the request to Bedrock, Bedrock throttles,
and the router falls over to Ollama. Note the order: constraints first, always.

```mermaid
sequenceDiagram
    autonumber
    participant S as llm.Service
    participant R as router.Router
    participant H as Health
    participant B as Bedrock
    participant O as Ollama

    S->>R: Generate(purpose=release-notes)

    Note over R: CONSTRAIN — both can serve it
    Note over R: SELECT — rule: release-notes → bedrock
    R->>H: is bedrock healthy?
    H-->>R: yes
    Note over R: ORDER — chain = [bedrock, ollama]

    R->>B: Generate
    B-->>R: ErrThrottled
    R->>H: bedrock Failed()
    Note over R: provider at fault → fall over

    R->>O: Generate
    O-->>R: completion
    R->>H: ollama Succeeded()
    R-->>S: Response{Provider: "ollama", …}
    Note over S: log: servedBy=ollama, fallback=true
```

## 5. Preference vs constraint

The idea the milestone turns on. A preference bends when it cannot be honoured; a constraint
does not, and empties the candidate list instead — at which point the request is refused.

```mermaid
flowchart TB
    req["a request"] --> gate{"CONSTRAINT gate"}

    gate -->|"RequireLocal + provider is hosted"| drop["excluded — no exceptions"]
    gate -->|"needs tools + provider has none"| drop
    gate -->|"prompt &gt; window"| drop
    gate -->|"eligible"| pool["candidate pool"]

    pool --> empty{"pool empty?"}
    empty -->|"yes"| refuse["ErrNoProvider<br/><b>refuse — do NOT relax the constraint</b>"]
    empty -->|"no"| strat["strategy picks<br/><i>a preference, may bend to a capable candidate</i>"]

    classDef bad fill:#c33,stroke:#900,color:#fff;
    class refuse,drop bad;
```

## 6. The fallback flow

Direction-agnostic, and structurally loop-free: the chain is a subset of the enabled
providers, each appearing once, walked forwards.

```mermaid
flowchart LR
    start["chosen provider"] --> try1{"succeeds?"}
    try1 -->|"yes"| done["answer<br/>Succeeded()"]
    try1 -->|"provider at fault"| fo{"fallback on?<br/>not pinned?<br/>stream silent?"}
    try1 -->|"request/model at fault"| stop["return the error<br/><i>a second model repeats it</i>"]

    fo -->|"no"| stop
    fo -->|"yes"| next{"another provider<br/>in the chain?"}
    next -->|"yes"| start2["next provider"] --> try2{"succeeds?"}
    next -->|"no"| exhausted["every provider failed<br/><i>names all of them</i>"]

    try2 -->|"yes"| done
    try2 -->|"no"| exhausted

    classDef bad fill:#c33,stroke:#900,color:#fff;
    class stop,exhausted bad;
```

## 7. Health as a circuit breaker

Why fallback without memory is a platform that is down. Demoted, never removed; recovers
through one half-open probe.

```mermaid
stateDiagram-v2
    [*] --> Healthy
    Healthy --> Healthy: success (count reset)
    Healthy --> Failing: a provider-at-fault failure
    Failing --> Healthy: success
    Failing --> Demoted: failures ≥ threshold
    Demoted --> Demoted: still inside cooldown\n(moved to BACK of chain, never removed)
    Demoted --> HalfOpen: cooldown expired
    HalfOpen --> Healthy: probe request succeeds
    HalfOpen --> Demoted: probe request fails

    note right of Demoted
        A demoted provider is still in
        the chain. "Remove on failure"
        can take the whole platform down
        from a DNS blip.
    end note
```

## 8. The three things that must never fail over

The router's real danger is a retry where retrying is unsafe. All three are refused
structurally, not by trusting a provider's error string.

```mermaid
flowchart TB
    subgraph a["1 · a stream that has spoken"]
        a1["token reached the caller"] --> a2["count &gt; 0 → do NOT fail over<br/><i>whatever the error</i>"]
        a2 --> a3["a 2nd provider = a 2nd beginning"]
    end
    subgraph b["2 · a tool has run"]
        b1["a Write tool changed the world"] --> b2["ErrEffectsCommitted → terminal"]
        b2 --> b3["fail over = run the workflow twice"]
    end
    subgraph c["3 · a conversation in progress"]
        c1["history has an assistant turn<br/>with tool calls / reasoning"] --> c2["PIN to the provider that started it"]
        c2 --> c3["signed reasoning + tool IDs<br/>cannot migrate to another model"]
    end

    classDef bad fill:#c33,stroke:#900,color:#fff;
    class a3,b3,c3 bad;
```

## 9. Capabilities: union vs guarantee

Combining several providers into one `Capabilities` is not a merge. Get the direction wrong
on `Local` and it is a security bug.

```mermaid
flowchart LR
    subgraph fleet["a fleet: Ollama + Bedrock"]
        o["Ollama<br/>Local ✓ · Tools ✗ · 8k"]
        b["Bedrock<br/>Local ✗ · Tools ✓ · 200k"]
    end

    fleet --> caps["router.Capabilities()"]

    caps --> u["<b>UNION</b> (a capability)<br/>Tools ✓ · Reasoning ✓ · window 200k<br/><i>any provider can → the router can</i>"]
    caps --> i["<b>INTERSECTION</b> (a guarantee)<br/>Local ✗<br/><i>every provider must → or it is a lie</i>"]
    caps --> w["<b>WORST</b> (a budget)<br/>cost = the priciest in the fleet<br/><i>pessimism is the safe direction</i>"]

    classDef guar fill:#c33,stroke:#900,color:#fff;
    class i guar;
```

## 10. Adding a provider

The claim the milestone is really making. The router is not in the list of files you touch.

```mermaid
flowchart TB
    new["a new provider<br/>(Nova, Mistral, OpenAI…)"] --> step1["1 · implement llm.Provider<br/>report your own Capabilities"]
    step1 --> step2["2 · add one case to internal/providers.build"]
    step2 --> done["done"]

    router["internal/router"] -.->|"never touched<br/>routes by Capabilities, not by name"| done
    svc["llm.Service · tool loop · every caller"] -.->|"never touched<br/>they take the interface"| done

    test["architecture_test.go"] ==>|"fails the build if the<br/>router imports a vendor"| router

    classDef pick fill:#0b6,stroke:#083,color:#fff;
    class done pick;
```
