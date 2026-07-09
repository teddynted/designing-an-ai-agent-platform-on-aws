# Architecture Decision Records

Each ADR records one decision: the forces acting on it, what was chosen, what was rejected, and what it costs. An ADR is immutable once accepted — a changed decision is a new ADR that supersedes the old one, so the reasoning trail survives.

Format: [Michael Nygard's](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions.html), lightly extended with an explicit **Consequences → negative** section, because the negative consequences are the part people skip and the part that matters in eighteen months.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-three-plane-decomposition.md) | Decompose the platform into control, agent, and inference planes | Accepted |
| [0003](0003-model-gateway-seam.md) | Introduce a Model Gateway seam with an OpenAI-compatible contract | Accepted |
| [0004](0004-inference-routing-policy.md) | Route inference by latency tolerance, with Bedrock as the backstop | Accepted |
| [0005](0005-spot-only-for-stateless-workloads.md) | Use Spot only for stateless, interruptible workloads | Accepted |
| [0006](0006-startup-time-strategy.md) | Minimise EC2 startup time with baked AMIs, not warm pools | Accepted |
| [0007](0007-cloudformation-stack-layering.md) | Layer CloudFormation stacks by rate of change; SSM parameters as the cross-stack contract | Accepted |
| [0008](0008-n8n-queue-mode-managed-datastores.md) | Run n8n in queue mode on managed datastores | Accepted |
| [0009](0009-openclaw-gateway-singleton.md) | Treat the OpenClaw Gateway as an EFS-backed singleton with fast recovery | Accepted |
| [0010](0010-agent-sandbox-containment.md) | Contain agents by removing privilege, not by filtering prompts | Accepted |
| [0011](0011-account-per-environment.md) | One AWS account per environment | Accepted |
| [0012](0012-scale-inference-to-zero.md) | Scale the self-hosted inference fleet to zero | Accepted |

## Which decisions are load-bearing

If you read three, read these. Every other decision in the platform is downstream of one of them.

- **[0002](0002-three-plane-decomposition.md)** — the separation that lets cost optimisation and high availability stop fighting.
- **[0003](0003-model-gateway-seam.md)** — the seam that makes providers swappable *and* makes Spot GPUs safe.
- **[0010](0010-agent-sandbox-containment.md)** — the reframing of prompt injection from a content problem to a privilege problem.
