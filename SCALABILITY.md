# Scalability — the reference

**Milestone 16.** How the platform grows, which parts grow independently, which
stay deliberately central, and the one piece of infrastructure this milestone adds
to make the first kind of growth possible. This is the operational companion to the
blog post,
[Scaling an AI Agent Platform on AWS](docs/blog/scaling-an-ai-agent-platform-on-aws.md),
and the diagrams in
[scalability-diagrams.md](docs/architecture/scalability-diagrams.md).

> **The one idea.** You cannot scale a thing you have not decoupled. A burst of
> requests can only be *absorbed* if arrival is separated from execution, and the
> only honest way to separate them is a queue. So this milestone adds exactly one
> seam — a durable **work queue** between the event bus and the workers that run
> long-running AI tasks — plus the **queue depth** signal a future fleet scales on.
> It builds the seam and the signal. It does **not** build the fleet: multiple
> OpenClaw workers, a scaled n8n, an EC2 Auto Scaling group are all *drop-in* on
> top of the queue, and that drop-in is a later milestone, not this one.

---

## Contents

- [What this milestone actually ships](#what-this-milestone-actually-ships)
- [Architecture review: where it does and doesn't scale today](#architecture-review-where-it-does-and-doesnt-scale-today)
- [The bottleneck, and the seam that removes it](#the-bottleneck-and-the-seam-that-removes-it)
- [Component-by-component scaling](#component-by-component-scaling)
- [Ollama vs. Amazon Bedrock as a scaling decision](#ollama-vs-amazon-bedrock-as-a-scaling-decision)
- [The provider abstraction is a scaling primitive](#the-provider-abstraction-is-a-scaling-primitive)
- [Infrastructure scaling strategy](#infrastructure-scaling-strategy)
- [What stays central, on purpose](#what-stays-central-on-purpose)
- [Deploy it](#deploy-it)
- [Well-Architected](#well-architected)
- [Explicitly deferred](#explicitly-deferred)
- [Future improvements](#future-improvements)

> **On scope.** This is a **partial implementation** by design. The goal is the
> architectural foundation for horizontal scaling, not a production-scale
> distributed platform. The platform continues to run as a single deployment; what
> changes is that it is now *shaped* to become more than one, without a rewrite.

---

## What this milestone actually ships

One CloudFormation stack, [`13-scalability.yaml`](infra/cloudformation/13-scalability.yaml),
containing five resources that together form a single idea:

| Resource | Role |
| --- | --- |
| **`AgentTaskQueue`** (SQS) | The buffer. Long-running agent tasks wait here instead of blocking on a worker being free. |
| **`AgentTaskDeadLetterQueue`** (SQS) | The holding pen. A task that fails `MaxReceiveCount` times is set aside for a human, so one poison payload can't block the line. |
| **`AgentTaskRule`** (EventBridge) | The router. Selects long-running task events by detail-type off the platform bus and sends them to the queue — heavy work goes async, everything else stays on the fast synchronous path. |
| **`AgentTaskQueuePolicy`** | Least privilege. Only EventBridge, only from this platform's rule, only in this account, may enqueue. |
| **`BacklogAlarm` / `OldestMessageAgeAlarm` / `DeadLetterAlarm`** | The signal. Depth, latency, and poison — the exact metrics a future autoscaler consumes, paging a human until one exists. |

Everything else in this document is the *reasoning* around those five resources:
what they unblock, what they deliberately don't, and how each plane of the platform
scales once the seam is in place.

## Architecture review: where it does and doesn't scale today

The platform as built (Milestones 1–15) is a **single EC2 instance** running Ollama,
n8n, and the OpenClaw integration, fronted by an EventBridge bus and a set of Lambda
functions, with Amazon Bedrock available as a managed fallback. Reviewed honestly
for scale, before this milestone:

**Scalability bottlenecks**

- **The event path was synchronous.** EventBridge invoked a dispatch Lambda that
  did — or fronted — the work. A burst of arrivals had nowhere to *wait*: it either
  fanned out into concurrent executions immediately or was throttled. There was no
  buffer to absorb a spike and no backpressure to shape one.
- **One instance is one throughput ceiling.** Ollama, n8n, and OpenClaw all share
  the single box's CPU, memory, and (eventually) GPU. Two heavy agent tasks contend
  for the same cores; local inference and workflow execution compete.

**Single points of failure**

- **The EC2 instance.** Ollama, n8n, and the OpenClaw runtime all live on it. Lose
  it and local inference, orchestration, and agent execution all stop at once.
  (Spot interruption — Milestone 3 — makes the *loss* graceful, but a replacement is
  still a single instance.)
- **Nothing else.** EventBridge, Lambda, SQS, Bedrock, and S3 are managed,
  regional, and multi-AZ by construction — they are not SPOFs and do not become one
  under load.

**Resource contention**

- Local inference (Ollama), workflow orchestration (n8n), and agent execution
  (OpenClaw) all share one instance. This is fine for a single stream of work and
  the wrong shape for concurrent streams — which is exactly why they must be able to
  scale *independently* rather than by making the one box bigger forever.

**Suitable for horizontal scaling** — stateless, share-nothing, or already managed:

- **Lambda** — already horizontally scaled by AWS; the platform's functions are
  stateless and short-running (see below).
- **OpenClaw workers** — each agent task is independent, so N workers pulling from
  one queue share the load with zero coordination.
- **n8n executions** — with the right execution mode, concurrent runs need not
  serialise.
- **Amazon Bedrock** — managed, elastic, no fleet to run.

**Should stay central** — stateful, coordinating, or a single source of truth:

- **The EventBridge bus** — one bus is the platform's front door; it is regional and
  already scales. Sharding it would add coordination for no throughput.
- **The scheduler and Spot/lifecycle handlers** — they act on the *account* and the
  fleet; there must be exactly one authority deciding when instances start and stop.
- **State and artifacts in S3** — one durable store the workers share, not per-worker
  state that would have to be reconciled.

## The bottleneck, and the seam that removes it

The one bottleneck worth fixing in this milestone is the synchronous event path,
and the fix is a queue. A queue does three things at once, each of which is a
prerequisite for horizontal scaling:

1. **It absorbs bursts.** Arrivals land in the queue instantly; workers drain it at
   their own rate. The spike becomes depth, not dropped work or a throttle.
2. **It decouples arrival from execution.** The producer (EventBridge, driven by a
   webhook or a workflow) no longer waits on a consumer being free. This is the
   literal meaning of "separate long-running AI workloads from lightweight
   orchestration" — the orchestration publishes an event and returns; the heavy work
   is picked up asynchronously.
3. **It enables share-nothing fan-out.** SQS delivers each message to exactly one
   receiver, so *N* workers polling one queue share the load with no leader, no
   partitioning, and no coordination. Adding a worker is adding a poller. That is
   what makes the future fleet a drop-in rather than a redesign.

The **dead-letter queue** is the reliability half of the same idea: horizontal
scaling multiplies the chances that *some* task is a poison payload, and without a
DLQ one bad message received-and-failed forever would cycle the queue and starve the
healthy work behind it. After `MaxReceiveCount` honest retries the task is set aside,
alarmed on, and the line keeps moving.

And the **queue-depth alarm is the scaling signal**. This is the subtle, important
part of a *foundation*: it is not enough to make scaling possible; the thing that
would scale has to be able to *see when to*. `ApproximateNumberOfMessagesVisible`
rising is precisely the metric an EC2 Auto Scaling target-tracking policy, or a
Lambda concurrency control, or a human on-call would act on. This milestone wires
that metric to a human today; the contract does not change when a policy replaces
the human tomorrow.

## Component-by-component scaling

### EC2 — vertical now, horizontal next

Today the platform is one instance, provisioned through a **launch template**
(Milestone 3) specifically so a fleet needs no re-architecture. Scaling paths:

- **Vertical** — a bigger instance (or a GPU instance for real-time inference on
  larger models) when a single stream of work needs more headroom. Simple, bounded,
  and eventually expensive.
- **Horizontal** — an Auto Scaling group across instance types and AZs, target-
  tracking on the queue-depth metric this milestone publishes. The launch template
  is the interface; the ASG is the deferred piece. Capacity-optimised allocation
  across a fleet is also what unlocks Spot's best pricing and interruption
  resilience (Milestone 3 noted this deferral explicitly).

### AWS Lambda — already horizontal, kept that way

Lambda scales horizontally by default: AWS runs as many concurrent executions as
demand requires, each isolated. The platform's job is only to keep its functions
*scalable-shaped*, and they are:

- **Stateless.** Each invocation carries its own context (the event); nothing is
  held between invocations. Two concurrent executions never share memory.
- **Short-running.** The dispatch, webhook, scheduler, and Spot handlers are
  seconds-scale event handlers, not workers. Long-running AI work is exactly what
  this milestone moves *off* the synchronous Lambda path and onto the queue — so
  Lambda stays lightweight orchestration, never the thing holding a 15-minute task.
- **Independently scalable.** Each function scales on its own trigger; the webhook
  handler's load does not affect the scheduler's. Reserved/provisioned concurrency
  is available per function if one ever needs a floor or a ceiling.
- **Appropriately timed out.** The dispatch function's timeout is 15s — an event
  handler's budget, not a worker's — which is the configuration that keeps it honest
  about what it is for.

### OpenClaw — the integration is ready for many workers

The OpenClaw *integration* (Milestone 6) is submit / track / retrieve / cancel with
mandatory budgets and untrusted-output validation. Each agent task is **independent**
— no shared state between runs — which is the property that makes horizontal scaling
trivial in principle: many workers, each pulling one task from the queue, executing
it under its own budget, writing results to S3. This milestone lays the foundation
(the queue every future worker reads from, the DLQ for the task none can finish, the
depth signal that says "add a worker"). It does **not** deploy multiple workers — the
OpenClaw runtime is deployed by the separate [`openclaw-on-aws`](README.md#related-repositories) repository, and a
multi-worker deployment is future work. What exists today is the seam they plug into.

### Ollama — a single-node ceiling, honestly stated

Ollama runs on the one instance and inherits its limits: it is a **single node**,
CPU-bound in the current deployment (the platform stays off GPU for cost and quota
reasons), and its throughput is one box's throughput. It does not scale horizontally
on its own — there is no built-in clustering — and this milestone **does not** attempt
distributed inference. The honest future strategies, documented not built:

- **Replicate behind a load balancer.** Multiple instances each running Ollama with
  the same models baked into the AMI, fronted by a target group. Stateless request
  serving fans out cleanly; the constraint is model distribution.
- **Model distribution.** Each node needs the model weights. Baking them into the
  custom AMI (Milestone 4) is the pragmatic answer — the image *is* the distribution
  mechanism — at the cost of a larger image and a rebuild to change models.
- **GPU node pools.** Real-time inference on larger models eventually wants GPU
  instances; those are expensive enough that Bedrock's zero-idle-cost elasticity
  often wins for spiky demand (below).

The single-node ceiling is not a flaw to hide; it is the reason the router exists.

### Amazon Bedrock — scaling is not your problem

Bedrock is a managed, elastic foundation-model service: it scales horizontally
behind the API with no fleet, no nodes, no model distribution, and no capacity
planning on the platform's part. Concurrency is governed by account-level quotas
(raisable), not by anything the platform runs. For scale specifically, this is its
entire value proposition — the platform offloads the hardest scaling problem it has
(serving a large model under bursty load) to AWS, and pays per token for the
privilege. See the trade-off below.

### n8n — concurrent by configuration, not by default

n8n orchestrates workflows on the instance. For scale the questions are about
concurrency and serialisation:

- **Concurrent executions.** n8n can run workflows concurrently; the platform's
  workflows should be written to *tolerate* concurrency — idempotent steps, no
  reliance on a global in-memory singleton, external state (S3) rather than
  execution-local state.
- **Avoid unnecessary serialisation.** A workflow that holds a lock, or funnels
  every run through one synchronous external call, serialises by construction.
  Publishing an event to the bus (and letting the queue absorb it) instead of
  calling a worker synchronously is how a workflow *stays* concurrent.
- **Future distributed execution.** n8n supports a queue-mode with separate worker
  processes for horizontal execution. The platform's move to an event/queue seam is
  the same shape at the infrastructure layer, so the two compose: workflows publish,
  the queue levels, workers scale.

## Ollama vs. Amazon Bedrock as a scaling decision

The router (Milestone 10) already chooses between local and managed inference. Seen
through a *scaling* lens rather than a cost one, the two are opposite answers to
"who runs the fleet":

|  | **Ollama (local)** | **Amazon Bedrock (managed)** |
| --- | --- | --- |
| **Who scales it** | You do — instances, model distribution, load balancing. | AWS does — elastic behind the API. |
| **Scaling limit** | The fleet you provision; a single-node ceiling until you build the fleet. | Account quotas (raisable); effectively elastic. |
| **Scale-out effort** | High: new nodes, models on each, a balancer, capacity planning. | Zero: it is already horizontal. |
| **Best for** | Steady, predictable volume you can size a fleet for; data that must not leave the network. | Spiky or unpredictable load, and models too large to self-host, where you want someone else to own the fleet. |
| **The trade** | Control and marginal-cost-≈-zero, in exchange for owning the scaling problem. | Elasticity and zero ops, in exchange for per-token cost and data leaving the box. |

The architectural point: **managed inference scales the way local inference cannot
without a fleet you have to build.** The router already encodes the policy — prefer
local when a suitable model exists and the instance is up, fall back to Bedrock
otherwise — and under load that same policy doubles as a scaling valve: when local
capacity is saturated or absent, Bedrock absorbs the overflow elastically. The
provider abstraction is what makes that valve possible.

## The provider abstraction is a scaling primitive

The `llm.Provider` interface (Milestones 7–10) was built for substitution — swap
Ollama for Bedrock without changing a caller. Reviewed for scale, it turns out to be
a scaling primitive for the same reason it is a substitution one:

- **Multiple providers behind one interface** means capacity can be *added* by
  adding a provider (a second Bedrock region, a GPU-node pool, another managed
  service) without touching a single call site.
- **Future providers** drop in behind the interface — the platform's scaling
  options are open-ended, not welded to today's two.
- **Independent scaling** — the router (which *is* a `Provider`) can send different
  request classes to different backends, so the local plane and the managed plane
  scale on their own curves rather than as one.
- **Routing flexibility** — health-aware fallback and the `RequireLocal` constraint
  mean the router can shed load to managed inference when local saturates, and
  refuse to for the requests that must stay local. That is a load-shedding policy
  expressed as routing.

An abstraction built to make providers *interchangeable* is, for free, the thing
that makes provider capacity *additive*. See [ROUTING.md](ROUTING.md).

## Infrastructure scaling strategy

Three kinds of scaling, mapped to the planes that use each:

| Kind | What it means | Where the platform uses it |
| --- | --- | --- |
| **Horizontal** | More instances of the same thing, sharing load | Future OpenClaw worker fleet and EC2 ASG (both drop-in on this milestone's queue); Lambda (already, by AWS); Bedrock (already, by AWS) |
| **Vertical** | A bigger instance | EC2 today — a larger or GPU instance when a single stream needs headroom; the simple first move before a fleet is justified |
| **Event-driven** | Scale reacts to a signal, not a schedule | The queue-depth/age metrics this milestone publishes are the signal; today a human acts on them, tomorrow an ASG target-tracking policy or Lambda concurrency control does |

The sequencing is deliberate: **event-driven scaling is built first (the signal),
horizontal scaling second (the fleet that reacts to it), because a fleet with no
signal scales blind.** This milestone builds the signal. Auto Scaling groups are
**not** implemented here — the current single-deployment architecture does not
require one, and building an ASG before the worker that would fill it is building the
reaction before the thing it reacts to.

## What stays central, on purpose

Not everything should scale out, and saying which parts must not is as much a part of
a scaling design as saying which parts should:

- **The event bus** — one front door, regional and already elastic. Sharding buys
  nothing and adds coordination.
- **Lifecycle authority** — the scheduler and Spot/state-change handlers act on the
  fleet; there must be exactly one authority deciding when instances start and stop,
  or two schedulers fight.
- **Durable state** — S3 is the one store all workers share. Per-worker state would
  have to be reconciled, which is a distributed-systems problem the platform has no
  reason to take on.

Centralising these is what *lets* the rest scale cleanly: share-nothing workers are
only simple because the state and the coordination live in exactly one place each.

## Deploy it

The scalability stack is pure CloudFormation — no Go to build. It imports the event
bus from `05-events`, so deploy that first (it is part of the core `make deploy`).

```sh
# Deploy the work queue, DLQ, event rule, and scaling alarms.
# If the monitoring stack (10-monitoring) is deployed, the alarms auto-wire to its
# SNS topic; if not, they are created visible-but-silent.
make -C infra scalability

# Publish a task event and watch it land in the queue (detail-type must match
# TaskDetailType, default AgentTaskRequested):
aws events put-events --entries '[{
  "EventBusName":"aiap-dev-bus",
  "Source":"aiap.dev.platform",
  "DetailType":"AgentTaskRequested",
  "Detail":"{\"task\":\"demo\"}"
}]'

# Then read it off the queue to confirm the seam end-to-end:
url=$(aws cloudformation describe-stacks --stack-name aiap-dev-13-scalability \
  --query "Stacks[0].Outputs[?OutputKey=='AgentTaskQueueUrl'].OutputValue" --output text)
aws sqs receive-message --queue-url "$url"
```

Tune the queue with the stack parameters: `VisibilityTimeoutSeconds` (the longest a
single task may take — never below a worker's true maximum), `MaxReceiveCount`
(retries before the DLQ), and the alarm thresholds (`BacklogAlarmThreshold`,
`OldestMessageAgeAlarmSeconds`).

## Well-Architected

Mapped to the two pillars this milestone serves — Performance Efficiency and
Reliability.

| Principle | Pillar | How the platform honours it |
| --- | --- | --- |
| **Use serverless architectures** | Performance Efficiency | The scaling seam is SQS + EventBridge + Lambda — managed, elastic, no fleet to run for the buffering layer itself. |
| **Decouple to scale independently** | Performance Efficiency | The queue separates arrival from execution, so orchestration and long-running work scale on their own curves rather than as one. |
| **Consume only what you need** | Performance Efficiency | Workers pull at their own rate; a burst becomes queue depth, not a wall of concurrent executions or a throttle. |
| **Scale horizontally to increase availability** | Reliability | Share-nothing workers on one queue mean losing a worker loses no work (it returns to the queue) and adding one is adding a poller. |
| **Stop guessing capacity** | Reliability | Queue depth and message age are the measured demand signal, replacing a guess about how many workers are "enough". |
| **Manage failure through a DLQ** | Reliability | A poison task is isolated after bounded retries and alarmed on, instead of blocking the line or being lost. |
| **Automatically recover from failure** | Reliability | An in-flight task on a crashed worker becomes visible again after the visibility timeout and is retried by another — no manual re-queue. |

## Explicitly deferred

Stated plainly so the boundary of this **partial implementation** is unambiguous:

- **Auto Scaling groups** — not built. The launch template (Milestone 3) is the
  interface; the ASG that target-tracks on the queue-depth metric is a later
  milestone. The current single-deployment architecture does not require one.
- **A multi-worker OpenClaw deployment** — not built. The queue, DLQ, and signal
  they would consume exist; the workers themselves are future work.
- **Distributed / replicated Ollama inference** — not built, by explicit scope. The
  single-node ceiling is documented, not removed.
- **n8n queue-mode workers** — not built. Workflows are guided toward
  concurrency-tolerance; the separate worker processes are future work.
- **A consumer for the queue** — this milestone produces to the queue (via the event
  rule) and signals on it; a worker that *consumes* it is the next milestone's job.
  The queue standing ready with nothing draining it is the foundation, not a bug.

## Future improvements

- **The worker fleet.** An EC2 Auto Scaling group (or a container service) whose
  workers poll `AgentTaskQueue`, scaled by a target-tracking policy on the
  backlog-per-instance metric this milestone publishes. The single biggest step
  from foundation to horizontal scale.
- **Inference-triggered lifecycle.** Start the instance when the queue goes non-empty
  and stop it after an idle timeout — the scheduler (Milestone 15's "what comes
  next") driven by queue depth instead of a clock, so the fleet is up only while
  there is work.
- **FIFO where ordering matters.** The work queue is standard (unordered) because
  agent tasks are independent. A workflow that genuinely needs ordered, exactly-once
  processing would use a FIFO queue with a message group per ordering key.
- **Backlog-aware routing.** Feed queue depth back into the router so that when local
  capacity is saturated the overflow is shed to Bedrock automatically — load
  shedding as a routing policy.
- **Multi-instance and multi-region.** Once the fleet exists, the same share-nothing
  shape extends to more than one AZ and, eventually, more than one region, with the
  bus and durable state as the seams that stay singular.
