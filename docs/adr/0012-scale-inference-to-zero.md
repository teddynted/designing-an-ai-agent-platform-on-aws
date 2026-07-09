# ADR-0012: Scale the self-hosted inference fleet to zero

**Status:** Accepted
**Date:** 2026-07-09

## Context

An agent platform spends most of its hours waiting for a message. Load is bursty and often diurnal. A GPU instance costs the same whether it serves a billion tokens or none: a `g5.xlarge` on Spot runs roughly $250/month if left on, and about triple that On-Demand.

Meanwhile, Bedrock costs **zero at idle**.

Keeping a GPU warm to avoid a cold start is therefore paying ~$250/month to remove 2–4 minutes of latency from a workload class ([bulk inference](0004-inference-routing-policy.md)) that by definition tolerates latency. That is a bad trade, and it is the default one.

The obstacle is cold start. A scaled-to-zero fleet takes 2–4 minutes to serve its first token even with baked AMIs, and **warm pools cannot help because they do not support Spot** ([ADR-0006](0006-startup-time-strategy.md)).

## Decision

**Run the Ollama Spot GPU ASG at `desired = 0` when idle.** Scale up on demand; scale back to zero after an idle period.

The mechanism ([04 §4.4](../architecture/04-flows.md)):

1. Bulk inference requests are enqueued to **SQS** by the Model Gateway.
2. A CloudWatch alarm on `ApproximateNumberOfMessagesVisible > 0` triggers `Lambda: scaler`.
3. The scaler sets ASG desired capacity based on queue depth.
4. Instances launch from a golden AMI with weights baked in (~2–4 min) and drain the queue.
5. After N minutes with an empty queue, the scaler sets desired back to **0**.

Three properties make this safe rather than reckless:

- **SQS absorbs the cold start.** The backlog is durable and measurable; no caller waits on a connection while a GPU boots. EventBridge could not do this — it routes, it does not hold a depth you can scale on.
- **Interactive traffic never touches this path.** It goes to Bedrock, which has no cold start ([ADR-0004](0004-inference-routing-policy.md)).
- **Bedrock is the fallback** when Spot GPU capacity is unavailable. A capacity shortfall raises the bill; it does not drop requests.

## Consequences

**Positive**

- **The largest single cost lever in the platform.** Combined with Spot pricing ([ADR-0005](0005-spot-only-for-stateless-workloads.md)), the GPU fleet costs roughly **5–10% of an always-on On-Demand fleet**. The two levers compound.
- Idle cost approaches zero, which matters because idle is the platform's dominant state. A design that is cheap under load and expensive at rest loses.
- Scaling is demand-driven and needs no capacity forecasting.
- Dev environments cost nearly nothing overnight and at weekends.

**Negative**

- **2–4 minute scale-up latency**, unavoidable and un-warm-poolable on Spot. Hidden by SQS buffering and Bedrock routing rather than removed. A workload that is simultaneously latency-sensitive *and* requires self-hosted models has **no good answer** in this architecture — it must choose.
- **Callers must classify requests.** A misclassified interactive request lands behind a cold GPU and times out. The default must be `interactive`, so that forgetting to classify fails safe.
- **Scale-up thrashing** is possible with sporadic single requests: boot a GPU, serve one request, scale down, repeat. Mitigate with a minimum queue depth or minimum-age threshold before scaling up, and a generous idle timeout before scaling down. Tune with real traffic.
- The `scaler` Lambda is a control-plane component whose failure silently means "no bulk inference." Alarm on *queue depth rising while desired capacity is zero* — the signature of a dead scaler.
- **Cold-start regressions are invisible until measured.** If someone rebuilds the AMI without baking weights, the fleet still works; it is just slow, and the bill for GPU-minutes spent downloading weights arrives later. Alarm on scale-up latency ([10 §10.2](../architecture/10-operations.md)).

## Alternatives considered

**Keep one GPU always warm (`min = 1`).** Removes cold start for the first concurrent request only, at ~$250/mo. Rejected as the default: it forfeits the platform's largest cost lever to solve a problem that SQS buffering and Bedrock routing already solve. Reasonable as a **dial** to pull if bulk-inference latency SLOs tighten — that is a cost/latency decision, not a redesign.

**Warm pool with `min = 0` running instances.** **Not available on Spot.** Would work with an On-Demand GPU fleet at 3–5× instance cost — trading the largest cost saving for two minutes of latency. Rejected, and noted as the escape hatch if scale-up latency ever becomes intolerable.

**Bedrock only; no self-hosted fleet at all.** Zero idle cost, zero cold start, zero Spot risk, zero GPU operations. Genuinely the simplest architecture and, **if bulk volume and data-residency requirements never materialise, the correct one.** Rejected because the brief requires self-hosted models — but the Model Gateway seam ([ADR-0003](0003-model-gateway-seam.md)) makes removing Ollama a config change. Instrument the routing split and revisit honestly: if Ollama's utilisation stays low, its cost case never existed.

**Scale on a schedule (business hours) rather than on queue depth.** Simpler, and no scale-up latency during working hours. Rejected: pays for idle GPUs through every quiet hour of the working day, and serves nothing outside it. Demand-driven scaling strictly dominates unless demand is genuinely known in advance — and agent traffic is not.

**Predictive scaling.** Requires a load history the platform does not yet have. Revisit once there is one.
