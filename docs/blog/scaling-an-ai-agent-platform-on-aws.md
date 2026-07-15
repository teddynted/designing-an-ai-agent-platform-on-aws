# Scaling an AI Agent Platform on AWS

> **Milestone 16 — Scalability.**
> This milestone does the one thing you must do before you can scale a platform
> horizontally, and stops there on purpose. It adds a durable **work queue** between
> the event bus and the workers that run long-running AI tasks
> ([`13-scalability.yaml`](../../infra/cloudformation/13-scalability.yaml)) — the seam
> that decouples arrival from execution — plus the **queue-depth signal** a future
> worker fleet scales on, a dead-letter queue for the tasks no worker can finish, and
> the reference ([SCALABILITY.md](../../SCALABILITY.md)) and diagrams. It does **not**
> build the fleet: no Auto Scaling group, no multiple OpenClaw workers, no distributed
> Ollama. Those are drop-in on top of the queue, and drop-in is a later milestone.

*Audience: engineers who have watched a single-instance service fall over under a
burst, reached for "just add an Auto Scaling group," and discovered the hard way that
you cannot autoscale a thing you never decoupled — the group just gives you more
instances fighting over the same synchronous bottleneck.*

---

## Contents

- [Scaling is a property you build in, not a button you press](#scaling-is-a-property-you-build-in-not-a-button-you-press)
- [The honest architecture review](#the-honest-architecture-review)
- [The bottleneck was the synchronous path](#the-bottleneck-was-the-synchronous-path)
- [A queue does three jobs, and all three are prerequisites](#a-queue-does-three-jobs-and-all-three-are-prerequisites)
- [Build the signal before the fleet](#build-the-signal-before-the-fleet)
- [Scaling each plane: what AWS does for you, and what it doesn't](#scaling-each-plane-what-aws-does-for-you-and-what-it-doesnt)
- [Local vs. managed inference is a "who runs the fleet" question](#local-vs-managed-inference-is-a-who-runs-the-fleet-question)
- [The abstraction built for substitution turned out to be a scaling primitive](#the-abstraction-built-for-substitution-turned-out-to-be-a-scaling-primitive)
- [What I deliberately did not build](#what-i-deliberately-did-not-build)
- [Lessons learned](#lessons-learned)
- [What comes next](#what-comes-next)

## Scaling is a property you build in, not a button you press

The most expensive misconception about scaling is that it is something you add later
— that you build the thing, and when it gets popular you turn on Auto Scaling. It
does not work, and the reason is worth stating plainly because it dictates this
entire milestone: **an Auto Scaling group is a machine for making more copies of a
component, and more copies of a component that is coupled to a synchronous
bottleneck is more things queuing for the same bottleneck.** You do not get
throughput; you get contention with a bigger bill.

Scalability is a *shape*. A system is scalable when its parts can grow independently,
which they can only do if they are decoupled, which they only are if arrival is
separated from execution. So the work of "making it scalable" is almost never adding
capacity. It is inserting the seams that let capacity be added later without a
rewrite. This milestone inserts exactly one seam and proves it works. That is the
whole job, and doing only that — rather than reflexively bolting on an ASG — is the
point.

## The honest architecture review

Before adding anything I reviewed what the platform actually is after fifteen
milestones, specifically for where it scales and where it doesn't. The platform is a
**single EC2 instance** running Ollama, n8n, and the OpenClaw runtime, fronted by an
EventBridge bus and a handful of Lambda functions, with Amazon Bedrock as a managed
inference fallback. Read for scale:

**The bottleneck** was the event path: EventBridge invoked a dispatch Lambda that
did or fronted the work, *synchronously*. A burst of arrivals had nowhere to wait —
it fanned straight into concurrent executions or hit a throttle. There was no buffer
and no backpressure.

**The single point of failure** is the instance. Ollama, n8n, and OpenClaw all live
on it; lose it and local inference, orchestration, and agent execution stop together.
(Spot interruption handling from Milestone 3 makes the loss *graceful* — work drains
to S3, a replacement boots in 2.5 seconds — but a replacement is still one instance.)

**The resource contention** is those same three workloads sharing one box's CPU and
memory. Fine for a single stream of work; wrong for concurrent streams. This is the
tell that they need to scale *independently*, not that the box needs to be bigger
forever.

**What is already fine:** EventBridge, Lambda, SQS, Bedrock, and S3 are managed,
regional, multi-AZ, and elastic. They are not the problem and do not become one under
load. The problem is entirely the single, shared, synchronously-fed instance — and
the fix is not to make it bigger but to put a seam in front of it.

## The bottleneck was the synchronous path

Here is the failure mode concretely. A webhook fires, or a workflow completes, and
publishes an event. EventBridge invokes the dispatch function, which needs to hand
the event to a worker that runs a long agent task. If the worker is busy, the caller
waits. If ten events arrive at once, ten executions start at once — or the eleventh
is throttled. Either way the producer's fate is tied to the consumer's availability,
and a spike is an incident.

The thing that removes this is boring and old and exactly right: put a queue between
them. Not because queues are fashionable, but because a queue is the only structure
that lets a fast, bursty producer and a slow, finite consumer coexist without one
dictating the other's pace.

## A queue does three jobs, and all three are prerequisites

I added one SQS queue, `AgentTaskQueue`, and an EventBridge rule that selects
long-running task events by `detail-type` and routes them to it instead of to the
synchronous Lambda. Lightweight orchestration events keep their fast path; heavy work
goes to the queue. That one queue does three things, and every one of them is a
precondition for horizontal scaling:

**It absorbs bursts.** Arrivals land instantly; workers drain at their own rate. A
spike becomes queue *depth* — a number that goes up and comes back down — instead of
dropped work or a throttle. The system degrades gracefully under load rather than
failing at a cliff.

**It decouples arrival from execution.** The producer publishes and returns; it never
waits on a consumer being free. This is the literal meaning of the milestone's brief
— *separate long-running AI workloads from lightweight orchestration.* The
orchestration is now genuinely lightweight because it has handed the heavy work to a
buffer and walked away.

**It enables share-nothing fan-out.** SQS delivers each message to exactly one
receiver. So *N* workers polling one queue split the load with no leader, no
partitioning, no coordination — adding a worker is adding a poller. This is the
property that makes the future fleet a drop-in rather than a distributed-systems
project. The workers never talk to each other; they talk to the queue.

Two supporting pieces make it safe rather than just fast. A **dead-letter queue**:
horizontal scaling multiplies the chance that some task is a poison payload, and a
poison message received-and-failed forever would cycle the queue and starve the
healthy work behind it. After `MaxReceiveCount` honest retries the task is set aside
and alarmed on, and the line keeps moving. And a **visibility timeout** sized to the
longest a task can take, so a task handed to a worker that then crashes becomes
visible again and is retried by another — automatic recovery, no manual re-queue.

```yaml
# The redrive policy: the one line that turns "a poison task blocks everything"
# into "a poison task is set aside after N tries."
RedrivePolicy:
  deadLetterTargetArn: !GetAtt AgentTaskDeadLetterQueue.Arn
  maxReceiveCount: !Ref MaxReceiveCount
```

## Build the signal before the fleet

This is the part that separates a foundation from a gesture. It is not enough to make
scaling *possible* — the thing that would scale has to be able to see *when to*. So
alongside the queue I published the signal: CloudWatch alarms on
`ApproximateNumberOfMessagesVisible` (depth — arrivals outrunning throughput) and
`ApproximateAgeOfOldestMessage` (latency — the front of the line waiting too long,
which catches a fleet that has stalled entirely even when depth looks modest).

The important property of these alarms is that **they are the exact metric a future
autoscaler consumes.** An EC2 Auto Scaling target-tracking policy scales on queue
depth. A human on-call scales on queue depth. The contract does not change when a
policy replaces the human — only the subscriber does. So wiring depth to a human
today is not a stopgap that gets thrown away; it is the same interface the automation
plugs into, exercised early. Build the signal first, the fleet second, because a
fleet with no signal scales blind — and this milestone is the signal.

I resisted building the Auto Scaling group. The launch template has existed since
Milestone 3 specifically so an ASG is a drop-in, and it was tempting to close the
loop. But an ASG with no worker to run is a reaction with nothing to react with, and
the current single deployment does not need one. Building it now would be building
the reaction before the thing it reacts to — the same mistake as reaching for
autoscaling before decoupling, one layer up.

## Scaling each plane: what AWS does for you, and what it doesn't

A scaling review is per-plane, because the planes scale by completely different
mechanisms:

- **Lambda** already scales horizontally — AWS runs as many isolated concurrent
  executions as demand needs. My only job is to keep the functions *scalable-shaped*:
  stateless, short-running, independently triggered, honestly timed out. Moving
  long-running work onto the queue is what keeps Lambda in its lane as lightweight
  orchestration, never the thing holding a 15-minute task open.
- **OpenClaw** tasks are independent — no shared state between runs — so many
  workers, each pulling one task, is trivial *in principle*. The queue, DLQ, and
  depth signal they'd consume now exist. The workers themselves are future work; what
  shipped is the seam they plug into.
- **Ollama** is a single node with a single node's ceiling, CPU-bound in this
  deployment. It does not scale horizontally on its own and I did **not** attempt
  distributed inference. The honest future path — replicate behind a load balancer,
  distribute models by baking them into the custom AMI, add GPU node pools — is
  documented, not built. The single-node ceiling is the reason the router exists.
- **Bedrock** scales behind the API with no fleet, no nodes, no model distribution,
  no capacity planning. For scaling specifically, that *is* its value: it offloads the
  hardest scaling problem the platform has to AWS, priced per token.
- **n8n** runs concurrent executions if the workflows are written to tolerate
  concurrency — idempotent steps, external (S3) state, publishing events instead of
  synchronous calls. Its queue-mode workers are the same shape as this milestone's
  seam one layer up, so the two compose.

## Local vs. managed inference is a "who runs the fleet" question

The router from Milestone 10 already chooses between local Ollama and managed
Bedrock. Through a cost lens (Milestone 15) they are two cost structures; through a
scaling lens they are opposite answers to one question — *who runs the fleet.*

Ollama: **you** do. You provision the instances, distribute the models, put a load
balancer in front, plan the capacity. In exchange you get marginal-cost-near-zero and
data that never leaves the network. Bedrock: **AWS** does. It is already horizontal;
your scale-out effort is zero. In exchange you pay per token and the data leaves the
box.

The architectural payoff is that these compose into a load-shedding valve. The router
already prefers local and falls back to managed; under load that same policy means
that when local capacity saturates, the overflow spills to Bedrock *elastically*,
because Bedrock's fleet is infinite as far as the platform is concerned. Managed
inference scales the way local inference simply cannot without a fleet you have to
build yourself — and the router is what lets the platform use both curves at once.

## The abstraction built for substitution turned out to be a scaling primitive

The nicest surprise of the review: the `llm.Provider` interface, built across
Milestones 7–10 so you could swap Ollama for Bedrock without touching a caller, is a
scaling primitive for exactly the same reason it is a substitution one. If providers
are interchangeable behind an interface, then provider *capacity* is additive behind
that interface — a second region, a GPU pool, another managed service is added by
adding a provider, not by editing call sites. The router that *is* a `Provider` can
send different request classes to different backends, so the local and managed planes
scale on their own curves. An abstraction built to make backends interchangeable
gives you, for free, the ability to make capacity additive. That is worth noticing:
good boundaries pay dividends in dimensions you did not design them for.

## What I deliberately did not build

Stated plainly, because the boundary of a partial implementation is part of its
design:

- **No Auto Scaling group.** The launch template is the interface; the ASG is
  deferred. The single deployment does not require one.
- **No multi-worker OpenClaw deployment.** The queue and signal exist; the workers
  are next.
- **No distributed or replicated Ollama.** The single-node ceiling is documented, not
  removed.
- **No n8n queue-mode workers.** Workflows are guided toward concurrency-tolerance;
  the worker processes are future work.
- **No consumer for the queue.** This milestone *produces* to the queue and *signals*
  on it. A worker that consumes it is the next milestone. A queue standing ready with
  nothing draining it is the foundation, not a bug.

## Lessons learned

- **You cannot autoscale what you have not decoupled.** More copies of a
  synchronously-coupled component is more contention, not more throughput. The queue
  comes before the fleet, always.
- **A queue is three features in one:** burst absorption, producer/consumer
  decoupling, and share-nothing fan-out. Each is a precondition for horizontal scale,
  and you get all three from one resource.
- **Build the signal before the thing that reacts to it.** Queue depth wired to a
  human today is the same interface an autoscaler plugs into tomorrow. A fleet with no
  demand signal scales blind.
- **Know which parts must stay central.** The bus, the lifecycle authority, and the
  durable store are singular on purpose — share-nothing workers are only simple
  because the coordination and state live in exactly one place each.
- **Good abstractions scale in dimensions you didn't design.** The provider interface
  built for substitution turned out to make capacity additive. Boundaries are worth
  more than the problem that motivated them.
- **The discipline is in what you leave unbuilt.** The launch template made an ASG a
  drop-in, and I still didn't build it, because the reaction before the thing it
  reacts to is the same mistake as autoscaling before decoupling.

## What comes next

The natural next milestone is **the worker fleet**: an Auto Scaling group (or a
container service) whose workers poll `AgentTaskQueue`, scaled by a target-tracking
policy on the backlog-per-instance metric this milestone publishes — the drop-in the
whole foundation was built for. Close behind: **inference-triggered lifecycle** (start
the instance when the queue goes non-empty, stop it when idle — the scheduler driven
by depth instead of a clock), **backlog-aware routing** (shed overflow to Bedrock
automatically when local saturates), and eventually **multi-AZ and multi-region**,
where the same share-nothing shape extends outward with the bus and S3 as the seams
that stay singular.

Each of those is a drop-in *because* of the seam this milestone added, which is the
whole argument for adding the seam first and the fleet never-in-this-milestone. The
operational reference — the full component-by-component review, the Well-Architected
mapping, and the deploy walkthrough — is [SCALABILITY.md](../../SCALABILITY.md).
