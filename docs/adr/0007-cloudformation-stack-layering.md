# ADR-0007: Layer CloudFormation stacks by rate of change; use SSM Parameter Store as the cross-stack contract

**Status:** Accepted
**Date:** 2026-07-09

## Context

The brief mandates CloudFormation. Two structural decisions follow, and both have long half-lives.

**How to divide the templates.** A single monolithic stack makes every change — a Lambda tweak, a new alarm — a risk to the VPC. Dividing by service type ("all Lambda here, all EC2 there") groups resources that change at wildly different rates and have wildly different blast radii.

**How stacks reference each other.** CloudFormation's native answer is `Outputs` + `Export` + `Fn::ImportValue`. It is the documented path, and it has a property that is easy to miss until it hurts: **an exported value cannot be modified or deleted while any stack imports it.**

Export the VPC ID and you have welded the network stack to every stack above it. Changing a subnet becomes a coordinated multi-stack teardown. This is the most common way a CloudFormation estate becomes un-evolvable, and it surfaces eighteen months in — exactly when the network first needs to change.

## Decision

**Layer stacks by rate of change and blast radius**, lowest and slowest at the bottom:

```
00-org-baseline   SCPs, CloudTrail, GuardDuty        ~yearly
10-network        VPC, subnets, NAT, endpoints       ~yearly
20-identity       IAM roles, instance profiles        monthly
30-data           S3, RDS, EFS, ElastiCache, Secrets  monthly
40-platform       ASGs, ALBs, launch templates        weekly
50-serverless     Lambda, EventBridge, SQS            daily
60-observability  alarms, dashboards                  weekly
```

A stack may depend on layers **below** it, never above.

**Cross-stack references go through SSM Parameter Store, not CloudFormation Exports.** Lower layers write outputs to a namespaced parameter path; upper layers read them at deploy time.

```
/platform/{env}/network/vpc-id
/platform/{env}/identity/n8n-worker-role-arn
/platform/{env}/data/artifacts-bucket-name
/platform/{env}/ami/ollama-gpu/latest
```

Stateful resources in `30-data` carry `DeletionPolicy: Retain` and `UpdateReplacePolicy: Retain`.

## Consequences

**Positive**

- **The network stack can evolve.** Replacing a subnet does not require tearing down every dependent stack, because nothing structurally imports it.
- Blast radius tracks the layer. A bad `50-serverless` change costs an event handler; a bad `10-network` change costs the platform. They get different review gates and different cadences.
- Daily-changing stacks deploy in minutes without touching yearly-changing ones.
- SSM parameters are the natural home for the golden-AMI IDs the deployment pipeline already publishes ([ADR-0006](0006-startup-time-strategy.md)), so the AMI rollout and the cross-stack contract use one mechanism.
- Parameters are readable at runtime too, not only at deploy time — useful for the Model Gateway's routing policy ([ADR-0004](0004-inference-routing-policy.md)).

**Negative**

- **CloudFormation no longer enforces the dependency.** You can delete a parameter that an upper stack needs, and nothing stops you until the next deployment fails. Exports gave you that safety; we have traded it away deliberately. Mitigation: a CI pre-flight check that every referenced parameter resolves before a change set is created. This is a real loss and the check is not optional.
- Coupling becomes **temporal** (read at deploy time) rather than **structural**. Deploy order matters and is enforced by the pipeline, not by the tool. Deploying `40-platform` before `10-network` fails confusingly.
- Parameter drift is possible: someone edits a parameter by hand, and the next deploy silently picks it up. Restrict write access to the deployment role.
- More stacks means more `ChangeSet` operations and a longer full-estate deploy.
- Layer boundaries require judgement. Resources that could sit in two layers will be argued about.

## Alternatives considered

**One monolithic stack.** Simplest, atomic, natively dependency-ordered. Rejected: every change risks every resource, deploy times grow without bound, and CloudFormation's 500-resource limit is a real ceiling. Worst of all, a routine Lambda change acquires the review weight of a VPC change.

**CloudFormation Exports / `Fn::ImportValue`.** The native, CloudFormation-enforced option, and genuinely better on the safety axis. Rejected because it makes lower layers immutable in practice. The failure mode is silent and delayed — everything works beautifully until the first time you must change the network, at which point you cannot. We prefer a known, checkable weakness (a missing parameter, caught in CI) to an unknown, unfixable one (a welded estate).

**Nested stacks with parameter passing from a root stack.** A reasonable middle ground; the root stack orchestrates and passes values down. Rejected because it recreates the monolith's coupling at the root: the root stack must be updated for any cross-layer change, and it becomes the thing everyone is afraid to touch.

**Terraform or Pulumi.** Better ergonomics — loops, types, composition, a real dependency graph across state. Rejected: CloudFormation is mandated, and it does buy **zero additional state management** (no state backend to secure, lock, and back up), native drift detection, and native change sets. For a platform whose thesis is minimising operational surface, not having a Terraform state file to protect is worth something.

**AWS CDK.** Attractive, and **not excluded by this ADR** — CDK synthesises CloudFormation, so it is a way of *authoring* these layers, not an alternative to them. Deferred: adds a build step and a language runtime to the deployment path. Revisit if template repetition becomes painful.
