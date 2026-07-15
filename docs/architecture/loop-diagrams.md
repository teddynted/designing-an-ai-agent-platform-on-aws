# Loop Diagrams — Milestone 11

> **Milestone 11 — Loop Engineering.**
> These diagrams describe [`internal/loop`](../../internal/loop) (the controller) and
> [`internal/loop/adapter`](../../internal/loop/adapter) (the edge that binds it to the
> reasoning and execution planes). They accompany the blog post,
> [Building Autonomous AI Agents with Loop Engineering](../blog/building-autonomous-ai-agents-with-loop-engineering.md),
> and the reference, [LOOP.md](../../LOOP.md).
>
> **No model and no agent are deployed here.** Reasoning is delegated to the inference plane
> (Milestone 7-10), execution to OpenClaw (Milestone 6). This repository owns the **loop
> controller** — which imports neither, and orchestrates both.

## Contents

- [1. High-level architecture](#1-high-level-architecture)
- [2. The agent lifecycle](#2-the-agent-lifecycle)
- [3. State transitions](#3-state-transitions)
- [4. The controller is a reducer](#4-the-controller-is-a-reducer)
- [5. Execution sequence](#5-execution-sequence)
- [6. The retry flow](#6-the-retry-flow)
- [7. The reflection flow](#7-the-reflection-flow)
- [8. Failure recovery](#8-failure-recovery)
- [9. Component interaction](#9-component-interaction)

## 1. High-level architecture

The loop sits above the two planes it orchestrates, and imports neither. Reasoning goes to the
inference plane (routed to Claude or Ollama by Milestone 10); execution goes to OpenClaw.

```mermaid
flowchart TB
    trigger["Workflow trigger"] --> n8n["n8n<br/><i>orchestration + the waiting</i>"]
    n8n --> loop["<b>loop.Runner / reducer</b><br/>plan · evaluate · reflect · decide · stop"]

    loop -->|"REASON<br/>(Planner/Evaluator/Reflector/Summariser)"| adapter1["adapter.Reasoner"]
    loop -->|"EXECUTE<br/>(Executor)"| adapter2["adapter.Executor"]

    adapter1 --> llm["llm.Service<br/>llm.Structured"]
    llm --> router["router (M10)"]
    router --> claude["Claude / Bedrock"]
    router --> ollama["Ollama"]

    adapter2 --> agent["agent.Service"]
    agent --> openclaw["OpenClaw<br/><i>executes one task</i>"]

    classDef core fill:#0b6,stroke:#083,color:#fff;
    class loop core;
```

## 2. The agent lifecycle

The stages the milestone names, and the decisions that sequence them.

```mermaid
flowchart TD
    goal["Goal"] --> plan["Planning"]
    plan --> select["Task selection"]
    select --> exec["Execution<br/><i>OpenClaw</i>"]
    exec --> eval["Evaluation<br/><i>inference</i>"]
    eval --> decision{"Decision"}

    decision -->|"task done, more to do"| select
    decision -->|"goal achieved"| summary["Summary"]
    decision -->|"transient failure"| reflect["Reflection<br/><i>inference</i>"]
    decision -->|"approach wrong"| plan
    decision -->|"budget / human / stuck"| summary

    reflect --> exec

    summary --> done["Result"]

    classDef stop fill:#0b6,stroke:#083,color:#fff;
    class done stop;
```

## 3. State transitions

The reducer's phases. Terminal phases (done, stopped, failed) are where a driver stops calling
`Decide`. A stop is a bound doing its job; a failure is a malfunction — the difference an alert
depends on.

```mermaid
stateDiagram-v2
    [*] --> planning
    planning --> executing: valid plan
    planning --> failed: no usable plan

    executing --> evaluating: task ran (outcome)

    evaluating --> executing: succeeded, next task
    evaluating --> reflecting: transient failure, retry (reflection on)
    evaluating --> executing: transient failure, retry (reflection off)
    evaluating --> planning: replan requested
    evaluating --> summarising: goal achieved / human / stuck

    reflecting --> executing: revised, retry

    executing --> summarising: a bound tripped (checked before every action)
    evaluating --> summarising: a bound tripped

    summarising --> done: goal achieved
    summarising --> stopped: a bound / human hand-off

    done --> [*]
    stopped --> [*]
    failed --> [*]

    note right of summarising
        Even a stopped loop summarises
        first, so a caller always gets
        an account.
    end note
```

## 4. The controller is a reducer

Two pure functions and a driver. The reducer does no I/O; the Runner (or n8n) performs the
actions and feeds results back. This is what makes stops always-enforced and state
recoverable.

```mermaid
flowchart LR
    subgraph pure["PURE — no I/O, testable with struct literals"]
        decide["Decide(state, cfg, now)<br/>→ Action"]
        advance["Advance(state, cfg, result)<br/>→ State"]
    end

    subgraph driver["DRIVER — the Runner, or n8n"]
        perform["perform the Action<br/>(call an engine)"]
    end

    decide -->|"Action: plan/execute/<br/>evaluate/reflect/summarise/stop"| perform
    perform -->|"StepResult"| advance
    advance -->|"new State"| decide

    guard["stopping conditions<br/>checked at the TOP of Decide,<br/>before every action"] -.-> decide
```

## 5. Execution sequence

One interesting turn: a task fails transiently, is reflected on, and succeeds on retry with a
backoff the driver honours.

```mermaid
sequenceDiagram
    autonumber
    participant D as Runner
    participant R as reducer
    participant P as Planner (inference)
    participant E as Executor (OpenClaw)
    participant V as Evaluator (inference)
    participant F as Reflector (inference)

    D->>R: Decide → plan
    D->>P: Plan(goal)
    P-->>D: 1 task
    D->>R: Advance → executing

    D->>R: Decide → execute(task)
    D->>E: Execute (a whole agent run)
    E-->>D: outcome{success:false, transient:true}
    D->>R: Advance → evaluating

    D->>R: Decide → evaluate
    D->>V: Evaluate(outcome)
    V-->>D: {taskSucceeded:false, retry:true}
    D->>R: Advance → reflecting (retry 1)

    D->>R: Decide → reflect
    D->>F: Reflect(failure)
    F-->>D: revised instructions
    D->>R: Advance → executing

    D->>R: Decide → execute (Delay = backoff)
    Note over D: driver waits (sleep / n8n wait node)
    D->>E: Execute (fresh execution, attempt 1)
    E-->>D: outcome{success:true}
    D->>R: Advance → evaluating → done
```

## 6. The retry flow

Only transient failures are retried, and only within budget. Above the per-task budget, the
iteration cap guarantees termination. There is no path that loops forever.

```mermaid
flowchart TD
    fail["task outcome: failure"] --> t{"transient?"}
    t -->|"no (deterministic)"| stop1["do NOT retry<br/><i>a second model repeats it</i>"]
    t -->|"yes"| e{"evaluator asked<br/>to retry?"}
    e -->|"no"| route{"replan?"}
    e -->|"yes"| b{"retries left?<br/>(< MaxRetries)"}
    b -->|"no"| stop2["stop: max-retries"]
    b -->|"yes"| backoff["retry with backoff<br/>attempt++"]

    route -->|"yes"| replan["replan<br/>(bounded by MaxReplans)"]
    route -->|"no"| stop3["stop: critical-failure"]

    cap["iteration cap<br/>checked before EVERY execute"] -.->|"guarantees termination"| backoff

    classDef bad fill:#c33,stroke:#900,color:#fff;
    class stop1,stop2,stop3 bad;
```

## 7. The reflection flow

Reflection changes the agent's behaviour without changing the platform's code: it rewrites the
task's instructions, on the platform's side of the boundary, for the next attempt.

```mermaid
flowchart LR
    failure["failure + evaluator's verdict"] --> reflect["Reflector (inference)"]
    reflect --> analysis["analysis: why it failed"]
    reflect --> revised["revisedInstructions"]

    revised -->|"non-empty"| apply["replace the task's<br/>instructions for the retry"]
    revised -->|"empty (purely transient)"| asis["retry as-is"]

    apply --> history["appended to<br/>ReflectionHistory"]
    asis --> history

    note["Instructions come from the PLATFORM,<br/>never laundered from repository content"] -.-> apply
```

## 8. Failure recovery

The state is a serialisable value, so a Spot reclaim mid-loop loses only the in-flight action.
The pending outcome survives, so a reload never re-runs the expensive execution.

```mermaid
sequenceDiagram
    autonumber
    participant L as loop (process A)
    participant S3 as persisted State
    participant L2 as loop (process B, after reclaim)

    L->>L: execute task (expensive, side-effecting)
    L->>L: Advance → evaluating (PendingOutcome set)
    L->>S3: Marshal + persist
    Note over L: Spot instance reclaimed 💥

    L2->>S3: Load
    L2->>L2: Decide → evaluate the PENDING outcome
    Note over L2: the execution is NOT re-run —<br/>its result rode along in the state
    L2->>L2: continue to completion
```

## 9. Component interaction

Who imports whom. The loop core is a leaf on the reasoning/execution side — the adapter
depends on it and on both planes; the loop depends on neither. The architecture test enforces
the arrows.

```mermaid
flowchart TB
    cmd["cmd/loop<br/><i>composition root</i>"] --> adapter["internal/loop/adapter"]
    cmd --> loop["internal/loop<br/><b>the controller</b>"]

    adapter --> loop
    adapter --> llm["internal/llm"]
    adapter --> agent["internal/agent"]
    adapter --> prompt["internal/prompt"]

    loop -.->|"imports NOTHING of ours<br/>(stdlib only)"| x[" "]

    test["architecture_test.go"] ==>|"fails the build if loop<br/>imports llm or agent"| loop

    classDef core fill:#0b6,stroke:#083,color:#fff;
    class loop core;
    style x fill:none,stroke:none;
```
