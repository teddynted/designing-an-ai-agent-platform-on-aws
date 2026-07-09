# Blog: *Designing an AI Agent Platform on AWS*

The architecture documentation in `docs/architecture/` and `docs/adr/` is the **single source of truth**. This file is the **narrative spine** for the technical article — the argument, its order, and where each section's material already lives.

Deliberately *not* a draft of the article. A second prose copy of the same content would drift from the docs within a milestone, and then nobody would know which one was true. Write the article from this spine, pulling material from the linked sources.

---

## The thesis

> **An AI agent platform is not one workload. It is three workloads with opposing operational characteristics, and most of the interesting engineering is in refusing to conflate them.**

Everything else — the Spot strategy, the Model Gateway, the EFS choice, the security model — is downstream of that sentence.

## Why this article is worth writing

Most "AI agents on AWS" content is a wiring diagram: here is Bedrock, here is a Lambda, here is an agent. It skips the parts that actually bite:

- What happens when your agent runtime holds a **WhatsApp device-link** and AWS reclaims the Spot instance underneath it?
- What happens when the fleet with the **worst cold start** turns out to be the one fleet AWS's warm pools **cannot** accelerate?
- What does "secure" mean when the thing you are securing takes instructions from **text it read on the internet** and has a shell?

These are the load-bearing problems. The article's value is that it answers them with trade-offs rather than a diagram.

## Narrative arc

### 1. Open with the contradiction
The brief demands Spot instances *and* production-ready autonomous agents. Those requirements appear to be in direct conflict: Spot reclaims your instance with two minutes' notice; an agent runtime holds chat sessions whose loss requires **a human, holding a phone, scanning a QR code**.

Do not resolve this yet. Let it sit.

*Source: [01 §1.2](../architecture/01-overview.md), [ADR-0005](../adr/0005-spot-only-for-stateless-workloads.md)*

### 2. Resolve it by decomposing
The conflict comes from the wrong decomposition. Split by **statefulness and interruption tolerance**, and it dissolves: control, agent, inference. Spot belongs where interruption is free; state belongs where it survives.

Show the three-plane table. This is the article's central image.

*Source: [ADR-0002](../adr/0002-three-plane-decomposition.md), [02 §2.5](../architecture/02-components.md)*

### 3. The provider seam — and its second-order payoff
Everyone will tell you to abstract your LLM provider "for flexibility." That is the boring reason and it is not why the seam is here.

The real reason: **Bedrock becomes Ollama's availability backstop.** Spot GPU capacity genuinely disappears. Without a shared fallback, running inference on Spot trades availability for cost — a bad trade. With the seam, a capacity shortfall degrades the *bill*, not the *service*.

The abstraction is what makes the cheap path safe. That is a more interesting claim than "abstraction is good."

*Source: [ADR-0003](../adr/0003-model-gateway-seam.md), [ADR-0004](../adr/0004-inference-routing-policy.md)*

### 4. The constraint that broke the obvious plan
Set it up: the brief says minimise EC2 startup time. AWS's canonical answer is ASG warm pools.

Then the reversal: **warm pools do not support Spot Instances.** The fleet with the worst cold start — GPU inference downloading tens of gigabytes of weights — is precisely the fleet that cannot use them.

This is the article's best "I checked, and the obvious answer was wrong" moment. Show the layered response: bake weights into golden AMIs, pre-stage snapshots, then **architect around the residual** — SQS absorbs the cold start, Bedrock serves anything interactive. The cold start is made *invisible* rather than *small*.

Include the Fast Snapshot Restore cost trap (~$540/month per snapshot per AZ, billed continuously). It is a cold-start accelerator with a standing cost, and it is how you build a bill nobody can explain.

*Source: [ADR-0006](../adr/0006-startup-time-strategy.md), [06 §6.4](../architecture/06-deployment.md), [09 §9.6](../architecture/09-cost.md)*

### 5. The singleton you cannot wish away
OpenClaw's Gateway is the single source of truth for sessions, routing, and channel connections. Chat channel pairings are **device registrations**, not tokens. Two Gateways sharing one is undefined behaviour, not load balancing.

So: no active-active. HA becomes fast recovery. And because EBS volumes are AZ-bound while this state is *unrecoverable by automation*, the state goes on **EFS** — paying latency and cost to buy cross-AZ relaunch.

Then the honest part, which is the point of the section: **publish 99.5%, not 99.9%.** A single Gateway with a 3–5 minute relaunch cannot support three nines. Publishing 99.9% would be a fiction that survives exactly until the first incident.

*Source: [ADR-0009](../adr/0009-openclaw-gateway-singleton.md), [07 §7.4](../architecture/07-scalability-and-ha.md), [10 §10.4](../architecture/10-operations.md)*

### 6. Security: the reframe
The strongest section. Lead with OpenClaw's own words: *"the agent can do anything you can do."*

Then dismantle the instinctive defence. Prompt injection is not a content-filtering problem:

- Untrusted text arrives via fetched pages, emails, issues, PDFs, tool output — not just the user's message.
- The model cannot reliably separate content-to-reason-about from instructions-to-follow. That is not a bug a better model fixes; it is what an instruction-following model *is*.
- Filters have false-negative rates. Shells do not have partial-credit failure modes.

Land the reframe: **it is a confused-deputy problem — privilege escalation wearing a natural-language costume.** So assume the model *will* be fully persuaded, and ask what happens next.

Then the punchline control: **block IMDS from the sandbox.** Without it, one `curl` to `169.254.169.254` retrieves the host's IAM credentials and every other boundary is theatre.

Finish with the two honest admissions: the Docker socket the Gateway holds is equivalent to root on the host, and human approval is a *social* control that decays into a rubber stamp under fatigue.

*Source: [ADR-0010](../adr/0010-agent-sandbox-containment.md), [08](../architecture/08-security.md), [05 §5.5](../architecture/05-network-and-boundaries.md)*

### 7. The cost section nobody writes
Two claims that reframe the whole topic:

**Infrastructure is a rounding error.** ~$720/month of infrastructure against token spend that ranges from $50 to $5,000+. Optimising a $36 ALB while an agent burns $3,000 in tokens is misallocated effort.

**The cost alarm is also the security alarm.** A runaway agent and a compromised agent produce the same signal — a spend spike tied to an identity. Once budgets are per-agent, cost anomaly detection *is* compromise detection. That is a genuinely useful property and not one you get in ordinary systems.

Then the control that follows: a **budget circuit-breaker** is a production requirement, not a nice-to-have. An agent retrying a failing tool call, resending a growing context to a frontier model, can spend thousands overnight with nobody logged in. This is the platform's most likely expensive incident — more likely than a breach.

Close on the metric: steer by **cost per successful outcome**, not cost per token. An agent that costs 3× per run but succeeds twice as often on the first attempt is cheaper.

*Source: [09](../architecture/09-cost.md), [10 §10.2](../architecture/10-operations.md)*

### 8. Close on what the design admits
Do not end on a triumphant diagram. End on the three admissions from the README:

1. Conversational availability is 99.5%, and here is exactly why.
2. Prompt injection is contained, not solved.
3. The two controls that matter most — the circuit-breaker and the sandbox — **do not exist yet**, and the platform must not touch production credentials until they do.

The last one is the strongest ending available: a design milestone that names its own preconditions for being safe to use.

*Source: [12](../architecture/12-risks-assumptions-constraints.md), [README](../../README.md)*

---

## Assets ready to lift

| Asset | Source |
|---|---|
| Three-plane comparison table | [01 §1.2](../architecture/01-overview.md) |
| High-level architecture diagram (Mermaid) | [01 §1.3](../architecture/01-overview.md), [README](../../README.md) |
| Statefulness → compute placement table | [02 §2.5](../architecture/02-components.md) |
| Sequence diagrams (5) | [04](../architecture/04-flows.md) |
| Prompt-injection attack tree | [08 §8.1](../architecture/08-security.md) |
| Trust-zone diagram | [05 §5.6](../architecture/05-network-and-boundaries.md) |
| Cold-start layer table + warm-pool constraint | [06 §6.4](../architecture/06-deployment.md) |
| Cost estimate tables + cost-trap table | [09 §9.3, §9.6](../architecture/09-cost.md) |
| Failure-mode matrix | [07 §7.5](../architecture/07-scalability-and-ha.md) |
| Risk register | [12 §12.3](../architecture/12-risks-assumptions-constraints.md) |

## Editorial notes

- **Every claim in the article must trace to a doc.** If the article makes a claim the docs do not, fix the docs first.
- **Numbers are illustrative.** Cost figures are order-of-magnitude for `us-east-1` and are labelled as such in [09](../architecture/09-cost.md). Bedrock batch/prompt-caching discounts and n8n queue-mode internals were **assumed, not verified**, during Milestone 1. Verify before publishing — an article is a stronger claim than an internal doc.
- **RTO/RPO figures are designed, not measured.** Milestone 5 is where they become facts. Say so in the article rather than implying they were tested.
- **The audience is an engineer who has deployed things on AWS and is sceptical of AI-platform hype.** Trade-offs over enthusiasm; admissions over completeness.
- **Suggested length:** 3,000–4,500 words. Sections 4, 6, and 7 are the differentiated content — give them room and compress sections 1–2.
