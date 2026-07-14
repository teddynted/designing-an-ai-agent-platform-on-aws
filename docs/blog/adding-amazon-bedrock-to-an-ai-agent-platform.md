# Adding Amazon Bedrock to an AI Agent Platform

> **Milestone 8 — Amazon Bedrock Integration.**
> This milestone adds Bedrock as a second implementation of the provider interface
> [Milestone 7](running-local-llms-with-ollama-on-aws.md) built, so the platform can
> switch between a local model and a managed one **by configuration, not by code**. It
> does not build a router (M10), a Knowledge Base, an Agent, or a Guardrail. The code is
> in [`internal/bedrock`](../../internal/bedrock) and
> [`internal/providers`](../../internal/providers).

*Audience: engineers who have written an interface with one implementation and told
themselves it was an abstraction. This is the milestone where I found out whether mine
was.*

---

## Contents

- [The point of this milestone is not Bedrock](#the-point-of-this-milestone-is-not-bedrock)
- [The abstraction held. The vocabulary did not](#the-abstraction-held-the-vocabulary-did-not)
- [There is no API key, and that is the best part](#there-is-no-api-key-and-that-is-the-best-part)
- [The permission error that is two errors](#the-permission-error-that-is-two-errors)
- [Throttling is not an outage](#throttling-is-not-an-outage)
- [The retry layer I did not know I had](#the-retry-layer-i-did-not-know-i-had)
- [Converse, or a bug per model family](#converse-or-a-bug-per-model-family)
- [The wildcard that bills you](#the-wildcard-that-bills-you)
- [The first dependency](#the-first-dependency)
- [Testing a cloud API without touching the cloud](#testing-a-cloud-api-without-touching-the-cloud)
- [The seam, enforced by a test rather than a promise](#the-seam-enforced-by-a-test-rather-than-a-promise)
- [Lessons learned](#lessons-learned)
- [What comes next](#what-comes-next)

## The point of this milestone is not Bedrock

Bedrock is a good API. Integrating it is not a hard problem, and if that were all this
milestone did, it would not be worth writing about.

The point is this: **last milestone I wrote an interface with one implementation and
claimed it was an abstraction.** That claim is free to make and expensive to be wrong
about, and there is exactly one way to find out — write the second implementation and see
what breaks.

So this is a report on what broke. The headline is that most of it didn't, and the
interesting part is the bit that did.

Here is the whole user-facing result of the milestone:

```bash
LLM_PROVIDER=ollama    # a model on hardware you own; the prompt does not leave
LLM_PROVIDER=bedrock   # a managed foundation model; the prompt leaves, and is billed
```

Same CLI, same logs, same retry semantics, same errors. No caller changed. That is what I
wanted it to say, and I am aware that it is exactly what someone would say whether or not
it was true — so the rest of this post is the evidence.

## The abstraction held. The vocabulary did not

The interface did not change:

```go
type Provider interface {
	Name() string
	Capabilities() Capabilities
	Models(ctx context.Context) ([]Model, error)
	Generate(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request, sink Sink) (Response, error)
}
```

Not one line. `llm.Service` — which does the validating, the retrying, the logging and the
redacting — did not change either. Bedrock was an *implementation*, and that is a genuinely
good outcome, better than I expected.

**The error vocabulary was a different story, and it is the most useful thing I learned all
milestone.**

Milestone 7 defined a provider-agnostic set of errors. I was quite pleased with it:
`ErrUnavailable`, `ErrTimeout`, `ErrStalled`, `ErrStreamBroken`, `ErrModelNotFound`,
`ErrContextExceeded`, `ErrEmptyCompletion`, `ErrInvalidResponse`. Provider-agnostic! Any
provider could produce those!

Then I wrote the second provider, and within an hour I needed three words I did not have:

```go
ErrUnauthorized      // the provider rejected our credentials
ErrModelAccessDenied // the model EXISTS, and this account may not use it
ErrThrottled         // the provider is fine; we are over our quota
```

Look at why each one was missing, because the pattern is the whole lesson:

| I had no word for | Because Ollama… |
| --- | --- |
| rejected credentials | **has no authentication.** It is a laptop tool. There is nobody to reject you. |
| a model you may not use | **has no entitlements.** If it is pulled, it is yours. |
| being throttled | **has no quotas.** It is a process. It gets slow; it does not say *no*. |

My "provider-agnostic" vocabulary was a careful description of **Ollama**, wearing an
interface's clothes. Every single one of those three failures exists on *every hosted
provider that has ever been built*. I had not omitted them because they were exotic. I had
omitted them because **my sample size was one, and a sample of one cannot tell you which of
its properties are essential and which are incidental.**

> You cannot design an abstraction from a single implementation. You can only design a
> description of it.

The consoling part: this is exactly why you write the second implementation early. Finding
this at two providers cost an afternoon. Finding it at four — with a router on top, and
callers that had quietly started matching on error strings because the sentinel they needed
did not exist — is the kind of refactor that gets scheduled and then doesn't happen.

And there is a second, subtler check I made myself do. Bedrock has failures that are
genuinely *Bedrock's*: a model that exists but has no on-demand throughput in your region,
an inference profile you were supposed to use instead. It would have been very easy, and
very wrong, to add `ErrInferenceProfileRequired` to the shared vocabulary. That noun would
be meaningless to Ollama, meaningless to Claude, and would have leaked AWS into the one
package that must not know AWS exists.

So those map onto the nouns that already exist — `ErrModelNotFound`, `ErrContextExceeded` —
and the *message* carries the AWS-specific fix:

```
error: no such model: "anthropic.claude-3-5-sonnet-20241022-v2:0" is not available for
       on-demand invocation in us-east-1. Newer models require a cross-region inference
       profile — try the "us." prefix: us.anthropic.claude-3-5-sonnet-20241022-v2:0
```

**The vocabulary grew by what is true of hosted providers in general. It did not grow by
what is true of Bedrock.** That distinction is the difference between an abstraction and a
union of its implementations, and it is a decision you have to make three or four times per
milestone, every time with the lazy option looking extremely reasonable.

## There is no API key, and that is the best part

Here is the entire Bedrock credential configuration:

```json
{ "credentials": "(AWS IAM — resolved by the SDK's default chain; no static key)" }
```

That is not a redaction. There is nothing behind it. There is no API key, no secret, no
token, nothing in Secrets Manager, nothing in the CloudFormation template, nothing in the
environment.

The SDK's default credential chain walks environment variables, then the shared config
file, then — on the EC2 instance where this actually runs — the **instance role**, via the
metadata service. What comes back is **temporary credentials that AWS rotates and that
expire on their own**.

I want to dwell on this, because it is the single largest practical difference between
integrating an AWS service and integrating a SaaS, and it is easy to be blasé about it:

> **A credential that does not exist cannot be leaked, committed, pushed to a public
> repository, pasted into a Slack channel, or rotated three months late.**

Compare it with `OLLAMA_TOKEN` from last milestone — which exists only for the case where a
proxy sits in front of Ollama and *does* authenticate — and which the config nonetheless has
to work quite hard to keep out of logs, out of error messages, and off the wire when the
endpoint is plain HTTP on a public host. That is real work, and every line of it is work
that IAM makes unnecessary.

And locally? `aws sso login`. The same temporary credentials, through the same chain, down
the same code path. **There is no development mode that authenticates differently** — because
a development mode that authenticates differently is a production incident that has not
happened yet.

## The permission error that is two errors

The first thing that went wrong when I pointed this at a real account was an
`AccessDeniedException`, and I spent a while re-reading an IAM policy that was correct all
along.

Bedrock has **two** permission gates. They live in completely different places, they are
managed by completely different people, and — this is the bit that costs you the afternoon —
**they throw the same exception.**

| | What it asks | Where it lives |
| --- | --- | --- |
| **1. IAM** | May this *role* call `InvokeModel` on this model? | Your IAM policy |
| **2. Model access** | May this *account* use Claude **at all**? | Bedrock console → *Model access* |

The second one is not an IAM concept. It is an **entitlement**: a per-account, per-model
request you make once, in a console, and which somebody has to approve. Your IAM policy can
be flawless and your call will still fail, with a message about access, which sends you
straight back to the policy.

So the platform's error names **both**, because AWS will not tell you which:

```
error: the account is not granted access to this model: You don't have access to the model
       with the specified model ID. Either the IAM role lacks bedrock:InvokeModel for this
       model, or the account has not been granted access to it in the Bedrock console
       (Model access) — the two are different, and both are required.
```

This is the whole job of an integration layer, and it is worth saying explicitly. It is not
to pass the upstream error through faithfully. **A faithful error that sends you to the
wrong place is worse than useless — it costs you the time you would have spent thinking.**
The integration knows something the API does not: it knows the two most likely reasons you
are seeing this, and it can say them.

## Throttling is not an outage

The defining failure mode of a hosted provider, and I gave it its own error kind rather than
folding it into `ErrUnavailable`. That looks like fussiness. It is not.

> `ErrThrottled` means **the provider is fine, and you are over your quota.**

If throttling reported as `ErrUnavailable`, then somewhere down the line an alarm called
*"Bedrock is down"* fires **every time the platform gets busy**. Which is exactly when it is
working. Which is exactly when you least want to be woken up to go and look at a healthy
service.

The two conditions want opposite responses from a human, and that is the test for whether two
things deserve different names:

- **Unavailable** → something is broken. Go and look at it.
- **Throttled** → nothing is broken. This is a graph of **demand**. Ask for a quota increase,
  or send the cheap work somewhere cheaper.

Both are retried with backoff and jitter, because both are transient. But only one of them
should ever wake anybody up.

(Bedrock quotas on a new account are lower than most people expect, and they are per-model,
per-region, measured in **tokens** per minute as well as requests. You will meet this.)

## The retry layer I did not know I had

This one is a genuine bug I shipped into my own working tree and caught because a test
counted calls.

The AWS SDK retries throttled requests **by default**. It is a good default. It is also,
when your integration *also* retries throttled requests, a quiet disaster:

```
    3 attempts (mine)  ×  3 attempts (the SDK's)  =  9 calls to Bedrock
```

Three problems, in ascending order of nastiness:

1. **Nine calls.** On a per-token API. Billed.
2. **The log line is a lie.** It says `attempts: 3`. There were nine. Every number derived
   from it — duration, cost, throttle rate — is wrong, and wrong in a way no one will catch,
   because it is *internally consistent*.
3. **The two layers hide each other.** The SDK's retries are invisible from where I stand.
   Mine are invisible to it. Neither can be reasoned about while the other is running.

**Two retry layers do not add. They multiply, and they conceal one another.** So the SDK's
are off:

```go
awsconfig.WithRetryMaxAttempts(1)   // this integration owns the retry policy.
```

The rule I have taken from this, and I think it generalises well past AWS: **exactly one
layer in a stack may retry, and it must be the layer that knows what the operation costs.**
Every SDK you adopt has an opinion about retries. Most of them are reasonable in isolation.
None of them know that they are the third one in the chain.

## Converse, or a bug per model family

Bedrock offers two ways to call a model, and picking the wrong one shapes the next year of
your codebase.

**`InvokeModel`** takes a model-specific JSON body. Anthropic's schema is not Meta's, is not
Amazon's — different field names, different message shapes, different places to put the
system prompt, different ways to say "stop". Build a provider on it and you have written a
small adapter per model family, and therefore a bug per model family, and therefore a reason
to never try a new model.

**`Converse`** is one messages-shaped API across all of them. Swapping `BEDROCK_MODEL_ID` from
Claude to Llama to Nova is genuinely just a different string.

The choice is obvious once stated, which is why it is worth stating: I nearly used
`InvokeModel` because every Bedrock example I found first used `InvokeModel`. The `Converse`
API is newer and the internet has not caught up. **The most-copied example is not the same
thing as the right answer**, and on AWS the gap between those two is frequently a couple of
years wide.

One wrinkle that cost me ten minutes and will cost you the same: **the system prompt is its
own top-level field.** Send it as a message with `role: "system"`, the way every OpenAI-shaped
API has trained you to, and Bedrock rejects the request.

## The wildcard that bills you

The IAM policy is the part of this milestone I would most want a reviewer to look at.

```yaml
Resource: "*"     # ← no
```

Almost every AWS wildcard you leave lying around is a *security* problem: it lets someone read
something, or delete something. Bad, but bounded.

**`bedrock:InvokeModel` on `*` is a wildcard that bills you.** It is permission to invoke the
most expensive model in the catalogue, as many times as an attacker likes, and the cost of
being wrong is not measured in exposed rows but in **dollars per minute**. Bedrock is an API
that converts permission directly into money, and I cannot think of another AWS action where
that is quite so literally true.

So the policy names the models, and grants **nothing** by default:

```yaml
BedrockModelArns:
  Type: CommaDelimitedList
  Default: ""          # ← no Bedrock access at all, until you say otherwise
```

Two things I got wrong on the way there, both worth repeating because both are silent:

**CloudFormation cannot map a list of model IDs onto a list of ARNs.** My first version built
the ARN with `!Select [0, !Ref BedrockModelIds]` — which cheerfully granted access to the
**first model in the list and no others**, and would have deployed perfectly, and would have
failed at runtime for model number two with an access-denied error that I would then have gone
and debugged in the *application*. `cfn-lint` does not catch that; it is valid template that
means the wrong thing. The fix was to make the operator pass full ARNs, which is slightly more
verbose and *exactly* what it says.

**An inference profile is a different resource from the model it fronts.** Newer models are
only available on-demand through a cross-region profile (the `us.`-prefixed ones), and invoking
through a profile requires **both** ARNs in the policy. Grant only the model and you get a
`ValidationException` that does not mention permissions at all.

And there is a region condition, because a model ARN alone still lets a stolen credential invoke
in any region AWS has:

```yaml
Condition:
  StringEquals:
    aws:RequestedRegion: [us-east-1]
```

## The first dependency

A detail I did not expect to be writing about. Here is `go.mod` as it stood after seven
milestones — infrastructure, Spot handling, AMIs, an n8n integration, an agent integration
and a local-inference integration:

```go
module github.com/teddynted/designing-an-ai-agent-platform-on-aws

go 1.25
```

That is the whole file. **Not one third-party dependency.** Not by ideology — it simply
never came up. HTTP, JSON, NDJSON streaming, retries with jitter, structured logging: the
standard library does all of it, and `internal/httpx` is about two hundred lines.

Bedrock is the first thing in this platform I could not sensibly do that way. SigV4
request signing, the credential-chain walk, and the binary **event-stream** framing that
`ConverseStream` uses are three things you do not hand-roll — not because it is hard, but
because a subtly wrong signature implementation fails in ways you will debug for a week.
So the AWS SDK went in, and it brought seventeen modules with it.

I want to be honest that this is a real cost and not pretend the SDK is free:

- **Seventeen modules** for one API, which is how the AWS SDK is built (a module per
  service, plus the shared machinery). They are all `github.com/aws/*`.
- It brought its **own retry policy**, which I then had to switch off — see above. The
  dependency did not just add code, it added *behaviour I had to go and find*.

What kept it bounded is the seam. `internal/bedrock` is the **only** package in the
repository that imports `aws-sdk-go-v2`, and the provider talks to it through three narrow
interfaces. The SDK is a detail of one package, not a fact about the platform — and if
Bedrock were removed tomorrow, `go.mod` would go back to being empty.

**A dependency you can delete by deleting one package is a dependency you still control.**
That is what the architecture test is really protecting.

## Testing a cloud API without touching the cloud

Not one test in this milestone talks to AWS. Not with a credential, not with a cheap model, not
"only in CI".

A unit test that calls Bedrock is a test that fails on an aeroplane, costs money on every push,
turns a code review into a permissions ticket, and goes red because *someone else's* quota was
exhausted. It is not a unit test. It is a monitoring check wearing a unit test's badge.

The provider takes the AWS operations it needs as **narrow interfaces** — three methods, not the
SDK's whole surface:

```go
type converseAPI interface {
	Converse(context.Context, *bedrockruntime.ConverseInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(context.Context, *bedrockruntime.ConverseStreamInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}
```

The real client satisfies it. So does a fake that returns a real `smithy.OperationError` wrapping
a real `types.ThrottlingException` — which means **the tests exercise the error mapping**, which
is where the entire logic of a cloud integration actually lives, and they do it in microseconds
with no network.

The test I am most glad I wrote counts calls:

> An `AccessDeniedException` is **not retried**. Neither is `ResourceNotFound`, nor an oversized
> prompt.

Retrying a permission error three times is a bug that is invisible in every log — it *works*, it
just fails three times as slowly and three times as expensively — and the only way to catch it is
to assert on the number of times you called the API. Nine calls where you expected three is not a
thing you notice. It is a thing you discover in the bill.

## The seam, enforced by a test rather than a promise

The rule that makes "switch providers by configuration" true rather than aspirational:

- `internal/llm` may not import a vendor.
- `internal/ollama` and `internal/bedrock` may not import each other.
- **Exactly one package** — `internal/providers`, the factory — may import both, and nothing
  depends on it.

I wrote all of that down last milestone. Writing it down does nothing.

So it is a test now. `internal/architecture_test.go` walks the import graph with `go/build`, and
it **fails the build** if any of it stops being true. I proved it works by adding a violating
import and watching it go red — which I mention because the previous version of this check was a
shell one-liner that, thanks to a quoting bug, tested nothing at all and printed `ok`. A green
check that cannot fail is worse than no check, because you stop looking.

The factory is deliberately *not* a router. It picks one provider at start-up from
`LLM_PROVIDER`, and gets out of the way. Choosing per-request is Milestone 10, and it will
implement `llm.Provider` itself and sit exactly where a single provider sits today. Nothing above
it will notice — which is a sentence I can now write with some confidence, having just done the
smaller version of it.

## Lessons learned

**You cannot design an abstraction from one implementation.** You can only describe that
implementation in general-sounding language. The second implementation is the first honest audit
of the first one, and the cheapest time to run it is now, not at provider four.

**A vocabulary should grow by what is true of the category, not of the newcomer.**
`ErrThrottled` belongs — every hosted provider throttles. `ErrInferenceProfileRequired` does not
— that is AWS leaking through a seam that exists to stop it. Making that call correctly, several
times a milestone, is most of what keeps an abstraction from decaying into a union of its
implementations.

**Exactly one layer may retry, and it must be the one that knows what the operation costs.**
Every SDK has a retry policy. Yours has a retry policy. They multiply, they lie to your logs, and
they hide each other.

**Two errors that want different human responses need different names.** Throttled is not
unavailable. One is a graph; the other is a page.

**The best credential is the one that does not exist.** IAM's temporary credentials removed an
entire category of work — storage, rotation, redaction, leak response — that the Ollama
integration had to do properly for a token it *usually doesn't even need*.

**A wildcard on an inference API is a wildcard that bills you.** Name the models.

**`cfn-lint` will not tell you your template means the wrong thing.** It told me a parameter was
unused. It did not tell me I was granting exactly one of the four models I had listed.

## What comes next

**Milestone 9 — Claude.** The third provider, and the third *authentication model* in three
providers: none, then IAM, then a real API key that must be stored, injected and rotated. I fully
expect it to find something else my vocabulary is missing — and if it doesn't, that will be the
first real evidence that the vocabulary has converged.

**Milestone 10 — the router.** The reason all of this exists. Choose per request: cheap-and-local
for a summary, frontier-and-hosted for reasoning, and **local-only** for a private repository whose
source may not leave the network. It routes on `Capabilities` — `Local`, and cost — and both of
those fields are only *true* today because Milestone 8 forced them to be. `Local: true` was a
lonely tautology until a provider existed for which it was false, and cost was `0` until a provider
existed that charges. **A router built on a `Capabilities` that only one provider had ever filled
in would be routing on fiction.**

And then the one that ties the whole platform together: **Bedrock as the fallback when the Spot GPU
is [interrupted](../../infra/SPOT.md).** Two minutes' notice, the local model goes away, and the
platform keeps answering — more expensively, and without anybody being paged. That is the shape
Milestone 3 built for, and it is now, finally, a thing I could actually wire up.

---

*Code: [`internal/bedrock`](../../internal/bedrock), [`internal/providers`](../../internal/providers).
Reference: [INFERENCE.md](../../INFERENCE.md). Diagrams:
[Bedrock diagrams](../architecture/bedrock-diagrams.md).*
