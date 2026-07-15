# Automating AI Workflows with GitHub Webhooks

> **Milestone 12 — GitHub Webhook Automation.**
> This milestone gives the platform a front door: a Lambda GitHub calls when something happens
> in a repository, which verifies the request, filters it, and publishes a curated event onto
> the platform's EventBridge bus. It builds the endpoint, the signature verification, the event
> parsing and filtering, and the EventBridge routing to n8n. It does **not** build GitHub Apps,
> OAuth, GitHub Actions, or any workflow logic — those are later or belong to n8n. The code is in
> [`infra/lambda/internal/webhook`](../../infra/lambda/internal/webhook) and
> [`infra/cloudformation/09-webhook.yaml`](../../infra/cloudformation/09-webhook.yaml).

*Audience: engineers who have wired a webhook to "just call the thing" and later discovered the
webhook timing out, the thing running twice, and the signature check that was a `==` comparison.
This is the milestone about the boring parts that are the whole point.*

---

## Contents

- [A webhook is an event, not a remote procedure call](#a-webhook-is-an-event-not-a-remote-procedure-call)
- [The endpoint is public, and that is correct](#the-endpoint-is-public-and-that-is-correct)
- [Three ways to get signature verification wrong](#three-ways-to-get-signature-verification-wrong)
- [Verify, then parse — never the other way round](#verify-then-parse--never-the-other-way-round)
- [The curated event, and why not the raw payload](#the-curated-event-and-why-not-the-raw-payload)
- [EventBridge is the decoupling, and it earns its keep](#eventbridge-is-the-decoupling-and-it-earns-its-keep)
- [Filter early, and say why](#filter-early-and-say-why)
- [The status codes are an API, and only one of them retries](#the-status-codes-are-an-api-and-only-one-of-them-retries)
- [One correlation id, all the way down](#one-correlation-id-all-the-way-down)
- [Testing a webhook without GitHub or AWS](#testing-a-webhook-without-github-or-aws)
- [Lessons learned](#lessons-learned)
- [What comes next](#what-comes-next)

## A webhook is an event, not a remote procedure call

The tempting shape for "when someone pushes, draft the release notes" is: GitHub calls the
Lambda, the Lambda calls n8n, n8n runs the agent, the agent drafts the notes, and the Lambda
returns when it is all done. It is one straight line, and it is wrong in a way that does not show
up until production.

GitHub gives a webhook about ten seconds and retries the ones that time out. An agent run takes
minutes to hours. So the straight-line webhook times out, GitHub retries it, and now the agent is
running twice — two drafts, two pull requests, from one push. The failure is not a bug in any one
component; it is a category error about what a webhook *is*. A webhook is GitHub saying "this
happened", and the only correct response is to durably record that it happened and get out of the
way. Whatever reacts to it reacts on its own schedule, not GitHub's ten-second clock.

So this Lambda does exactly four things — verify, parse, filter, publish — and then returns. It
does not call n8n. It does not start an agent. It does not reach a model. It puts a small event on
a bus. That single decision is what makes the front door fast, retry-safe, and impossible to turn
into a double pull request, and everything else in the milestone follows from it.

## The endpoint is public, and that is correct

The webhook lives behind a Lambda Function URL with `AuthType: NONE`. The first time you write
that down it looks like a mistake — a public HTTPS endpoint with no authentication. It is not,
and the reason is worth stating plainly: **GitHub cannot authenticate with AWS IAM.** It does not
have credentials in your account; it cannot sign a SigV4 request. The only authentication GitHub
*can* perform is the one webhooks are built around — an HMAC signature over the request body,
using a secret only it and you know.

So "no AWS auth" is not "no auth". The auth is in the body, and putting an API Gateway in front to
add an authorizer would be adding a component to authenticate a request whose authentication is
already inside it. A Function URL is the smallest thing that terminates TLS and hands a Go
function the raw bytes, which is all this needs. The signature is the gate; the URL just opens the
door to the gate.

## Three ways to get signature verification wrong

Signature verification is the security boundary of the entire platform's front door, so it is the
most carefully tested code in the milestone. The happy path is four lines. The value is in the
ways it says no, and there are three classic ways to get it wrong:

**Comparing in variable time.** The obvious verification recomputes the HMAC and compares it to
the one in the header with `==` or `bytes.Equal`. Both return as soon as they find a differing
byte — and that early return leaks, through timing, how many leading bytes of a guess were
correct. Given enough attempts, an attacker forges a valid signature one byte at a time. The fix
is `hmac.Equal`, which compares in constant time, and it is a library call precisely so nobody
reimplements it. This is the single most important line in the package.

**Accepting the weaker signature.** GitHub sends two headers: `X-Hub-Signature` (SHA-1) and
`X-Hub-Signature-256` (SHA-256). SHA-1 is broken; GitHub sends it only for old integrations.
Accepting either means accepting the security of the weaker one, so the platform ignores the SHA-1
header entirely and verifies only SHA-256.

**Trusting a field in the request.** The verification's only inputs are the body and the secret.
Nothing the attacker controls selects the algorithm or the key — the header supplies a digest to
compare against, and that is all. Recomputing from scratch, rather than "checking" what the
request asserts, is what makes it a verification and not a formality.

## Verify, then parse — never the other way round

There is an ordering here that is easy to get backwards and expensive to debug. The signature is
over the **raw bytes GitHub sent**. So verification must happen before parsing, for two reasons
that point the same way.

The correctness reason: JSON round-trips are not byte-preserving. Parse the body and re-encode it
and you have changed the whitespace and the key order, and the signature over those new bytes will
not match — you would reject every legitimate delivery. The security reason is worse: parsing an
unverified body is doing an attacker's decoding for them, running your JSON parser over bytes you
have not yet established are from GitHub at all. So the handler verifies the raw body first, and
only touches the parser once the bytes are trusted. The test that pins this is the one that signs
a compact body and then tries to verify the same JSON with spaces added — it must fail, and if it
ever passes, someone has "helpfully" started verifying a re-marshalled payload.

## The curated event, and why not the raw payload

When the Lambda publishes to EventBridge, it does not forward GitHub's payload. It publishes a
small `GitHubEvent` with the fields the platform routes on: event, action, repository, branch,
sha, sender, delivery id, correlation id. A few hundred bytes, where GitHub's push payload is tens
of kilobytes. This is a deliberate decision with three independent justifications:

- **Decoupling.** Every consumer reads these stable fields and never learns GitHub's schema. When
  GitHub changes a payload — and it does — the change is absorbed here, in the parser, and no
  consumer notices. Forward the raw payload and you have coupled every downstream system to
  GitHub's JSON forever, and a field GitHub renames becomes an outage in n8n.
- **Redaction.** GitHub's payloads carry commit messages, file paths, and sometimes author
  emails. None of it is anything the platform routes on, and all of it is content that has no
  business being sprayed across every downstream system and log. What the parser never extracts is
  never published and never logged — the pruning *is* the redaction.
- **Size.** EventBridge caps an entry at 256 KB, and a large repository's push can approach it.
  The curated event never will.

This is the same instinct as the rest of the platform: a boundary should expose the platform's own
vocabulary, not the vendor's. The router exposes `llm.Capabilities`, not a Bedrock struct; the
webhook exposes a `GitHubEvent`, not a GitHub payload.

## EventBridge is the decoupling, and it earns its keep

It would be simpler, today, for the Lambda to call n8n directly. There is one consumer; why put a
bus between them? Because "there is one consumer" is a statement about today, and the bus is what
makes tomorrow cheap.

The event lands on the platform's EventBridge bus with `source: aiap.<env>.github`. An n8n rule
routes it onward. But adding a second consumer — an audit log of every event, a metrics
aggregator, a dead-letter queue, a replay mechanism — is a new rule on the same bus, and the
Lambda does not change, does not redeploy, does not even know. The producer publishes once; who
listens is a bus concern, not a webhook concern. That is the decoupling the milestone asks for,
and it is worth the one extra hop precisely because the hop is where the future extensibility
lives.

The one trap EventBridge sets, which the spot handler in this repository already learned and this
one inherits: `PutEvents` returns a 200 even when it accepted *none* of your entries. The per-entry
failures are in the response body, in a `FailedEntryCount` you have to read. Not reading it is the
canonical way to build an event pipeline that silently drops events and reports success. The
handler checks it, and returns a 500 when an entry was rejected, because a dropped event the
sender thinks succeeded is the worst outcome available.

## Filter early, and say why

Filtering happens in the Lambda, before anything is published — the cheapest place to not do work
is before you have started it. An ignored fork costs one `PutEvents` that never happens, rather
than a workflow that starts and then discovers it should not have.

The order of the rules is deliberate and visible: safe-first, then cheap-first. The
security-adjacent drops come first — a fork, an archived repository — because those are the ones
where processing the event would be actively *wrong*, not merely wasteful. An agent pointed at a
fork reads content the fork's owner controls, which is the Milestone 6 untrusted-content hazard
arriving through the front door. Only after those come the policy drops: is this a supported event,
is the repository on the allow-list, does the branch match. A reader going top to bottom sees the
dangerous things refused before the uninteresting ones.

And every drop carries a reason. "Ignored" in a log with no reason is a support ticket; "ignored:
repository acme/other is not on the allow-list" is self-service. The disposition is a small enum —
accepted, ignored, acknowledged — and not a boolean, because "we published it", "we deliberately
did nothing", and "that was a ping" are three different outcomes, and a webhook pipeline where you
cannot tell a dropped event from a processed one in the logs is one you cannot operate.

## The status codes are an API, and only one of them retries

The HTTP responses are a contract with GitHub's retry machinery, and getting them right is what
keeps a broken delivery from becoming a retry storm:

- **202** — authentic, wanted, published. Done.
- **200** — authentic, deliberately ignored by a filter, or a ping. Also done. GitHub does not need
  to know what the platform chose to do with a valid event; a 4xx for an event we simply do not
  care about would fill GitHub's delivery log with red that means nothing.
- **401** — signature missing or wrong. Refused, and terminal.
- **400** — authentic but malformed. Terminal.
- **500** — failed to publish a wanted event.

The load-bearing decision is that **only a publish failure returns 500**, because a 5xx is the only
response that makes GitHub redeliver. For a transient EventBridge failure that is exactly right:
the event is wanted and was not stored, the delivery id stays the same, so retrying is safe and
downstream idempotency handles the duplicate. But a 5xx for a bad signature or a malformed body
would make GitHub retry something that will fail identically forever — one broken delivery becomes
an endless retry loop. So those are 4xx: terminal by design. The status code is not decoration; it
is instructions to a retry engine you do not control.

## One correlation id, all the way down

Every delivery gets a correlation id of `<event>:<deliveryId>` — `push:a1b2c3…` — and it threads
the entire platform: webhook → EventBridge → n8n → agent → inference. It is the same id AGENTS.md
has used as its example since Milestone 6, and now it has a real origin.

The reason it is `<event>:<deliveryId>` and not a fresh UUID is idempotency. GitHub's delivery id
is stable across redeliveries — the same delivery, retried, carries the same id — so the
correlation id is stable too, and the agent derives its idempotency key from it. A webhook GitHub
sends twice produces one agent run, not two. A random id here would look perfectly fine and quietly
reintroduce the double-pull-request bug the whole architecture exists to prevent. When a pull
request appears in six months and nobody knows why, this id is the single thread back to the push
that caused it.

## Testing a webhook without GitHub or AWS

The whole handler is tested against a fake EventBridge, with signed sample payloads built the way
GitHub builds them — and the signing helper is the exact inverse of what the handler verifies, so
the test exercises the real verification path and not a parallel one. No GitHub, no AWS, runs in
milliseconds.

That is possible because every AWS dependency is behind an interface — the same `EventsAPI` seam
the spot handler in this module uses — and because verify, parse, and filter are three pure
functions with three separate tests. The signature test alone is a dozen cases: the correct
signature, the wrong secret, the missing header, the SHA-1 header offered as SHA-256, the re-encoded
body, the non-hex digest. Each is a way the front door must say no, and each is one line to assert
once the boundary is a function rather than a Lambda you can only test by deploying it.

## Lessons learned

- **A webhook is an event, not an RPC.** Record that it happened and return; never do the work on
  GitHub's ten-second clock, or a timeout becomes a retry becomes a double execution.
- **Public with a signature is the right auth for GitHub**, because IAM is an auth GitHub cannot
  perform. "No AWS auth" is not "no auth".
- **Verify in constant time, over the raw bytes, before parsing.** Each of those is a distinct bug
  waiting in the obvious implementation.
- **Publish your vocabulary, not the vendor's.** A curated event decouples every consumer from
  GitHub's schema and redacts repository content in the same stroke.
- **EventBridge earns its hop** by making the second consumer free — the producer never learns who
  listens.
- **Read `FailedEntryCount`,** or build a pipeline that drops events and reports success.
- **The status code is an API to a retry engine.** Only the retryable failure gets a 5xx; the rest
  are terminal, or one broken delivery loops forever.
- **The correlation id is the whole chain**, and it must be stable across redeliveries or
  idempotency breaks.

## What comes next

The bus is the extensibility, and the milestone's future is a list of consumers and producers that
plug into it without the Lambda changing:

- **A dead-letter queue and event replay** are a rule and a queue on the same bus — the events are
  already there, structured and correlated.
- **Additional event consumers** — an audit trail, a metrics aggregator (Milestone 15's
  observability) — are new rules, not new endpoints.
- **GitHub Apps and organization webhooks** replace how the request is authenticated and how many
  repositories it covers, behind the same verify-parse-filter-publish shape.
- **Multi-repository and multi-account** routing is richer EventBridge rules and, eventually,
  cross-account bus policies — the curated event already carries the project and environment to
  route on.

A push in, a verified and filtered event on a durable bus, and a fast, retry-safe answer to GitHub
— with everything that reacts to it doing so on its own time, behind a seam that makes the next
consumer free. That is what an event-driven front door is for, and the engineering was almost
entirely in the parts that are invisible when they work: the constant-time compare, the verify
order, the one status code that retries, the id that stays stable.
