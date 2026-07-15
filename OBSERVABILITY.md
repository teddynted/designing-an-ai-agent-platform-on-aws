# Observability — the reference

**Milestone 13.** How the platform is made observable: what it logs, what it
measures, what it shows, and what it wakes you up for. This is the operational
companion to the blog post,
[Monitoring an AI Agent Platform with CloudWatch](docs/blog/monitoring-an-ai-agent-platform-with-cloudwatch.md),
and the diagrams in
[observability-diagrams.md](docs/architecture/observability-diagrams.md).

> **The one idea.** A structured log line and a metric are the same bytes seen two
> ways. The platform emits **one** line; CloudWatch reads it as a searchable log
> and — when it matches a metric filter, or carries an EMF envelope — as a
> graphable, alarmable metric. Nothing is measured twice, and (for EMF) measuring
> costs no more than logging already did.

---

## Contents

- [What this milestone builds](#what-this-milestone-builds)
- [Architecture](#architecture)
- [Logging standards](#logging-standards)
- [Metrics collected](#metrics-collected)
- [Dashboards](#dashboards)
- [Alarms](#alarms)
- [Health monitoring](#health-monitoring)
- [Distributed tracing (X-Ray)](#distributed-tracing-x-ray)
- [The `observe` CLI](#the-observe-cli)
- [Configuration](#configuration)
- [Deploy it](#deploy-it)
- [Operational troubleshooting](#operational-troubleshooting)
- [Cost, security, scalability](#cost-security-scalability)
- [Well-Architected](#well-architected)
- [Future improvements](#future-improvements)

---

## What this milestone builds

| Piece | Where | What it is |
| --- | --- | --- |
| Observability library | [`internal/observability`](internal/observability) | The platform's **one** logging/metrics/health standard — a leaf that anything may import and that imports nothing of ours. |
| `observe` CLI | [`cmd/observe`](cmd/observe) | See a real log + metric line, probe a dependency, or run the health server. |
| Monitoring stack | [`infra/cloudformation/10-monitoring.yaml`](infra/cloudformation/10-monitoring.yaml) | Dashboards, alarms, metric filters, an SNS topic, and a telemetry IAM policy. |
| Host metrics | [`infra/scripts/ami/provision.sh`](infra/scripts/ami/provision.sh) | The baked CloudWatch agent now ships **memory and disk**, not just logs. |
| Lambda tracing | [`infra/cloudformation/09-webhook.yaml`](infra/cloudformation/09-webhook.yaml) | X-Ray **active tracing** on the webhook Lambda, the platform's one owned Lambda. |

Milestone 2's [`06-observability.yaml`](infra/cloudformation/06-observability.yaml)
already decided **where** logs land and **how long** they are kept. This milestone
decides what the platform **shows** and what it **alarms on**. The two stacks are
deliberately separate: retention is a data-lifecycle concern that predates any
dashboard.

---

## Architecture

Three metric sources feed one place an operator looks:

- **`AWS/EC2`** and **`AWS/Lambda`** — the host and function basics AWS emits for
  free (CPU, network, status checks; invocations, errors, duration, throttles).
- **`CWAgent`** — memory and disk, which AWS does *not* see from outside the guest,
  shipped by the CloudWatch agent baked into the AMI.
- **The platform's own namespaces** — `aiap/<env>/logs` (metric filters over the
  structured logs) and `aiap/app` (EMF application metrics).

See [the monitoring-architecture diagram](docs/architecture/observability-diagrams.md#1-monitoring-architecture).

---

## Logging standards

Every service logs **structured JSON** through `observability.New(cfg)`, which
returns a plain `*slog.Logger` — so a package that already takes a `*slog.Logger`
(and they all do) adopts the standard by changing *how the logger is built*, in one
place per binary, not by changing a single call site.

### The standard fields

Stamped consistently so one CloudWatch Logs Insights query can follow a unit of
work across the whole platform. They are constants in the package, never string
literals, because a dashboard is built on these names.

| Field | Meaning |
| --- | --- |
| `time`, `level`, `msg` | slog's own — timestamp, severity, message. |
| `service` | The process: `webhook`, `workflow`, `loop`. |
| `component` | The part within a service: `engine`, `controller`, `health`. |
| `correlationId` | The platform's cross-service thread, derived stably from the originating event. **The single most useful field in an incident.** |
| `workflowId` | The workflow or its logical name. |
| `executionId` | A long-running unit of work — an OpenClaw or n8n execution. |
| `requestId` | The inbound request/invocation ID (the AWS request ID in a Lambda). |
| `traceId` | The X-Ray trace ID, when present — joins a log line to a trace. |
| `error`, `errorKind` | A failure and its **stable class** (`timeout`, `unauthorized`), so an alert matches the kind, not a message someone will reword. |

The correlation fields ride on the `context.Context`
(`observability.WithFields(ctx, …)`), so a function deep in the stack logs the
right IDs without threading them through its signature.

### Redaction — secrets and source code never land in a log group

Redaction is a property of the **handler**, not a discipline asked of each caller.
Two kinds of value are replaced with `[REDACTED]` at any depth, by key:

- **Credentials** — `token`, `secret`, `password`, `authorization`, an API key. A
  log group is a database that gets backed up; a token in one is compromised.
- **Repository content** — `prompt`, `completion`, a raw `payload`. These are
  *somebody's source code*, and a log group is not where it should end up. The
  inference plane already logs a size and a hash instead; this is the backstop for
  the line that forgets.

Matching is on a normalised key (case-insensitive, separators stripped) and a
suffix, so `api_key`, `API-Key`, `n8n_api_key` and `webhook_token` are all caught.

---

## Metrics collected

### Infrastructure (`AWS/EC2`, `CWAgent`)

| Metric | Namespace | Notes |
| --- | --- | --- |
| CPU utilization | `AWS/EC2` `CPUUtilization` | Free from EC2. |
| Network in/out | `AWS/EC2` `NetworkIn`/`NetworkOut` | Free from EC2. |
| Status checks | `AWS/EC2` `StatusCheckFailed` | Instance + system health. |
| Memory used % | `CWAgent` `mem_used_percent` | Needs the agent (baked). |
| Disk used % | `CWAgent` `disk_used_percent` | Root volume, needs the agent. |

### Lambda (`AWS/Lambda`)

`Invocations`, `Errors`, `Duration` (graphed at p99), `Throttles`,
`ConcurrentExecutions` — dimensioned by `FunctionName`.

### Application — EMF (`aiap/app`)

Emitted by `observability.Emitter` as EMF. These are the platform's names; a
service that emits them gets the dashboards for free.

| Metric | Unit | Meaning |
| --- | --- | --- |
| `WorkflowInvocations` | Count | Workflows triggered. |
| `WorkflowSuccess` / `WorkflowFailures` | Count | Outcome — the success **rate** is a dashboard math expression over these two. |
| `WorkflowDurationMs` | Milliseconds | Graphed at p50/p99. |
| `AIRequests` | Count | Inference calls (the throughput signal). |
| `AIResponseTimeMs` | Milliseconds | Inference latency, p50/p99. |
| `AIFailures` | Count | Inference/agent failures. |
| `ActiveExecutions` | None | In-flight work (gauge). |
| `QueueLength` | None | Backlog, where applicable (gauge). |
| `AgentExecutions` / `AgentFailures` | Count | OpenClaw activity. |
| `N8nExecutions` | Count | n8n workflow executions. |

### Application — log-derived (`aiap/<env>/logs`)

Extracted by metric filters from the structured logs, so a service that only logs
is still alarmable:

| Metric | Filter |
| --- | --- |
| `Errors` | `{ $.level = "ERROR" }` on the platform log group. |
| `WorkflowFailures` | `{ $.msg = "workflow failed" }`. |
| `RetriesExhausted` | `{ $.retriesExhausted IS TRUE }`. |
| `AIFailures` | `{ $.level = "ERROR" }` on the agent log group. |
| `FailedOrchestrations` | `{ $.errorKind = "output_rejected" }` — an agent's output rejected as credential-shaped. |

---

## Dashboards

Three, by audience. Names are `${ProjectName}-${Environment}-<name>`.

| Dashboard | Answers |
| --- | --- |
| **infrastructure** | Is the host healthy — CPU, memory, disk, network, status checks — and are the Lambdas healthy (invocations, errors, throttles, p99 duration)? |
| **ai-platform** | What is the AI platform doing — workflow success/failure, active executions, OpenClaw activity, n8n executions, inference latency? |
| **application** | Error counts, throughput, **success rate** (a math expression), latency percentiles, and a single-value panel of the alarming signals. |

---

## Alarms

Every alarm publishes both its **ALARM** and its **OK** transition to one SNS topic
(`${ProjectName}-${Environment}-alarms`) — "it recovered" is as useful as "it
broke". Thresholds are all template parameters; the defaults are conservative.

### Infrastructure (only with an `InstanceId`)

| Alarm | Metric | Default |
| --- | --- | --- |
| `cpu-high` | `AWS/EC2 CPUUtilization` | > 85% for 2 periods |
| `memory-high` | `CWAgent mem_used_percent` | > 85% for 2 periods |
| `disk-high` | `CWAgent disk_used_percent` | > 85% |
| `instance-unhealthy` | `AWS/EC2 StatusCheckFailed` | ≥ 1 for 2 minutes |

### Lambda

| Alarm | Metric |
| --- | --- |
| `lambda-webhook-errors` | `AWS/Lambda Errors` ≥ threshold |
| `lambda-webhook-slow` | `AWS/Lambda Duration` p99 > 5000 ms |
| `lambda-webhook-throttles` | `AWS/Lambda Throttles` ≥ 1 |
| `lambda-spot-interruption-errors` | `AWS/Lambda Errors` |
| `lambda-spot-statechange-errors` | `AWS/Lambda Errors` |

Lambda alarms key on the `FunctionName` **string**, so they deploy and are valid
before the function's own stack exists — they simply sit in `INSUFFICIENT_DATA`
until it reports. The same three-line pattern extends to any function.

### Application

| Alarm | Metric | Meaning |
| --- | --- | --- |
| `workflow-failures` | `WorkflowFailures` | A workflow failed. |
| `ai-failures` | `AIFailures` | Inference/agent failure. |
| `failed-orchestrations` | `FailedOrchestrations` | An agent output was rejected (security-relevant). |
| `excessive-retries` | `RetriesExhausted` > 5 | A dependency is down, not just a bad request. |

---

## Health monitoring

`observability.Health` serves the two probes an orchestrator actually asks for, and
the distinction between them is where the whole story lives:

- **`GET /healthz` — liveness.** Is *this process* healthy? A failure means
  **restart me**. Dependencies do **not** belong here: restarting does not fix the
  database you cannot reach, it adds a crash loop to an outage.
- **`GET /readyz` — readiness.** Can I do useful work — are my dependencies
  reachable? A failure means **stop routing to me** (`503`, try elsewhere), and is
  the correct home for "is n8n up", "is OpenClaw reachable".

`HTTPCheck` is the check most dependencies need — a 2xx from a URL — and it
deliberately sends **no credentials**: a probe answers "can I reach it", not "can I
use it", so a rotated token does not become a restart loop.

Run the server standalone against the platform's HTTP dependencies:

```bash
observe serve --addr :8080 \
  --target openclaw=http://localhost:8088 \
  --target n8n=https://n8n.internal,/healthz
```

---

## Distributed tracing (X-Ray)

**What is integrated.** The webhook Lambda runs with **active tracing**
(`TracingConfig: Active`), so the Lambda service emits a service-boundary segment
per invocation and sets `_X_AMZN_TRACE_ID`. `observability.TraceIDFrom(ctx)` reads
that (and the `X-Amzn-Trace-Id` header) and stamps `traceId` on every log line — so
a slow request in a trace and the log lines explaining it are one click apart, not
one investigation apart.

**What is deliberately *not*.** This package does not create segments or sample.
That is the X-Ray SDK's (or ADOT's) job; a half-instrumented trace looks complete
and is not, which is worse than none.

**Where tracing cannot reach, and why:**

- **The EC2 workloads — OpenClaw, Ollama, n8n — are deployed by their own
  repositories.** The platform traces its own Lambda; it cannot instrument the
  inside of a service it does not deploy. A trace that reaches the OpenClaw boundary
  and stops there is the honest shape.
- **A trace does not survive EventBridge.** When the webhook publishes an event and
  n8n later consumes it, that is an *intentional* decoupling (see
  [WEBHOOKS.md](WEBHOOKS.md)) — two transactions, joined by the **correlation ID**,
  not one trace. Drawing a trace across it would assert a causal line the
  architecture specifically does not have.

The correlation ID is what carries the story across those boundaries.

---

## The `observe` CLI

```bash
# SEE what lands in CloudWatch — a structured log line and its EMF metric line.
OBS_METRICS_NAMESPACE=aiap/app observe emit \
  --metric WorkflowDurationMs=1450 --dim Workflow=blog-generator

# PROBE dependencies (exit != 0 if any is down — usable in CI).
observe health --target openclaw=http://localhost:8088 --target n8n=https://n8n.internal

# BE the health endpoint.
observe serve --addr :8080 --target openclaw=http://localhost:8088
```

---

## Configuration

Everything is an environment variable; nothing is compiled in.

| Variable | Default | Meaning |
| --- | --- | --- |
| `OBS_SERVICE` | — | Process name, stamped on every line. |
| `OBS_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `OBS_LOG_FORMAT` | `json` | `json` (machines) \| `text` (a terminal). |
| `OBS_LOG_SOURCE` | `false` | Add a file:line to each record. |
| `OBS_METRICS_NAMESPACE` | — | EMF namespace (e.g. `aiap/app`). Empty disables metric emission. |
| `OBS_METRICS_ENABLED` | `true` | Force metrics off even with a namespace set. |

Infrastructure parameters (thresholds, `AlarmEmail`, `AppMetricNamespace`) are on
the [monitoring stack](infra/cloudformation/10-monitoring.yaml).

---

## Deploy it

```bash
cd infra

# The core observability foundation (log groups) is part of `make deploy`.
make deploy

# Enable X-Ray tracing on the webhook by redeploying it (additive).
make webhook

# Dashboards, alarms, metric filters. Resolves the instance from 03-compute.
make monitoring ALARM_EMAIL=you@example.com

make outputs                     # includes 10-monitoring
```

Memory and disk metrics need the updated CloudWatch agent config, which is baked
into the AMI — `make ami && make deploy-ami` to pick it up on the instance.

---

## Operational troubleshooting

| Symptom | Likely cause | What to check |
| --- | --- | --- |
| A workflow never happened | The trigger, an engine failure, or a silent filter | Query the log group for the GitHub delivery's `correlationId`; the `requested`/`completed`/`failed` lines tell you where it stopped. |
| An alarm is `INSUFFICIENT_DATA` | The metric isn't reporting (yet) | For a host alarm after an interruption, the instance was replaced — the alarm's `InstanceId` is stale until Milestone 16's ASG. For a Lambda alarm, the function may not be deployed. |
| `memory-high`/`disk-high` never fire | The agent isn't shipping the metric | Confirm the AMI has the metrics section (`make ami` after this milestone) and the agent is running: `amazon-cloudwatch-agent-ctl -a status`. |
| An EMF metric doesn't appear | Namespace unset, or the envelope is malformed | Confirm `OBS_METRICS_NAMESPACE` is set; run `observe emit` and read the `_aws` block; extraction is asynchronous (allow a minute). |
| A secret appeared in a log | A key the redactor doesn't know | Add the key's normalised form to `sensitiveKeys` in `internal/observability/redact.go` — and rotate the secret. |
| No alarm emails | The SNS subscription isn't confirmed | Confirm the subscription in the email AWS sent; check the topic has a confirmed subscriber. |

---

## Cost, security, scalability

**Cost.** The design is cost-aware by construction. EMF metrics cost what the logs
already cost — there is no separate `PutMetricData` bill — and custom metrics are
only emitted where a service opts in. Log **retention** is short by default
(14 days) and raised per environment. The expensive mistakes CloudWatch invites are
*high-cardinality dimensions* (never dimension on an ID) and *one-minute alarm
periods everywhere* (the default here is five). Both are avoided deliberately.

**Security.** Redaction keeps credentials and repository content out of the log
groups by construction. The telemetry IAM policy is least-privilege:
`PutMetricData` is scoped to the platform's own namespaces with the
`cloudwatch:namespace` condition key (the only scope the API accepts), and X-Ray is
write-only telemetry, not data access. No observability component reads application
data or holds a credential.

**Scalability.** The seam that makes this scale is that observability is a **leaf**
library. A new service adopts the standard by constructing its logger through the
package; a new metric is a call; a new alarm is a few lines of CloudFormation on a
metric that already exists. Per-instance host alarms are the one thing that does not
scale as-is — the Auto Scaling group (Milestone 16) moves them to an ASG dimension.

---

## Well-Architected

The milestone maps onto the AWS Well-Architected **Operational Excellence** and
**Reliability** pillars:

- **Detect, then respond.** Metrics and dashboards make state visible; alarms and
  the SNS path turn a breach into a notification.
- **Anticipate failure.** The `excessive-retries` and `instance-unhealthy` alarms
  fire on the *approach* to failure, not only the arrival.
- **Refuse the confused deputy.** `failed-orchestrations` alarms on an agent output
  rejected as credential-shaped — observability serving security.
- **Make the right thing the easy thing.** Redaction and the standard fields are
  properties of the shared logger, so a hurried change inherits them.

---

## Future improvements

Deliberately **not** built in this milestone:

- **Composite alarms** — one "platform unhealthy" roll-up over the individual
  alarms, to cut noise.
- **Anomaly-detection alarms** on latency and throughput, instead of static
  thresholds.
- **A CloudWatch Logs subscription** to ship logs to a longer-term store (S3/OpenSearch)
  for cheap retention beyond the log-group window.
- **ASG-dimensioned host alarms** — the fix for per-instance alarms going stale on a
  Spot replacement. *(Milestone 16.)*
- **ADOT / X-Ray SDK segments** inside the platform's own long-running services,
  once they run, for spans finer than the Lambda service boundary.
- **A dead-letter queue alarm** once the event bus grows DLQs.
