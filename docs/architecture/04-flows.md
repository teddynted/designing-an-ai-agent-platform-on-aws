# 4. Request and Event Flows

Five flows define the platform's runtime behaviour. Flows 4.4 and 4.5 are the ones that make the cost model safe; they deserve as much attention as the happy paths.

## 4.1 Inbound webhook → deterministic workflow

The classic n8n path. Synchronous acknowledgement, asynchronous execution.

```mermaid
sequenceDiagram
    autonumber
    participant C as External caller
    participant A as ALB (public)
    participant M as n8n main (On-Demand)
    participant R as Redis
    participant W as n8n worker (Spot)
    participant G as Model Gateway
    participant B as Bedrock

    C->>A: POST /webhook/... (HTTPS)
    A->>M: forward
    M->>M: authenticate, validate, assign agent_run_id
    M->>R: enqueue execution
    M-->>C: 202 Accepted
    Note over M,C: Ack before execution — the ingress<br/>path never waits on inference.
    R->>W: worker pulls job
    W->>G: POST /v1/chat/completions
    G->>B: InvokeModel (via VPC endpoint)
    B-->>G: tokens
    G-->>W: completion + usage
    G->>G: emit token/cost metric (agent_run_id)
    W->>W: continue workflow nodes
    W-->>R: mark complete
```

**Why acknowledge at step 5, before any inference happens.** Inference latency is unbounded and Spot workers are interruptible. Coupling an HTTP caller's connection to either would make the platform's availability a function of GPU spot capacity. The 202 decouples them. Callers that need a result poll or receive a callback.

**Where it can fail.** If the worker is interrupted mid-execution, the job returns to the queue and another worker retries it. This only works if workflow steps are **idempotent or compensating** — a constraint the platform imposes on its tenants, not a property it provides ([12 — Constraints](12-risks-assumptions-constraints.md)).

## 4.2 Chat message → autonomous agent → tool execution

The OpenClaw path. Note there is no ingress: the Gateway dialled out.

```mermaid
sequenceDiagram
    autonumber
    participant U as User (WhatsApp/Slack)
    participant P as Chat platform
    participant O as OpenClaw Gateway (private subnet)
    participant G as Model Gateway
    participant B as Bedrock
    participant S as Tool sandbox (Docker)

    Note over O,P: Gateway holds a long-lived OUTBOUND<br/>connection. No inbound ingress exists.
    U->>P: "summarise yesterday's failed runs"
    P-->>O: message over existing connection
    O->>O: resolve session, load history, check allowlist
    O->>G: chat/completions (+ tool schemas)
    G->>B: InvokeModel
    B-->>G: tool_call: shell("...")
    G-->>O: tool_call
    O->>O: policy check: tool allowed? approval required?
    O->>S: exec in sandbox (no IMDS, egress allowlist)
    S-->>O: stdout / exit code
    O->>G: chat/completions (+ tool result)
    G->>B: InvokeModel
    B-->>G: final answer
    G-->>O: completion
    O-->>P: reply
    P-->>U: reply
```

**Step 6 is the security boundary of the entire platform.** The model has just been asked, by text that may have originated from an untrusted source, to run a shell command. Everything in [08 — Security](08-security.md) exists to constrain what happens between steps 6 and 8. The sandbox has no instance-profile credentials (IMDS blocked), a deny-by-default egress allowlist, and resource limits.

**Step 4 is the second boundary.** The channel allowlist decides whose messages become agent turns at all. An open Gateway is an open shell.

## 4.3 AWS or scheduled event → workflow or agent

```mermaid
sequenceDiagram
    autonumber
    participant SRC as S3 / schedule / AWS service
    participant EB as EventBridge bus
    participant L as Lambda: event-router
    participant M as n8n main
    participant O as OpenClaw Gateway

    SRC->>EB: event (e.g. ObjectCreated, cron)
    EB->>EB: rule match + input transform
    EB->>L: invoke
    L->>L: enrich, assign agent_run_id, choose target
    alt deterministic, known steps
        L->>M: trigger workflow (internal endpoint)
    else open-ended, needs reasoning
        L->>O: create agent task
    end
```

The router's `alt` branch is the platform's core routing question: **are the steps known in advance?** If yes it is a workflow, and n8n gives inspectability and audit. If no it is an agent, and OpenClaw gives adaptability at the cost of determinism. Sending open-ended work to n8n produces brittle DAGs; sending known procedures to an agent burns tokens and introduces nondeterminism where none was needed.

## 4.4 Scale-to-zero inference (the cost mechanism)

This flow is why the GPU fleet costs nothing overnight.

```mermaid
sequenceDiagram
    autonumber
    participant W as n8n worker / agent
    participant G as Model Gateway
    participant Q as SQS (bulk queue)
    participant AL as CloudWatch alarm
    participant LS as Lambda: scaler
    participant ASG as Ollama Spot ASG
    participant B as Bedrock

    W->>G: inference request (bulk, async)
    G->>G: routing policy: latency-tolerant?
    alt interactive / latency-sensitive
        G->>B: route to Bedrock (no cold start)
        B-->>G: tokens
    else bulk / async
        G->>Q: enqueue
        Q->>AL: ApproximateNumberOfMessagesVisible > 0
        AL->>LS: alarm
        LS->>ASG: set desired capacity N
        Note over ASG: ~2–4 min cold start:<br/>golden AMI + baked weights.<br/>No warm pool — Spot ASGs can't use them.
        ASG->>Q: workers poll and drain
        alt Spot capacity unavailable
            LS->>G: signal degraded
            G->>B: fall back to Bedrock
            Note over G,B: Degradation is a COST event,<br/>not an outage.
        end
    end
    Note over AL,ASG: Idle N minutes → scaler sets desired = 0
```

Three things are doing the work here:

1. **SQS holds the backlog** so the ~2–4 minute cold start is absorbed rather than experienced by a caller. EventBridge could not do this — it routes, it does not buffer a measurable depth.
2. **The routing policy is latency-based**, not preference-based. Interactive traffic goes to Bedrock because Bedrock has no cold start. Bulk traffic goes to Spot GPUs because at volume the per-token economics invert.
3. **Bedrock is the backstop.** Without it, running inference on Spot would trade cost for availability. With it, a Spot capacity shortfall degrades the *bill*, not the *service*. This is the payoff for the Model Gateway seam.

## 4.5 Spot interruption (the availability mechanism)

Two minutes is enough, but only if nothing in the path needs a human.

```mermaid
sequenceDiagram
    autonumber
    participant EC2 as EC2 Spot service
    participant EB as EventBridge
    participant LD as Lambda: spot-drain
    participant TG as ALB target group
    participant W as Worker instance
    participant Q as Queue

    EC2->>EB: Spot Instance Interruption Warning (T-2 min)
    EB->>LD: invoke
    par
        LD->>TG: deregister target (stop new traffic)
    and
        LD->>W: SSM: stop pulling new jobs
    end
    W->>W: finish or checkpoint current job
    W->>Q: requeue in-flight work (visibility timeout expiry)
    W-->>LD: drained
    EC2->>W: reclaim
    Note over EB: Separately, Capacity Rebalance lets the<br/>ASG launch a replacement BEFORE reclamation.
```

**Capacity Rebalance is the more important half** and is easy to overlook. The interruption warning is reactive with a hard 2-minute budget; the rebalance recommendation arrives *earlier*, on a signal that the instance is at elevated risk, and lets the ASG launch a replacement proactively. Enable both. Rely on rebalance; treat the 2-minute drain as the fallback.

For **long inference requests** that cannot complete in two minutes, checkpointing is impractical — the request is simply retried elsewhere, or routed to Bedrock. This is acceptable precisely because inference is stateless and idempotent. It is *not* acceptable for the OpenClaw Gateway, which is exactly why the Gateway is not on Spot.

## 4.6 Trace propagation

One identifier, `agent_run_id`, is minted at the entry point of every flow above (n8n `main`, the event router, or the Gateway on a new turn) and propagated through every hop, including into Model Gateway token metrics.

```
agent_run_id ─┬─> n8n execution record
              ├─> OpenClaw session turn
              ├─> sandbox container label
              ├─> CloudWatch log field (structured)
              └─> token + cost metric dimension
```

This makes the platform's most important operational question answerable: *what did this agent run do, how long did it take, what did it cost, and what did it touch?* Without it, per-agent cost attribution and incident forensics are guesswork. See [10 — Operations](10-operations.md).
