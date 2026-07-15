# Webhook Diagrams — Milestone 12

> **Milestone 12 — GitHub Webhook Automation.**
> These diagrams describe [`infra/lambda/internal/webhook`](../../infra/lambda/internal/webhook)
> (the handler) and [`infra/cloudformation/09-webhook.yaml`](../../infra/cloudformation/09-webhook.yaml)
> (the stack). They accompany the blog post,
> [Automating AI Workflows with GitHub Webhooks](../blog/automating-ai-workflows-with-github-webhooks.md),
> and the reference, [WEBHOOKS.md](../../WEBHOOKS.md).
>
> **The webhook does no work.** It verifies, filters, and publishes an event, then returns. n8n,
> OpenClaw, and the models are downstream consumers of the bus, on their own schedule.

## Contents

- [1. High-level architecture](#1-high-level-architecture)
- [2. The webhook sequence](#2-the-webhook-sequence)
- [3. Event routing](#3-event-routing)
- [4. The event lifecycle](#4-the-event-lifecycle)
- [5. Filtering: the decision](#5-filtering-the-decision)
- [6. Failure and retry](#6-failure-and-retry)
- [7. Component interaction](#7-component-interaction)

## 1. High-level architecture

GitHub is the producer; a Lambda behind a Function URL is the authenticated entry point;
EventBridge is the decoupling seam; everything else is a consumer.

```mermaid
flowchart TB
    gh["GitHub repository"] -->|"HTTPS POST + HMAC signature"| url["Lambda Function URL<br/>(AuthType NONE — auth is the signature)"]
    url --> lambda["webhook Lambda<br/><b>verify · parse · filter · publish</b>"]
    lambda --> secret["Secrets Manager<br/><i>shared secret (cold start)</i>"]
    lambda -->|"curated GitHubEvent"| bus["EventBridge<br/>platform bus"]

    bus -->|"rule: source=github"| apidest["API destination"]
    apidest --> n8n["n8n"]
    n8n --> oc["OpenClaw"]
    oc --> router["router (M10)"]
    router --> claude["Claude / Bedrock"]
    router --> ollama["Ollama"]

    classDef edge fill:#0b6,stroke:#083,color:#fff;
    class lambda edge;
```

## 2. The webhook sequence

One delivery, verified and published. Note the order: verify **before** parse, filter **before**
publish.

```mermaid
sequenceDiagram
    autonumber
    participant GH as GitHub
    participant L as Lambda
    participant SM as Secrets Manager
    participant EB as EventBridge

    Note over L: at cold start
    L->>SM: GetSecretValue (once)
    SM-->>L: shared secret

    GH->>L: POST / (event, delivery id, signature, body)
    L->>L: VERIFY signature over raw body (constant time)
    alt bad or missing signature
        L-->>GH: 401 (refused; nothing published)
    else authentic
        L->>L: PARSE (only the routing fields)
        L->>L: FILTER (supported? fork? archived? allow-list? branch?)
        alt filtered or ping
            L-->>GH: 200 (ignored/acknowledged; nothing published)
        else accepted
            L->>EB: PutEvents (curated GitHubEvent)
            alt published
                L-->>GH: 202 (accepted)
            else publish failed
                L-->>GH: 500 (retry me — same delivery id)
            end
        end
    end
```

## 3. Event routing

The curated event on the bus is matched by a rule and delivered to n8n via an API destination.
EventBridge is the point where one producer becomes many possible consumers.

```mermaid
flowchart LR
    lambda["webhook Lambda"] -->|"source: aiap.env.github<br/>detail-type: GitHub Event"| bus["platform bus"]

    bus --> rule["EventBridge rule"]
    rule -->|"assumes rule role"| apidest["API destination<br/>(rate-limited)"]
    apidest -->|"POST + auth header"| n8n["n8n webhook"]

    bus -.->|"future consumers<br/>(audit, replay, DLQ)"| future["added without<br/>touching the Lambda"]

    note["The Lambda publishes once.<br/>Who consumes is a bus concern,<br/>not a webhook concern."] -.-> bus
```

## 4. The event lifecycle

A delivery's disposition — the three outcomes the logs distinguish, because "dropped",
"rejected", and "processed" are different things.

```mermaid
stateDiagram-v2
    [*] --> Received
    Received --> Refused: bad/missing signature (401)
    Received --> Malformed: signed but unparseable (400)
    Received --> Verified: signature ok

    Verified --> Acknowledged: ping (200)
    Verified --> Ignored: a filter dropped it (200)
    Verified --> Accepted: passed every filter

    Accepted --> Published: PutEvents ok (202)
    Accepted --> PublishFailed: PutEvents failed (500 → GitHub retries)

    Refused --> [*]
    Malformed --> [*]
    Acknowledged --> [*]
    Ignored --> [*]
    Published --> [*]
    PublishFailed --> [*]

    note right of Ignored
        A success: the request was
        authentic; the platform simply
        had nothing to do.
    end note
```

## 5. Filtering: the decision

Pure function of the delivery and the config. Safe-first (the drops where processing would be
*wrong*), then cheap-first (the merely uninteresting).

```mermaid
flowchart TD
    d["parsed delivery"] --> ping{"ping?"}
    ping -->|"yes"| ack["Acknowledged"]
    ping -->|"no"| supp{"supported &<br/>in configured set?"}
    supp -->|"no"| ign1["Ignored: unsupported"]
    supp -->|"yes"| fork{"fork?<br/>(security-adjacent)"}
    fork -->|"yes"| ign2["Ignored: fork"]
    fork -->|"no"| arch{"archived?"}
    arch -->|"yes"| ign3["Ignored: archived"]
    arch -->|"no"| repo{"on repo<br/>allow-list?"}
    repo -->|"no"| ign4["Ignored: repo"]
    repo -->|"yes"| del{"branch deletion?"}
    del -->|"yes"| ign5["Ignored: deletion"]
    del -->|"no"| branch{"branch matches<br/>allow-list?"}
    branch -->|"no"| ign6["Ignored: branch"]
    branch -->|"yes"| acc["Accepted → publish"]

    classDef ok fill:#0b6,stroke:#083,color:#fff;
    class acc ok;
```

## 6. Failure and retry

Which failures ask GitHub to retry, and which are terminal. Only a publish failure is retryable —
everything else fails the same way forever, so retrying it is a storm.

```mermaid
flowchart TD
    req["delivery"] --> auth{"authentic?"}
    auth -->|"no"| r401["401 — terminal<br/><i>a retry fails identically</i>"]
    auth -->|"yes"| parse{"parseable?"}
    parse -->|"no"| r400["400 — terminal"]
    parse -->|"yes"| want{"wanted?"}
    want -->|"no"| r200["200 — done (ignored)"]
    want -->|"yes"| pub{"published?"}
    pub -->|"yes"| r202["202 — done"]
    pub -->|"no"| r500["500 — RETRYABLE<br/>GitHub redelivers, same delivery id,<br/>downstream idempotency makes it safe"]

    classDef bad fill:#c33,stroke:#900,color:#fff;
    classDef good fill:#0b6,stroke:#083,color:#fff;
    class r401,r400 bad;
    class r202,r200 good;
```

## 7. Component interaction

Responsibilities and boundaries. The Lambda is in the `infra/lambda` module (edge
infrastructure), separate from the platform's application logic — it needs neither, because it
publishes an event rather than calling anything.

```mermaid
flowchart TB
    subgraph edge["infra/lambda module"]
        cmd["cmd/webhook<br/><i>Function URL adapter</i>"] --> pkg["internal/webhook<br/><b>signature · parse · filter · handler</b>"]
        pkg --> ebif["EventsAPI (interface)"]
    end

    ebif -.->|"real"| sdk["eventbridge SDK"]
    ebif -.->|"test"| fake["fake (no AWS)"]

    subgraph cfn["09-webhook.yaml"]
        fn["Lambda + Function URL"]
        sec["Secrets Manager secret"]
        iam["least-privilege role"]
        route["rule + API destination → n8n"]
    end

    classDef core fill:#0b6,stroke:#083,color:#fff;
    class pkg core;
```
