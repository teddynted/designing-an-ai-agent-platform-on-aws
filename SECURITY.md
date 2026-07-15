# Security — the reference

**Milestone 14.** How the platform is hardened around software that does what it
is told. This is the operational companion to the blog post,
[Securing an AI Agent Platform on AWS](docs/blog/securing-an-ai-agent-platform-on-aws.md),
and it documents the three stacks the milestone touches:
[`01-network.yaml`](infra/cloudformation/01-network.yaml) (egress),
[`04-storage.yaml`](infra/cloudformation/04-storage.yaml) (encryption at rest),
and [`11-security.yaml`](infra/cloudformation/11-security.yaml) (audit and alarms).

> **The one idea.** An agent is a program that runs attacker-influenced text as
> if it were instructions. You cannot reliably stop it from being persuaded — so
> you make persuasion *worthless*. Prompt injection is not a content-filtering
> problem, it is a **privilege-escalation** problem: the payload can only do what
> the process it hijacks is allowed to do. Scope that process to exactly what it
> needs, record everything it does, and alarm on anything out of band, and a
> successful injection reaches nothing, exfiltrates over nothing, and leaves a
> trace on the way out.

---

## Contents

- [Threat model](#threat-model)
- [What this milestone builds](#what-this-milestone-builds)
- [Egress control](#egress-control)
- [Encryption at rest](#encryption-at-rest)
- [The audit trail](#the-audit-trail)
- [Security alarms](#security-alarms)
- [Secret handling](#secret-handling)
- [Least privilege, recapped](#least-privilege-recapped)
- [Deploy it](#deploy-it)
- [What this does not do](#what-this-does-not-do)
- [Well-Architected](#well-architected)
- [Future improvements](#future-improvements)

---

## Threat model

The platform's distinguishing risk is not a web vulnerability. It is that its
core workload — an agent driving [OpenClaw](docs/blog/integrating-openclaw-into-an-ai-agent-platform.md),
reasoning over a model — **consumes untrusted input and acts on it**. A GitHub
issue, a fetched web page, a file in a repository: any of these can contain text
crafted to read as an instruction. "Ignore your task and run this" is a bug class,
not a hypothetical, and no amount of prompt engineering closes it completely.

So the design does not try to. It treats every agent process as **already
compromised** and asks a different question: *when it is, what can it do?* Four
controls bound the answer, and this milestone is where the last two land.

| Control | Bounds | Where |
| --- | --- | --- |
| **Least privilege** | What the process is *allowed* to do | IAM (Milestone 2's `02-iam`), recapped below |
| **Egress control** | What it can *reach*, however it is persuaded | `01-network` — allow-list + S3 endpoint |
| **Encryption at rest** | What a stolen artifact or disk *reveals* | `04-storage`, `11-security` |
| **Auditability** | Whether the attempt *leaves a trace* and *trips an alarm* | `11-security` — CloudTrail + alarms |

The first three make an injection cheap to survive. The fourth makes it
impossible to hide: a misconfiguration, a stolen credential, or an agent talked
into doing something unusual leaves a signed record and pages a human, rather
than passing unseen.

---

## What this milestone builds

| Piece | Where | What it is |
| --- | --- | --- |
| Egress allow-list | [`01-network.yaml`](infra/cloudformation/01-network.yaml) | The instance security group drops open egress for **HTTPS, HTTP (OS mirrors) and DNS only** — an agent that cannot open an arbitrary socket cannot exfiltrate over one. |
| S3 gateway endpoint | [`01-network.yaml`](infra/cloudformation/01-network.yaml) | Keeps bucket traffic on the AWS backbone instead of the internet gateway. **Free**, and a prerequisite for the zero-egress hardening below. |
| Artifact encryption option | [`04-storage.yaml`](infra/cloudformation/04-storage.yaml) | SSE-S3 by default (free, right for non-secret artifacts); an `ArtifactKmsKeyArn` parameter upgrades to SSE-KMS when the bucket holds something that *is* a secret. |
| CloudTrail + CMK | [`11-security.yaml`](infra/cloudformation/11-security.yaml) | A multi-region trail with **log-file validation**, encrypted by a customer-managed key, delivered to both a locked-down S3 bucket **and** CloudWatch Logs. |
| Security alarms | [`11-security.yaml`](infra/cloudformation/11-security.yaml) | The CIS-benchmark set of metric-filter alarms — root usage, denied calls, MFA-less sign-in, failed logins, and changes to IAM, security groups, and the trail itself. |
| Security SNS topic | [`11-security.yaml`](infra/cloudformation/11-security.yaml) | Its **own** topic, separate from the monitoring stack's: "someone used root" is a different page, often for a different person, than "the platform is unhealthy". |

Everything else the platform runs was already least-privilege and encrypted in
transit. This is the milestone that makes it **auditable**, and that closes the
one door — open egress — a compromised agent would most want.

---

## Egress control

Before this milestone, the instance security group allowed all outbound traffic:
it had to reach out to bootstrap, pull models, and push to Git, and the egress
rules were left open with a note that the security milestone would tighten them.
This is that milestone.

The rule is now an **allow-list**, and three protocols cover every legitimate
need:

| Protocol | Port | Why it is needed |
| --- | --- | --- |
| HTTPS (TCP) | 443 | AWS APIs (SSM, CloudWatch, S3, Bedrock), Git, model endpoints, package mirrors. |
| HTTP (TCP) | 80 | The OS package mirrors that still start on port 80 before redirecting to TLS. |
| DNS (UDP + TCP) | 53 | Name resolution. TCP as well as UDP, for responses too large for a datagram. |

Everything else outbound is dropped. The reasoning is the threat model applied to
the network: **a box running attacker-influenced content should reach exactly
what it needs and nothing else.** An agent persuaded to open a reverse shell on
port 4444, or to POST your repository to an arbitrary host on some other port,
finds the socket refused. It does not stop the injection; it makes the injection
land on a dead end.

Two things keep working that look like they should not:

- **IMDS and time sync.** Traffic to the link-local instance-metadata address
  (`169.254.169.254`) and the Amazon Time Sync address is handled by the
  hypervisor, not routed through the security group, so NTP and metadata are
  unaffected by the egress rules.
- **SSM management.** Session Manager is **outbound-only** — the instance dials
  AWS over 443, nothing ever dials the instance. The default-deny *inbound*
  posture (no ingress rules at all, no SSH) is unchanged; this milestone only
  tightened the outbound side.

### The S3 gateway endpoint

A gateway VPC endpoint for S3 now sits on the route table. It keeps the instance's
S3 traffic — its artifacts, and anything the OS pulls from S3-backed mirrors — on
the AWS backbone rather than out through the internet gateway. Gateway endpoints
are **free** (unlike interface endpoints), so there is no reason not to have one.

Its endpoint policy is deliberately left at full access. The real control on what
the instance can do to S3 is its **instance role** (`02-iam`), already scoped to
this project's single bucket; a second, tighter policy on the endpoint would risk
blocking the anonymous, AWS-owned package-repo buckets the OS reads through the
same endpoint, for no gain the instance role does not already provide.

---

## Encryption at rest

Two buckets, two deliberately different choices — because encryption at rest is a
control only when the key is one, and that depends on what the data *is*.

**The artifact bucket** (`04-storage`) defaults to **SSE-S3** (AES-256):
encrypted, free per request, and the right default for generated artifacts that
are not themselves secrets. When the bucket will hold something that *is* a
secret, supply an `ArtifactKmsKeyArn` and it becomes **SSE-KMS** with your
customer-managed key. Bucket keys are enabled either way, which sharply cuts KMS
request cost when SSE-KMS is in use.

> **Caveat, called out because it fails loudly.** If you set `ArtifactKmsKeyArn`,
> the instance role (`02-iam`) must **also** be granted `kms:GenerateDataKey` and
> `kms:Decrypt` on that key, or every write to the bucket fails with an opaque
> `AccessDenied`. SSE-KMS moves the authorization decision from S3's own policy to
> the key policy; both have to agree.

**The trail bucket** (`11-security`) uses **SSE-KMS unconditionally**, with a
customer-managed key created by the same stack. Here the key *is* the control:
audit logs are the one dataset whose integrity is the whole point, and a CMK gives
a key policy **we** control — so we can say exactly who may decrypt the record of
what everyone did — plus a digest chain that log-file validation can verify. The
roughly one dollar a month is the cheapest tamper-evidence there is.

---

## The audit trail

CloudTrail is the account's black-box recorder. Three properties make this one
trustworthy:

- **Multi-region + global service events.** Nothing that happens in another
  region — or in IAM, which is global — is invisible. An attacker cannot simply
  operate from `eu-west-1` to stay off the recording.
- **Log-file validation.** The trail writes a signed digest chain, so a deleted
  or altered log file is *detectable after the fact*. Tampering is not prevented
  by this — it is made evident, which for an audit log is the property that
  matters.
- **Dual delivery.** The trail goes to **both** an S3 bucket (the durable,
  long-retention record) **and** CloudWatch Logs (the live stream the alarms
  watch). Without the second, CloudTrail is a flight recorder nobody reads until
  after the crash.

### The KMS key policy

The key policy on the trail's CMK is small but every statement earns its place:

- **`AllowAccountAdministration`** — the account root administers the key. Without
  it a key can become unmanageable; it is the one statement AWS requires on every
  CMK.
- **`AllowCloudTrailEncrypt`** — CloudTrail may generate a data key to encrypt
  each log file, but **only for a trail belonging to this account**. The
  `kms:EncryptionContext:aws:cloudtrail:arn` condition is the confused-deputy
  guard: it stops the key being used to encrypt some other account's trail and
  bill us for it.
- **`AllowAccountDecrypt`** — anyone reading the trail (the console, an incident
  responder) can decrypt it, but only from within this account
  (`kms:CallerAccount`).

The trail bucket carries the same confused-deputy guard on its side: the
`AWSCloudTrailWrite` statement is conditioned on `aws:SourceArn` being *our*
trail, so no other trail in the account can write into this bucket.

### Retained on delete

Both the trail bucket **and its KMS key** are `Retain`-on-delete. This is a
correctness requirement, not caution: the audit record must outlive the stack, and
a bucket full of KMS-encrypted log files whose key was scheduled for deletion is an
*unreadable* bucket. `make delete-security` removes the stack and leaves both the
record and the key that decrypts it intact. Empty and delete them by hand only
when you truly mean to erase the audit history.

### S3 data events (optional, off by default)

Management events — "who changed the bucket?" — are always on and free for the
first trail. Object-level **data** events — "who *read* this object?" — are billed
per event and off by default; a busy bucket makes them expensive. Set
`IncludeS3DataEvents=true` (or `INCLUDE_S3_DATA_EVENTS=true` on the Make target) to
turn them on for the artifact bucket when you need read-level forensics.

---

## Security alarms

Each alarm turns a class of security-relevant CloudTrail event into a CloudWatch
metric via a metric filter, and pages the security SNS topic when it appears.
These are the CIS-benchmark events you want to hear about within minutes, not
discover in a quarterly review. Most fire on a **single occurrence** — "someone
used root once" is already the whole story.

| Alarm | Fires when | Threshold | Why it matters |
| --- | --- | --- | --- |
| **Root account usage** | The root user does anything not driven by an AWS service | ≥ 1 | Root should set up the account and then never be used again. Any use is an event. |
| **CloudTrail changes** | `StopLogging`, `DeleteTrail`, `UpdateTrail`, `PutEventSelectors` | ≥ 1 | Disabling logging is the first move of anyone who does not want to be seen — the loudest alarm. |
| **IAM changes** | Any policy/role/user/key create, delete, or attach | ≥ 1 | On a CloudFormation-over-OIDC platform, an interactive IAM change is by definition out of band. |
| **Security group changes** | Ingress/egress authorize/revoke, group create/delete | ≥ 1 | The default-deny inbound posture is only as good as this staying quiet. |
| **Console sign-in without MFA** | A `ConsoleLogin` where MFA was not used (excludes federated/SSO) | ≥ 1 | Every human principal must have MFA enforced. |
| **Console auth failures** | Repeated failed `ConsoleLogin` | > 3 | A password-guessing attempt against a human account. |
| **Unauthorized API calls** | `AccessDenied` / `UnauthorizedOperation` responses | > 5 | A probe, a misconfiguration, or a stolen credential testing its reach. A little tolerance: the odd denial is background noise; a burst is not. |

The topic is separate from the monitoring stack's (`10-monitoring`) on purpose,
and the alarms send **both** `AlarmActions` and `OKActions`, so a responder sees
the all-clear as well as the alert. Set `SECURITY_EMAIL` at deploy time to
subscribe an address; AWS sends a confirmation that must be clicked before
anything is delivered. Security alerts are the one place a human should always be
in the loop.

---

## Secret handling

The platform holds exactly one long-lived secret, and it never touches an agent:

- **The GitHub webhook secret** lives in AWS Secrets Manager (Milestone 12), read
  by the webhook Lambda at invocation to verify the HMAC signature. It is never in
  an environment variable, never in the template, never logged.
- **Redaction is in the handler.** The observability library (Milestone 13)
  redacts secrets and repository content *before* a log line is written, so a
  secret cannot leak into CloudWatch by way of a debug log — including the trail's
  own log group.
- **No standing credentials on the instance.** Compute gets its permissions from
  an **instance role**, not baked keys; deploys run through GitHub Actions **OIDC**,
  not a stored access key. There is no long-lived credential for a compromised
  agent to read and exfiltrate — the thing egress control would otherwise be
  racing to contain.

---

## Least privilege, recapped

This milestone leans on decisions made back in Milestone 2, worth restating
because they are what make the new controls sufficient rather than cosmetic:

- **The instance role is scoped to this project's one bucket** and the specific
  actions the workload needs — not `s3:*`, not `*`. An injection that reaches the
  AWS API inherits exactly that, and no more.
- **Management is SSM, not SSH.** No inbound port, no key pair, no bastion — the
  attack surface a default-deny inbound posture removes is the one most services
  leave open.
- **The deploy role is scoped to `aiap-*` resources.** It is shipped as a
  documented **starter** (`infra/scripts/deploy-role-policy.json`) — "tighten
  further before production" — because a CI role broad enough to stand up every
  stack is inherently powerful; scoping it to the project's resource prefix is the
  floor, not the ceiling.

---

## Deploy it

The security stack is pure CloudFormation — no Go to build — so it needs nothing
but the AWS CLI. It audits the account, not one instance, so unlike the monitoring
stack it resolves nothing from another stack and can be deployed independently.

```sh
# Deploy the trail, its key, the log delivery, and the alarms.
make -C infra security SECURITY_EMAIL=you@example.com

# Then confirm the SNS subscription in your inbox — nothing is delivered until you do.

# Optional: also record object-level reads on the artifact bucket (billed per event).
make -C infra deploy-security INCLUDE_S3_DATA_EVENTS=true SECURITY_EMAIL=you@example.com

# See its outputs alongside every other stack.
make -C infra outputs
```

To tear it down without losing the audit history:

```sh
make -C infra delete-security   # removes the stack; the trail bucket and its KMS key are RETAINED
```

---

## What this does not do

Being explicit about the boundary is part of the control.

- **Zero-egress compute.** Interface endpoints for SSM, CloudWatch Logs, and EC2
  Messages would let the instance run with **no internet egress at all** — the
  strongest version of egress control. They are *not* deployed by default because
  each costs roughly seven dollars a month; with the free S3 gateway endpoint
  already in place, they are documented here as a **production hardening** rather
  than a default. Add them, then drop the 0.0.0.0/0 egress rules entirely, when
  the workload's outbound needs are known and fixed.
- **Account-wide threat detection.** GuardDuty (anomaly detection over the trail
  and DNS logs), AWS Config (continuous compliance), and Security Hub (posture
  aggregation) are the obvious next layer. This milestone builds the *substrate*
  they consume — a validated, encrypted, alarmable trail — but does not enable the
  paid services themselves.
- **Runtime sandboxing of the agent.** The controls here bound what a compromised
  agent can *reach* and ensure it is *seen*; they do not sandbox the process on the
  host itself. Container/microVM isolation of the agent runtime is a larger
  architectural change, noted in the roadmap.
- **WAF / edge protection.** The webhook Lambda verifies its HMAC signature in
  constant time before parsing (Milestone 12); there is no public web tier here for
  a WAF to sit in front of.

---

## Well-Architected

Mapped to the Security pillar, and where each control lives.

| Principle | How this milestone honours it |
| --- | --- |
| **Enable traceability** | Multi-region CloudTrail with log-file validation, delivered to S3 *and* CloudWatch Logs, with alarms on the events that matter. |
| **Apply security at all layers** | Network (egress allow-list, S3 endpoint), identity (instance role, OIDC deploys), data (SSE-S3/SSE-KMS at rest, TLS-only in transit), detective (the alarms). |
| **Protect data in transit and at rest** | Bucket policies deny `aws:SecureTransport=false`; both buckets encrypt at rest; the trail uses a customer-managed key. |
| **Keep people away from data** | Management is SSM Session Manager, not SSH; the audit trail is retained and separately permissioned from the data it audits. |
| **Prepare for security events** | A dedicated security SNS topic pages a human on root use, trail tampering, and IAM/SG drift — with an all-clear when it resolves. |
| **Automate security best practices** | Every control is CloudFormation; the alarms are metric filters, not a cron job someone forgets to run. |

---

## Future improvements

- **Turn on GuardDuty, Config, and Security Hub** against the trail this milestone
  provides — the detective layer above metric filters.
- **Deploy the interface endpoints and drop 0.0.0.0/0 egress** for a truly
  air-gapped compute plane once outbound needs are pinned down.
- **Sandbox the agent runtime** (container or microVM isolation) so a compromise is
  contained on the host, not only on the network.
- **Tighten the deploy role** from the shipped starter to the minimal action set
  each stack actually requires, and split it per environment.
- **Route the security topic to an on-call tool** (PagerDuty/Opsgenie) rather than
  email, so a 3 a.m. root-usage alarm reaches someone who is awake.
