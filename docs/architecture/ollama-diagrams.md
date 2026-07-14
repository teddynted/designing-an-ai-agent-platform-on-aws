# Inference Diagrams — Milestone 7

> **Milestone 7 — Ollama Integration.**
> These diagrams describe the integration in [`internal/llm`](../../internal/llm)
> (the provider abstraction) and [`internal/ollama`](../../internal/ollama) (the
> client). They accompany the blog post,
> [Running Local LLMs with Ollama on AWS](../blog/running-local-llms-with-ollama-on-aws.md),
> and the reference, [INFERENCE.md](../../INFERENCE.md).
>
> **Ollama itself is not deployed here.** The instance, the GPU and the models on
> disk belong to `ollama-on-aws`. This repository owns *the provider abstraction that
> calls it*.

> **This is a snapshot of Milestone 7.** For what is deployed **today**, see
> **[The Platform As Built](current-architecture.md)**, the living diagram.

Five diagrams, sharing the colour key of the earlier sets.

## 1. Two consumers of inference

The correction to a claim Milestone 6 made in bold, and the reason it is not a
contradiction.

```mermaid
flowchart TB
    gh(["GitHub event"]) --> app["Platform"]
    app --> n8n["n8n — orchestration (M5)"]

    n8n --> fork{"What kind of work?"}

    fork -->|"an ERRAND<br/>open-ended · tools · a loop"| oc
    fork -->|"a FUNCTION CALL<br/>one prompt · one completion"| svc

    subgraph agentplane["Agent plane (M6) — another repository"]
        oc["OpenClaw"] --> ocm["its OWN model<br/>the platform is not in this path"]
    end

    subgraph infplane["Inference plane (M7) — THIS repository"]
        svc["llm.Service<br/>validate · fit · correlate · log"]
        svc --> prov{{"llm.Provider<br/>(interface)"}}
        prov --> ollama["Ollama<br/>LOCAL — the prompt does not leave"]
        prov -.-> future["Bedrock (M8)<br/>Claude (M9)<br/>router (M10)"]
    end

    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef ext fill:#E8E8E8,stroke:#666,color:#232F3E
    classDef fut fill:#FFF,stroke:#999,stroke-dasharray: 5 5,color:#666
    class app,n8n,svc,prov aws
    class gh,oc,ocm,ollama ext
    class future fut
```

Milestone 6 said *"the platform calls no model; the agent does"*. That is still true
of **the agent's** inference — it calls its own model, behind its own boundary, and
nothing here is in that path.

What has changed is that **not everything worth doing with a model needs an agent.**
"Summarise this diff" is one prompt and one completion: no shell, no tools, no loop.
Routing it through an agent means paying for an errand when what you wanted was a
function call. So the platform now has an inference plane of its own — the one the
architecture has had on paper since Milestone 1.

## 2. The provider abstraction

Why the interface exists *before* there is a second provider: because the second,
third and fourth are the roadmap.

```mermaid
flowchart LR
    subgraph callers["Callers"]
        c1["release notes"]
        c2["diff summary"]
        c3["classification"]
    end

    c1 & c2 & c3 --> svc["llm.Service<br/>validate · context-fit · correlate · log<br/>(identical for EVERY provider)"]
    svc --> iface{{"llm.Provider"}}

    iface --> ol["Ollama (M7)<br/>Local: true · cost: 0<br/>the prompt does not leave"]
    iface -.-> br["Bedrock (M8)<br/>Local: false"]
    iface -.-> cl["Claude (M9)<br/>Local: false · frontier"]
    iface -.-> rt["Router (M10)<br/>chooses per request on<br/>Capabilities{Local, cost, context}"]

    classDef aws fill:#FF9900,stroke:#232F3E,color:#232F3E
    classDef good fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    classDef fut fill:#FFF,stroke:#999,stroke-dasharray: 5 5,color:#666
    class svc,iface aws
    class ol good
    class br,cl,rt fut
```

A provider abstraction retro-fitted at Milestone 10, on top of three call sites that
each learned Ollama's JSON shape, is a rewrite. Added now, it is an interface with one
implementation.

**`internal/llm` does not import `internal/ollama`** — the mechanical test that the
seam is real rather than decorative.

## 3. Streaming, and the timeout that actually works

```mermaid
sequenceDiagram
    autonumber
    participant S as llm.Service
    participant O as Ollama
    participant Sink as the caller

    S->>O: POST /api/generate {stream: true}
    Note over S: arm the idle timer (60s)

    loop each token
        O-->>S: NDJSON chunk
        Note over S: RESET the idle timer —<br/>the model is alive
        S->>Sink: token
    end

    O-->>S: {done: true, eval_count, eval_duration}
    Note over S: tokens/sec — below 10 means CPU
```

```mermaid
flowchart TB
    q{"Has it produced a token<br/>in the last 60 seconds?"}

    q -->|yes| alive["The model is ALIVE.<br/>Slow is not broken —<br/>a CPU generation takes minutes."]:::good
    q -->|no| dead["ErrStalled.<br/>The model is swapping, or wedged."]:::bad

    tot["A TOTAL timeout cannot ask this question.<br/>Set it long enough for a legitimate slow generation<br/>and it waits just as patiently for one that hung instantly."]:::bad

    classDef good fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    classDef bad fill:#D13212,stroke:#7D1D0C,color:#FFFFFF
```

The useful question is not *"has this finished?"* but *"has it produced a single token
recently?"* — and only a stream can answer it. That is why streaming is the default and
why `OLLAMA_IDLE_TIMEOUT` matters more than `OLLAMA_TIMEOUT`.

## 4. A retry is safe here — until the first token

The mirror image of the last two milestones.

```mermaid
flowchart TB
    subgraph before["Milestones 5 and 6 — retries were DANGEROUS"]
        m5["retry an n8n trigger<br/>→ the workflow runs TWICE"]:::bad
        m6["retry an agent submit<br/>→ a SECOND pull request, and a second bill"]:::bad
    end

    subgraph now["Milestone 7 — a retry is SAFE"]
        m7["retry an inference<br/>→ it costs COMPUTE. Nothing else.<br/>No side effects. Nothing to deduplicate."]:::good
    end

    subgraph except["…with one exception"]
        e1{"Has a token reached the caller?"}
        e1 -->|"no"| ok["Retry freely.<br/>They have seen nothing."]:::good
        e1 -->|"YES"| no["ErrStreamBroken — TERMINAL.<br/>A retry would hand them<br/>a SECOND BEGINNING,<br/>glued onto the first."]:::bad
    end

    classDef good fill:#3F8624,stroke:#243B0B,color:#FFFFFF
    classDef bad fill:#D13212,stroke:#7D1D0C,color:#FFFFFF
```

The sink is a **side effect**: the caller may already have written those tokens to a
terminal, a websocket, or a file. This applies to stalls too — a stall after output is
a stream that broke by going quiet — so the error wraps **both**: `ErrStalled` (the
cause, which the log reports) and `ErrStreamBroken` (the consequence, which stops the
retry).

## 5. The silent-truncation trap

The failure that does the most damage while looking most like success.

```mermaid
flowchart TB
    big["A 13,000-token prompt<br/>(a large diff)"] --> win{"Does it fit the<br/>4,096-token window?"}

    win -->|"send it anyway"| trunc["The model does NOT refuse.<br/>It silently DROPS the beginning<br/>and answers from what is left."]:::bad
    trunc --> conf["A plausible, CONFIDENT, WRONG summary.<br/>No error. Nothing in any log.<br/>It reads fine until someone notices<br/>it is about the wrong commit."]:::bad

    win -->|"llm.Service checks FIRST"| refuse["ErrContextExceeded — REFUSED.<br/>'~13334 tokens into a 4096-token window.<br/>The model would silently drop the beginning.<br/>Summarise or chunk the input instead.'"]:::good

    classDef bad fill:#D13212,stroke:#7D1D0C,color:#FFFFFF
    classDef good fill:#3F8624,stroke:#243B0B,color:#FFFFFF
```

The estimate is deliberately **pessimistic** — code tokenises far worse than prose, so
refusing a prompt that would have fitted is a much better way to be wrong than allowing
one that gets quietly halved.

Which makes `OLLAMA_CONTEXT_TOKENS` the one setting where **being wrong is invisible**.
