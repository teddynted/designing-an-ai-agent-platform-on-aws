# ADR-0010: Contain agents by removing privilege, not by filtering prompts

**Status:** Accepted
**Date:** 2026-07-09

## Context

OpenClaw's documentation states its security model without euphemism: *"the agent can do anything you can do."* The agent reads files, runs shell commands, drives a browser, and calls APIs. Its instructions come from a language model whose context contains **untrusted text** — chat messages, fetched web pages, email bodies, GitHub issues, PDFs, tool outputs.

The instinctive defence is to filter: detect malicious instructions in the input and block them. That defence fails, for reasons that are structural rather than incidental:

- **Untrusted text arrives through many channels**, most of them not the user's message. Any content the agent reads enters its context.
- **The model cannot reliably separate "content to reason about" from "instructions to follow."** This is not a defect that a better model repairs; it is what an instruction-following model *is*.
- **Detection is adversarial and unbounded.** Filters have false-negative rates. Shells do not have partial-credit failure modes.

The right frame: prompt injection is a **confused deputy** problem. The agent holds real authority — an IAM role, a shell, network reach — and accepts instructions from parties who should not command that authority. It is privilege escalation wearing a natural-language costume.

## Decision

**Assume the model will be fully persuaded, and ensure that a fully persuaded agent cannot do much damage.**

Design blast radius, not filters. Concretely:

**1. Tool execution runs in a sandbox that holds nothing worth stealing.**

| Control | Purpose |
|---|---|
| **IMDS unreachable** (`169.254.169.254` blocked) **and** IMDSv2 with **hop limit = 1** | The highest-value control in the platform. Without it, one `curl` retrieves the Gateway's IAM credentials and every other boundary is theatre. Two independent enforcement points, because one will eventually be misconfigured |
| **No AWS credentials in the sandbox — ever.** Not env vars, not mounted files | An AWS action must go through a narrow, audited tool on the Gateway, never `aws` in a shell |
| **Deny-by-default egress allowlist** | Data that cannot leave has not leaked |
| **No VPC CIDR reachability** | No lateral movement to RDS, EFS, ElastiCache |
| Read-only root filesystem; writable `/workspace` only | Limits persistence |
| `no-new-privileges`, dropped capabilities, non-root, seccomp | Raises the cost of container escape |
| CPU / memory / PID limits, wall-clock timeout | An agent loop is a resource-exhaustion vector |
| Ephemeral, destroyed after session | No cross-session contamination |

**2. Tools are an allowlist with risk tiers, and the risky ones need a human.**

| Tier | Examples | Control |
|---|---|---|
| Read-only | read file, allowlisted HTTP GET | Auto-approved |
| Write, reversible | write `/workspace`, post to a dev channel | Auto-approved, logged |
| Write, irreversible or outward-facing | send email, post publicly, open a PR | **Human approval** |
| Privileged | any mutating AWS API, credential access, spend | **Human approval + audited tool + CloudTrail alarm** |

**3. Treat model output as untrusted input.** A tool call emitted by the model is an instruction from an untrusted source and is checked against policy before execution — never executed because the model "decided" it.

**4. Constrain the Gateway's own authority.** IAM scoped to specific resource ARNs (`bedrock:InvokeModel` on named models, never `bedrock:*`). Channel allowlists. Per-agent budget circuit-breaker.

**5. Make everything attributable.** `agent_run_id` on every log line, tool call, sandbox container, and token metric.

## Consequences

**Positive**

- **The sandbox attack paths are closed by architecture, not by vigilance.** A compromised sandbox has no credentials to steal, no network to exfiltrate through, and no VPC to pivot into. It does not matter how convincing the injection was.
- Blast radius is bounded by the *auto-approved tool set*, which is small and reviewable, rather than by the model's judgement, which is not.
- Human-in-the-loop on irreversible actions is the one control that survives a fully persuaded model.
- Per-agent budgets serve double duty: a runaway agent and a compromised agent produce the same signal, so **the cost alarm is also the earliest security alarm** ([09 §9.5](../architecture/09-cost.md)).
- Attribution makes post-incident "what did it touch" answerable in minutes.

**Negative**

- **Prompt injection is contained, not solved.** A fully persuaded agent can still use every auto-approved tool. The mitigation is to keep that set small and boring — which limits what agents can autonomously do, which is the point and also the cost.
- **Docker's sibling-container model shares a kernel.** The Gateway holds the host Docker socket, which is equivalent to root on the host. Sandbox escape is a live concern; Docker does not fully prevent it. Escalation path: Firecracker/microVM isolation, or sandboxes on dedicated instances. Tracked as risk R7/A8.
- **Human approval is a rate-limited resource.** Approval fatigue converts a control into a rubber stamp. This is a *social* control — the weakest kind. Monitor approval-queue depth and wait time; keep the privileged tier small enough that approvals stay rare and therefore stay real.
- Deny-by-default egress is operationally annoying. Every new tool needs an allowlist entry. Expect pressure to widen it; resist.
- n8n workers are an acknowledged soft spot: arbitrary outbound HTTPS is inherent to a workflow-integration tool. They compensate with narrow IAM roles and by never holding the Gateway's credentials.
- **None of this exists in Milestone 1.** Until the sandbox boundary is built, the platform must not be pointed at production data or real credentials. This is a sequencing requirement, not a hardening backlog item ([12 §12.4](../architecture/12-risks-assumptions-constraints.md)).

## Alternatives considered

**Prompt-injection detection / input filtering / guardrails as the primary defence.** Rejected as *primary*. Filters have false-negative rates; shells have no partial failure. Bedrock Guardrails are retained on the inference path for **content safety**, which is a different and legitimate problem. They are not a privilege control and must not be mistaken for one.

**Run the agent with no tools.** Eliminates the risk and the platform. Rejected — the point is autonomous agents that act.

**Follow OpenClaw's guidance literally: dedicated machine, no sensitive data, no credentials.** The right instinct, and insufficient. A platform must hold credentials to be useful. Our answer keeps the *sandbox* credential-free while the *Gateway* holds narrowly scoped ones, so the OpenClaw guidance is honoured exactly where the untrusted code runs.

**Trust the model.** Rejected. The model is not the attacker; it is the vector. Trusting it is precisely the confused-deputy mistake.

**Give the sandbox a scoped IAM role instead of no credentials.** Tempting and more convenient. Rejected: a scoped role in a sandbox running attacker-influenced code is a scoped role in the attacker's hands. Narrow tools on the Gateway achieve the same capability with an audit point and an approval gate.
