# ADR-0011: One AWS account per environment

**Status:** Accepted
**Date:** 2026-07-09

## Context

Environments must be isolated. The cheap approach is one account with IAM policies, tags, and naming conventions separating `dev` from `prod`. It works for ordinary workloads and it is what most teams start with.

This platform is not an ordinary workload. The agent plane **executes model-directed code while holding an instance profile.** The realistic failure is not "an engineer wrote a bad IAM policy." It is:

> An agent was persuaded, via text it read, to use the credentials it legitimately holds.

Against that failure, IAM separation offers nothing. The identity is not compromised — it is *used*, exactly as designed, by the wrong instructions. Policy boundaries assume the principal is trustworthy. Here the principal is a language model reading untrusted input.

The account boundary is the only boundary that holds when the identity itself is the vector.

## Decision

**One AWS account per environment**, under an Organisation.

```
Organisation
├── management     billing, SCPs, no workloads
├── security       CloudTrail sink, GuardDuty admin, log archive
├── platform-dev   full platform, permissive, real Spot, real Bedrock
└── platform-prod  full platform, restrictive SCPs
```

Baseline SCPs on `platform-prod`:

- Deny IAM user creation (roles only, via Identity Center)
- Deny CloudTrail disablement
- Deny leaving the Organisation
- Deny non-approved regions
- Deny disabling EBS default encryption
- Deny `elasticfilesystem:DeleteFileSystem` — Gateway state is unrecoverable by automation ([ADR-0009](0009-openclaw-gateway-singleton.md))

CloudTrail from all accounts aggregates into `security`. The same CloudFormation templates deploy to both workload accounts, parameterised by environment ([06 §6.6](../architecture/06-deployment.md)).

## Consequences

**Positive**

- **A compromised prod agent cannot reach dev, and a compromised dev agent — running looser controls, freer experiments, and unreviewed workflows — cannot reach prod.** Dev is where risky agent experiments belong, and the account boundary is what makes that safe.
- SCPs are enforced *above* the account's own administrators. An SCP-denied action stays denied even to an account root user, which is not true of any IAM policy.
- Blast radius of a misconfiguration is one environment.
- Clean cost attribution per environment, without tag discipline.
- Service quotas do not contend across environments — dev load testing cannot exhaust prod's Bedrock quota.
- A tamper-evident audit trail: CloudTrail lands in an account the workload identities cannot write to.

**Negative**

- **Operational overhead.** Multiple accounts to bootstrap, wire, and keep consistent. Account vending should be automated (Control Tower or equivalent) or it will drift.
- Cross-account access requires assumed roles and trust policies — more moving parts than one account.
- Some resources are duplicated per account (VPC endpoints, NAT Gateways, ECR repositories), which costs real money at small scale. Roughly $70–100/mo of duplication.
- Shared artifacts (golden AMIs, container images) must be explicitly shared or replicated across accounts, adding a distribution step to the AMI pipeline ([ADR-0006](0006-startup-time-strategy.md)).
- CI/CD needs cross-account deployment roles, which is the highest-privilege path in the estate and must be guarded accordingly.

## Alternatives considered

**Single account, IAM + tag separation.** Cheapest, simplest, and adequate for most workloads. Rejected here for the reason in Context: it does not defend against a legitimate identity being misused, which is this platform's characteristic failure. It also permits a dev experiment to consume prod's Bedrock quota, and offers no SCP-style control that survives an account administrator.

**Single account, separate VPCs per environment.** Better network isolation, same IAM weakness. A compromised dev agent with an over-broad role still calls `s3:GetObject` on a prod bucket — network separation does not constrain the AWS control plane, only the data plane. Rejected.

**Account per component (network account, data account, workload account).** Stronger still, and standard in large enterprises. Rejected as premature: the operational cost of cross-account VPC sharing, cross-account IAM, and multi-account deployment orchestration exceeds the benefit at this platform's size. Environment is the boundary that matters first. Revisit at multi-tenancy ([11 — Extensibility](../architecture/11-extensibility.md)), where **account-per-tenant** may become the right — and expensive — answer.

**Account per developer.** Useful for experimentation and genuinely attractive for a platform where agents run shell commands. Deferred: adds vending and cost-control burden. Reconsider once more than a handful of engineers work on the platform.
