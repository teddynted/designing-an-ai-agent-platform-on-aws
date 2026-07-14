# Using n8n as the Workflow Engine for AI Automation

> **Milestone 5 — Self-hosted n8n Integration.**
> This milestone integrates n8n; it does **not** deploy it. n8n's infrastructure
> lives in the `self-hosted-n8n-on-aws` repository, which owns it. What follows is
> the contract between the two, and the code is in
> [`internal/workflow`](../../internal/workflow) and
> [`internal/n8n`](../../internal/n8n).

*Audience: platform and backend engineers wiring an orchestrator into an AI
system, and anyone who has ever watched an automation open the same pull request
twice.*

The last four milestones built a foundation and put nothing on it. There is a VPC,
a Spot instance that boots in six seconds from a custom AMI, an event bus, and a
drain agent that saves work when AWS takes the machine away. It is a well-built,
empty platform.

This is where the work starts arriving. And the first real decision is not *what*
the platform does with a GitHub event — it is **where the long, messy, multi-step
part of that work is allowed to live.**

---

## Contents

- [The shape of the work](#the-shape-of-the-work)
- [Why an orchestrator, and why n8n](#why-an-orchestrator-and-why-n8n)
- [The integration, not the deployment](#the-integration-not-the-deployment)
- [The seam](#the-seam)
- [The hard problem: a retry is not free](#the-hard-problem-a-retry-is-not-free)
- [What is retried, and what must never be](#what-is-retried-and-what-must-never-be)
- [A 200 is not a success](#a-200-is-not-a-success)
- [The payload is not yours](#the-payload-is-not-yours)
- [Observability](#observability)
- [Adding a workflow](#adding-a-workflow)
- [Testing it](#testing-it)
- [Lessons learned](#lessons-learned)
- [What comes next](#what-comes-next)

---

## The shape of the work

A push lands on `main`. The platform is supposed to: read the diff, read enough of
the repository to have context, ask a model for a draft, turn the draft into a
file, open a pull request, wait for a human to approve it, publish, and announce
it.

Look at that list honestly. It is **minutes to hours long**. It has a **human in
the middle of it**. Half its steps call something that fails on a bad afternoon —
a model API, GitHub, Slack. It will need to be changed constantly, because the
prompt is wrong, the tone is wrong, the wrong branch triggered it.

You can absolutely write that as code. Then you own: retries between every step,
back-off, partial-failure recovery, a state machine to survive a Lambda timeout,
somewhere to park the run while a human reviews it, and a way to see where run
#4,182 got stuck at 3am. And every tweak to the prompt is a deploy.

That is not the platform's problem to solve. **It is a solved problem, and the
solutions are called orchestrators.**

## Why an orchestrator, and why n8n

The honest case for n8n here, including the parts that are not flattering:

**It makes the long stuff visible.** An n8n execution is a picture of what
happened, node by node, with the data at each step. When a blog post does not
appear, you look at the run and see which node threw. Reproducing that in code
means building a UI nobody asked you to build.

**It makes the changeable stuff cheap to change.** The prompt, the branch filter,
the Slack channel, the order of two steps — these change weekly and they belong
somewhere that changing them is not a deploy. Hard-code them in a Lambda and every
tweak is a release.

**It already does the boring parts.** Retries per node, waits, human approval
steps, fan-out, cron. Any of these is a day's work to do properly and a career's
work to do properly *repeatedly*.

**And self-hosted, because of what flows through it.** The payloads carry
repository content and, on a bad day, credential-shaped fields. That is not going
through someone else's SaaS.

And what it is **not**: n8n is not a good place to put anything that must be
correct, transactional, or hot-path. It is an orchestrator, not a service. The rule
this platform follows — **the plane that does the work goes on Spot; the plane that
*remembers* the work does not** — is exactly why n8n itself is not on Spot in the
other repository. It is the thing that reschedules interrupted work; it cannot be
the thing that gets interrupted.

## The integration, not the deployment

The brief for this milestone was unusually clear about a boundary, and the
repository had already committed to the same one:

> *If a change affects more than one component, it belongs in the platform. If it
> affects exactly one, it belongs in that component's repository.*

An n8n version bump affects n8n. **The shape of the JSON we send affects everything
that sends it.**

So `self-hosted-n8n-on-aws` owns the servers, the database, the queue mode, the
upgrades, the backups. This repository owns **the contract**: the payload, the auth
header, the retry policy, the errors, the idempotency key. Nothing in this
milestone provisions anything, and if it ever starts to, the boundary has failed.

That sounds like an administrative detail. It is the reason this milestone is 900
lines of Go and not a second CloudFormation stack fighting with the first one over
who owns the security group.

## The seam

The obvious implementation is one line in the webhook handler:

```go
http.Post("https://n8n.internal/webhook/blog", "application/json", body) // don't
```

It works. It also welds the platform to n8n. Every caller now knows the URL scheme,
the auth header, the retry policy, and the response shape — and every caller
invents its own slightly different version of each. Replacing the engine later, or
running two during a migration, means touching all of them.

So there is an interface:

```go
type Engine interface {
	Name() string
	Workflows() []string
	Trigger(ctx context.Context, req Request) (Result, error)
}
```

and a `Service` above it that does what must be identical **for every engine**:
validate the request, derive a correlation ID, time it, log it. The engine below
does what is specific and dirty: speak HTTP, authenticate, survive a flaky network.

That division is the whole design, and it is worth being precise about *why* the
line is drawn there. If each engine did its own logging, no dashboard could span
them. If each invented its own correlation, a GitHub delivery could not be followed
across the platform. Those must not vary. Whereas *how you talk to n8n* should be
free to be thrown away without taking the observability with it.

The test that the seam is real is mechanical: **`internal/workflow` does not import
`internal/n8n`.** If that dependency ever points the other way, the interface is a
decoration. A second test says the same thing behaviourally — the `Service`'s test
suite runs entirely against a fake engine, with no HTTP anywhere. If that test ever
needs a server, the abstraction has failed.

## The hard problem: a retry is not free

Here is the part that took the longest to get right, and it is not the part the
brief emphasised.

Triggering a workflow is **not a read**. Retrying a GET is free. Retrying a *POST
that opens a pull request* is not.

```
Platform ──POST /webhook/blog──▶ n8n
                                  └─▶ workflow runs, opens a PR
Platform ◀──── timeout ────────╳    (the answer is lost)
Platform ──POST /webhook/blog──▶ n8n     ← the retry
                                  └─▶ workflow runs AGAIN, opens a SECOND PR
```

The thing to internalise, because it is the root of the whole problem:

> **A timeout tells you that no answer arrived. It tells you nothing about whether
> the request did.**

The work may be running *right now*. The connection died on the way back. And the
naive response — "so don't retry" — is worse, because then a genuinely dropped
request silently never runs, and nobody notices until someone asks where the blog
post is.

You cannot escape this. You can only make the duplicate **detectable**. Every
request carries an idempotency key:

```
X-Idempotency-Key: blog-generator:delivery-abc-123
```

derived from the event's own ID — GitHub's delivery ID — and therefore **stable by
construction**. The same delivery, retried by us or replayed by an operator next
week, produces the same key. A random key here would look sophisticated and defeat
the entire purpose.

That makes the **transport** at-least-once, and lets n8n make the **execution**
effectively-once. But note carefully what that sentence does *not* say:

> ⚠️ **It only works if the workflow on the other side actually checks the key.**

This repository cannot enforce that. The key travels in a header *and* in the body
— because n8n workflows find body fields far easier to work with, and a key the
workflow cannot conveniently reach is a key nobody will use — but an n8n workflow
that ignores it will duplicate work the first time the network hiccups.

It is the single most important thing to get right on the other side of the
boundary, it is invisible from this one, and it is the first thing to check when
something has happened twice. So it is in bold in the integration docs, and it is
the one test I would not delete:

```go
// A retried trigger MUST carry the same idempotency key as the attempt it is
// retrying. If it does not, n8n cannot tell a retry from a new event, and a
// blog-generating workflow opens two pull requests.
func TestRetriesReuseTheSameIdempotencyKey(t *testing.T) { … }
```

## What is retried, and what must never be

Retrying the wrong thing is worse than not retrying at all.

| Failure | Retry? | Why |
| --- | --- | --- |
| Connection refused, DNS, TLS reset | ✅ | The work almost certainly did not start. The safest failure there is. |
| Timeout | ✅ | It may have started. Retry, and let the key sort it out. |
| `429`, `5xx` | ✅ | n8n restarting is the textbook transient failure. |
| `401`, `403` | ❌ | The token will not become valid because you asked again. |
| `404` | ❌ | The workflow is not active. Asking again will not activate it. |
| `400` | ❌ | A malformed request stays malformed. |
| **Workflow failed** | ❌ | **It ran.** Retrying runs it again. |

That last row is the one people get wrong. A workflow that executed and threw is
not a transient failure — it is a *result*. Re-running a workflow with side effects
is a decision for a human, or for n8n's own error workflow. It is emphatically not
a decision for an HTTP client with a retry loop.

The backoff is exponential with **full jitter**, capped, and it honours
`Retry-After`. Jitter is not decoration: a fleet of handlers recovering from an n8n
restart, all retrying on the same exponential schedule, retries *in lockstep* and
knocks it straight back over. There is a test that fails if jitter ever stops
actually spreading:

```go
if !sawShorter {
    t.Error("jitter never produced a shorter delay; the herd would stay synchronised")
}
```

## A 200 is not a success

n8n answers `200` and puts the error **in the body** when a workflow throws and a
"Respond to Webhook" node catches it:

```json
{"status": "error", "message": "node 'Draft' failed"}
```

If you check `resp.StatusCode == 200` and move on — and that is the default thing
to write — your platform will cheerfully report that it triggered workflows, for as
long as it takes someone to notice that no blog posts have appeared.

Nor is a `200` with an HTML body: that is a load balancer or an SSO login page
answering *instead of* n8n, and treating it as success means you are triggering
workflows into a void.

So the response is **validated, not trusted**: status, content type, size cap, JSON,
and then the body's own idea of whether it worked. Along with a bounded read,
because an engine that answers with a gigabyte — because it is broken, or because
it is not the engine — should not be able to take the process down, once per retry.

## The payload is not yours

The platform forwards GitHub's webhook payload, because a workflow will eventually
need a field nobody thought to model. And that payload is the one thing in this
whole flow that **this platform did not author**: it is large, nested, versioned by
someone else, and it occasionally carries credential-shaped things — an
installation access token on a GitHub App event, a client secret in a
poorly-configured integration.

"We are only passing it on" is exactly how secrets travel. Forwarded verbatim, they
land in **n8n's execution history** — which is a database, which gets backed up, and
which anyone with n8n UI access can read.

So the payload is sanitised on the way out: walk the JSON at any depth, replace the
values of credential-shaped keys with `[REDACTED BY PLATFORM]`, keep everything
else. The structure survives — a sanitiser that guts the payload is a sanitiser
nobody keeps enabled.

Verified against a real, running trigger rather than only in a unit test:

```
what n8n ACTUALLY received:
  payload: {'installation': {'access_token': '[REDACTED BY PLATFORM]', 'id': 42},
            'repository': {'full_name': 'teddynted/platform'},
            'sender': {'login': 'teddynted'}}
```

The token is gone. The `installation.id` — which a workflow genuinely needs — is
not.

And our *own* secret never leaves either. The token goes in a header, and never
into a log or an error, **including when n8n rejects it and echoes it back at us in
the response body.** A real gateway has done exactly that, which is why the
unauthorized path deliberately does not include the response body, and why there is
a test that fails if it ever does.

## Observability

Every execution logs at least twice, sharing a correlation ID derived from the
GitHub delivery:

```json
{"level":"INFO","msg":"workflow requested","correlationId":"push:delivery-abc-123","workflow":"blog-generator","repository":"teddynted/platform","commitSha":"deadbeef"}
{"level":"INFO","msg":"workflow completed","correlationId":"push:delivery-abc-123","status":"succeeded","executionId":"exec-local-1","attempts":1,"durationMs":4}
```

Because when a blog post fails to appear three hours after a merge, there is
exactly one question — *did the platform ask, and what did the engine say?* — and
it has to be answerable from the delivery ID alone.

Two details that are worth stealing:

**`errorKind` is the sentinel's name, not the message.** An alert built on
`"connection refused"` breaks the first time someone improves the wording. One
built on `errorKind: unavailable` does not.

**"We gave up" and "what we gave up on" are logged separately.** `ErrRetriesExhausted`
wraps its cause, so `errorKind` still reports the *timeout* while `retriesExhausted:
true` reports the surrender. An on-call engineer needs both, and collapsing them
into one field throws away the more useful one.

## Adding a workflow

This is the payoff, and it is the thing to judge the whole design on.

1. Draw the workflow in n8n. Add a Webhook trigger with Header Auth.
2. Check the idempotency key in an early node.
3. Add one entry to an environment variable:

```bash
N8N_WORKFLOWS=blog-generator=/webhook/blog,social-publisher=/webhook/social
```

4. Call it: `svc.Run(ctx, workflow.Request{Workflow: "social-publisher", Event: ev})`

**No recompile. No new client. No new retry policy.** Social publishing, release
notes, repository indexing, a video storyboard pipeline, a weekly digest on a cron
— each is a drawing in n8n and one line of configuration.

If adding a workflow ever requires touching Go code, the integration has failed at
its one job.

## Testing it

n8n is mocked with `httptest`, so the tests are hermetic and fast. What they
actually pin down is the stuff that will otherwise rot: that a retry reuses the same
idempotency key; that a `401` is **not** retried (with the call count asserted, so a
regression that hammers an auth failure fails the build); that a `200` with an error
body is a failure; that the token never reaches a log even when the server echoes it
back; that a live-looking `access_token` never reaches the wire.

But the unit tests only prove the client handles the responses I *imagined*. So
there is also a CLI, and it is not a demo:

```bash
go run ./cmd/workflow trigger blog-generator \
  --id delivery-abc-123 --repo teddynted/platform --sha deadbeef \
  --message "feat: add the thing" --payload event.json
```

It exists because four things actually break in production and **none of them can be
mocked**: the token is wrong, the webhook path does not exist, the workflow is not
*active*, and the network does not permit it. It is also how you replay an event by
hand — with its original `--id`, so a workflow that already did half the job does not
do that half twice. (The CLI warns you when it has to invent an ID, precisely because
that is the moment you are about to create a duplicate.)

Running it against a stub n8n immediately found a bug that every unit test had
missed — see below.

## Lessons learned

**The interesting problem was not "how do I call n8n".** It was "what happens when I
call n8n twice by accident". The HTTP client is the easy half; idempotency is the
half that decides whether the platform is trustworthy. And the uncomfortable part is
that the fix lives on the *other side* of the boundary, in a workflow this repository
does not own and cannot test. The best I can do is send a stable key, document it in
bold, and know it is the first thing to check when something happens twice.

**Go's `flag` package silently ignores flags after a positional argument.** `trigger
blog-generator --id x --sha y` parsed *zero* flags — no error, no warning, it just
quietly did the wrong thing with a generated ID and an empty commit. Every unit test
passed, because the tests call the library, not the CLI. It took thirty seconds
against a real stub to see it, and I would not have found it any other way. **A CLI
that ignores your input is worse than one that rejects it**, and the only defence is
to actually run the thing.

**"Only passing it on" is how secrets travel.** I nearly forwarded the GitHub payload
verbatim, because it is not the platform's secret and not the platform's payload.
That reasoning is exactly backwards: it is the platform's *pipe*, the credential
lands in *n8n's* database, and it is the platform's job not to widen the blast radius
of someone else's mistake.

**The seam earns its keep immediately, not eventually.** The usual argument for an
interface is "we might swap the implementation one day", which is a promise nobody
collects on. The real payoff arrived on day one: the `Service`'s entire test suite
runs against a fake engine with no HTTP at all, which means the platform's
orchestration logic is testable without pretending to be a web server. That would
have been worth it even if n8n were the last engine this platform ever used.

## What comes next

**Milestone 6 — OpenClaw**, the agent runtime that will actually *do* the work these
workflows orchestrate. n8n decides what happens and in what order; OpenClaw is what
holds the shell.

Two things this milestone deliberately did not build:

- **The webhook handler.** Something has to receive the GitHub event and call
  `Service.Run`. That is Milestone 12, and it is a Lambda that will do exactly what
  `cmd/workflow` does — which is why the CLI exists and stays: it is the reference
  caller, and the thing you run when you suspect the Lambda.
- **A response path.** Today a workflow is fire-and-forget: we trigger it, log that
  we did, and n8n gets on with it. When a workflow needs to report back — "the post is
  drafted, here is the PR" — it should publish to the platform's **own event bus**,
  which has been sitting there since Milestone 2 waiting for someone to use it. That
  is a nicer shape than a callback URL, and it is the seam that Milestone 3 built
  without knowing what for.

Until then: the platform can trigger a workflow, authenticate to it, survive it
being down, refuse to leak a token into it, and prove — from a GitHub delivery ID
alone — exactly what it asked for and what came back.

---

*The implementation is in [`internal/workflow`](../../internal/workflow) (the
engine-agnostic core) and [`internal/n8n`](../../internal/n8n) (the client), driven
by [`cmd/workflow`](../../cmd/workflow). The integration reference is
[WORKFLOWS.md](../../WORKFLOWS.md), and the diagrams are in
[n8n-diagrams.md](../architecture/n8n-diagrams.md).*
