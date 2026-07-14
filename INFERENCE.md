# Inference — Two Providers Behind One Interface

The platform runs its own inference against **either** a self-hosted **Ollama**
(Milestone 7) **or** **Amazon Bedrock** (Milestone 8), chosen by one environment
variable and nothing else:

```bash
LLM_PROVIDER=ollama    # a model on hardware you own; the prompt does not leave
LLM_PROVIDER=bedrock   # a managed foundation model; the prompt leaves, and is billed
```

Same interface, same CLI, same logs, same retry semantics. Claude (M9) is the third,
and a router that chooses **per request** is M10.

- **Generate** — one prompt, one completion
- **Stream** — tokens as they arrive, because a generation you cannot watch is
  indistinguishable from a hang
- **Discover** — what models are actually available to you
- **Refuse** — a prompt that does not fit, before the model silently truncates it

> **This repository deploys neither.** Ollama's instance, GPU and models belong to
> `ollama-on-aws`; Bedrock is AWS's to run. This repository owns *the provider
> abstraction that calls them*.

The *why* is in the blog posts:
[Running Local LLMs with Ollama on AWS](docs/blog/running-local-llms-with-ollama-on-aws.md)
and
[Adding Amazon Bedrock to an AI Agent Platform](docs/blog/adding-amazon-bedrock-to-an-ai-agent-platform.md).

## Contents

- [Wait — Milestone 6 said the platform calls no model](#wait--milestone-6-said-the-platform-calls-no-model)
- [Who does what](#who-does-what)
- [What a second provider did to the abstraction](#what-a-second-provider-did-to-the-abstraction)
- [Choosing a provider](#choosing-a-provider)
- [Local or hosted](#local-or-hosted)
- [The provider abstraction](#the-provider-abstraction)
- [Authentication: there is no credential](#authentication-there-is-no-credential)
- [The two permissions Bedrock needs](#the-two-permissions-bedrock-needs)
- [Throttling is not an outage](#throttling-is-not-an-outage)
- [Streaming, and the timeout that actually works](#streaming-and-the-timeout-that-actually-works)
- [A retry is safe here — with one exception](#a-retry-is-safe-here--with-one-exception)
- [The silent-truncation trap](#the-silent-truncation-trap)
- [Configuration](#configuration)
- [Example request and response](#example-request-and-response)
- [Errors](#errors)
- [Observability](#observability)
- [Security](#security)
- [Local development](#local-development)
- [Testing](#testing)
- [Supported models](#supported-models)
- [Future providers](#future-providers)
- [Troubleshooting](#troubleshooting)

## Wait — Milestone 6 said the platform calls no model

It did, in bold, in several places. That statement was true, and it is worth being
precise rather than quietly widening it.

**The agent's inference is still the agent's.** When OpenClaw reasons, it calls its
own model, behind its own boundary. Nothing here is in that path. Swapping the
agent's model is still a change in `openclaw-on-aws` that this repository does not
notice.

**But not everything worth doing with a model needs an agent.** "Summarise this
diff." "Write release notes from these commits." "Classify this event." These are
single-shot: one prompt, one completion, no shell, no tools, no reasoning loop.
Routing them through an agent means paying for an errand when what you wanted was a
function call.

So there are two consumers of inference, and they are different:

| | Calls the model | Shape |
| --- | --- | --- |
| **The agent plane** (M6) | OpenClaw does, itself | An errand: tools, a loop, minutes |
| **The platform** (M7, M8) | The platform does | A function call: prompt in, tokens out |

This is the *inference plane* the architecture has had on paper since Milestone 1,
and the provider abstraction is listed under "what this repository owns" in the
[repository scope](README.md#repository-scope).

## Who does what

| | Owns |
| --- | --- |
| **n8n** (M5) | **Orchestration.** What happens, in what order, and the waiting. |
| **OpenClaw** (M6) | **Agentic execution.** Open-ended tasks with tools. Calls its own model. |
| **`internal/llm`** (M7) | **The platform's own inference**, and the provider abstraction. |
| **Ollama** (M7) | **Local model serving.** Deployed by `ollama-on-aws`. |
| **Amazon Bedrock** (M8) | **Managed model serving.** Deployed by nobody; it is an API. |
| **`internal/providers`** (M8) | Building the configured provider. The one package that knows both exist. |
| **Claude** (M9) | A third provider, behind the same interface. |
| **The router** (M10) | Choosing between them **per request**. |

## What a second provider did to the abstraction

This is the honest headline of Milestone 8, and it is more useful than "it worked".

**The interface held.** `Provider` did not change. `Service` did not change. No
caller changed. Bedrock was an *implementation*, not a rewrite — which is the entire
claim M7 made, and it is now tested rather than asserted.

**The error vocabulary did not hold**, and could not have:

| M7 knew about | Because Ollama… |
| --- | --- |
| `ErrUnavailable`, `ErrTimeout`, `ErrStalled` | is a process that can be down or slow |
| `ErrModelNotFound` | has models you either pulled or didn't |
| `ErrContextExceeded`, `ErrEmptyCompletion` | is a model, like any other |

Ollama has **no authentication**, so it cannot reject your credentials. It has **no
quotas**, so it cannot throttle you. It has **no entitlements**, so a model that
exists is a model you may use. Milestone 7 therefore had no word for any of those,
and none of them are Bedrock exotica — *every* hosted provider has all three.

So Milestone 8 added three words to `internal/llm`:

```go
ErrUnauthorized      // the provider rejected our credentials
ErrModelAccessDenied // the model EXISTS, and this account may not use it
ErrThrottled         // the provider is fine; we are over our quota
```

> **You cannot design an abstraction from a sample of one.** M7's interface was
> right, and M7's *vocabulary* was a description of Ollama wearing an interface's
> clothes. The second implementation is the first honest test of the first one — and
> the cheapest time to discover this was at two providers, not at four.

Note also what did **not** need adding: no `ErrRegionUnsupported`, no
`ErrInferenceProfileRequired`, no Bedrock-shaped nouns leaking upward. Those are real
Bedrock failures, and they map onto `ErrModelNotFound` and `ErrContextExceeded` with
a message that explains the AWS-specific fix. The vocabulary grew by what is **true
of hosted providers in general**, not by what is true of Bedrock.

## Choosing a provider

`internal/providers` is a factory, and it is the **only** package in the repository
that imports two vendors:

```go
provider, info, err := providers.New(ctx, log)   // reads LLM_PROVIDER
svc := llm.NewService(provider, log)             // everything above here is unchanged
```

That constraint is enforced mechanically, not by convention —
`internal/architecture_test.go` fails the build if `internal/llm` ever imports a
vendor, if `ollama` and `bedrock` ever import each other, or if any package other
than the factory reaches for more than one of them.

**The default is `ollama`**, deliberately: a platform that ships somebody's source
code to a hosted service because nobody set an environment variable has made that
choice on their behalf, and made it badly.

## Local or hosted

The usual arguments are cost and latency. Neither is the real one.

**The real one is that the prompt does not leave.** This platform's prompts are full
of *somebody's source code* — diffs, files, commit messages, and occasionally
something nobody meant to commit. A hosted provider means all of that crosses the
internet to a third party, and no amount of TLS changes the fact that you have sent
them your source.

For a public repository, fine. For a private one, it is the whole question — and it
is why `Capabilities.Local` exists as a first-class field that the M10 router will
route on.

The other trade-offs, honestly:

| | **Ollama** (M7) | **Bedrock** (M8) |
| --- | --- | --- |
| **The prompt leaves** | ❌ no | ✅ yes — to AWS, in your account's region |
| **Cost** | The instance, paid whether you use it or not | Per token, paid only when you do — **and it never stops scaling** |
| **Idle cost** | The full instance | **Zero** |
| **Quality** | A 3B–13B model. Good at summarising; not at reasoning | Frontier models |
| **Speed** | GPU: fast. **CPU: single-digit tokens/sec** | Fast, consistently |
| **Auth** | None (it's a laptop tool) | **IAM.** No credential to store or rotate |
| **Fails by** | Being down, being slow, swapping | **Throttling**, and permission |
| **Availability** | Yours to keep up | AWS's problem — within your quota |
| **Capacity** | One box. Concurrency is a queue | Elastic — until the quota, which is a wall |
| **Runs on Spot** | Yes, and Spot [takes it away](infra/SPOT.md) | Not applicable — nothing to interrupt |

**Local models are not small versions of big models.** A 3B model is genuinely good
at "summarise this diff in three bullets" and genuinely bad at "reason about whether
this architecture is sound". Sending it the second kind of work produces something
confident and wrong, which is worse than a refusal. That asymmetry is the entire
reason Milestone 10 exists — and now that there are two providers with genuinely
different `Capabilities`, it has something to route *on*.

**The cost inversion is worth stating plainly.** Ollama's marginal token is free and
its idle hour is not; Bedrock's idle hour is free and its marginal token is not. The
right answer is not one of them — it is knowing which of those two bills you are
currently signing, which is what `estimatedCostUsd` in the logs is for.

## The provider abstraction

```go
type Provider interface {
	Name() string
	Capabilities() Capabilities
	Models(ctx context.Context) ([]Model, error)
	Generate(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request, sink Sink) (Response, error)
}
```

The usual justification for an interface — *"we might swap it one day"* — is a
promise nobody collects on. This one is different: **the swap is the roadmap.** A
provider abstraction retro-fitted at Milestone 10, on top of three call sites that
each learned Ollama's JSON, is a rewrite. Added at M7, it is an interface with one
implementation; at M8, it is an interface with two, and the second one cost no caller
anything.

`Capabilities` is deliberately small — the facts a router needs, and nothing else:

```go
type Capabilities struct {
	Local                    bool  // does the prompt LEAVE? the field that matters
	Streaming                bool
	MaxContextTokens         int
	CostPer1MInputTokensUSD  float64  // 0 for Ollama: the cost is the instance
	CostPer1MOutputTokensUSD float64
}
```

Two providers now answer these differently, which is the point:

| | Ollama | Bedrock |
| --- | --- | --- |
| `Local` | `true` | `false` |
| `CostPer1M…` | `0` — the cost is the instance, not the token | Real money, per token |

**The price is configuration, not a constant.** `BEDROCK_INPUT_COST_PER_1M_USD` is an
environment variable rather than a table of AWS prices baked into this repository,
because a price table in source is a price table that is quietly wrong within a year
— and M10 will make routing decisions on it. A router that routes on a stale price
routes confidently to the wrong provider.

## Authentication: there is no credential

The Bedrock config has no API key, no secret, no token. Deliberately, and it is the
single best thing about integrating an AWS service rather than a SaaS one:

```json
{
  "modelId": "anthropic.claude-3-5-haiku-20241022-v1:0",
  "region": "us-east-1",
  "credentials": "(AWS IAM — resolved by the SDK's default chain; no static key)"
}
```

The SDK's default credential chain resolves, in order: environment variables, the
shared config file, and — on the instance where this actually runs — the **EC2
instance role**, via IMDS. Those credentials are **temporary**, rotated by AWS, and
never written down anywhere.

**A credential that does not exist cannot be leaked, committed, or rotated late.**
There is no secret in the CloudFormation template, none in Secrets Manager, none in
the environment, and nothing for a compromised process to read out of a config file.
Compare that to `OLLAMA_TOKEN`, which exists only because a proxy might need one — and
which the config has to work quite hard to keep out of the logs.

Locally, `aws sso login` produces the same temporary credentials, and the code path is
identical. There is no "development mode" that authenticates differently, because a
development mode that authenticates differently is a production incident waiting for
its moment.

## The two permissions Bedrock needs

The most common Bedrock failure has nothing to do with your code, and the error message
is unhelpful enough that it is worth spelling out. **There are two separate gates**, and
passing the first tells you nothing about the second:

| | What it is | Where it lives | Symptom if missing |
| --- | --- | --- | --- |
| **1. IAM permission** | *May this role call `bedrock:InvokeModel`?* | Your IAM policy | `AccessDeniedException` |
| **2. Model access** | *May this **account** use Claude at all?* | Bedrock console → **Model access** | `AccessDeniedException` |

They produce **the same exception**. So the platform's error names both, because a
message that says only "access denied" sends you to re-read an IAM policy that was
correct all along:

```
error: the account is not granted access to this model: You don't have access to the
       model with the specified model ID. Either the IAM role lacks bedrock:InvokeModel
       for this model, or the account has not been granted access to it in the Bedrock
       console (Model access) — the two are different, and both are required.
```

The required IAM policy, scoped to the models you actually use:

```yaml
- Sid: InvokeNamedModels
  Effect: Allow
  Action:
    - bedrock:InvokeModel
    - bedrock:InvokeModelWithResponseStream
  Resource:                      # the named models, and ONLY the named models
    - arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-5-haiku-20241022-v1:0
    - arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-3-5-sonnet-20241022-v2:0
  Condition:
    StringEquals:
      aws:RequestedRegion: [us-east-1]
```

Three things about that, each of which is a bug someone has shipped:

**`Resource: "*"` is a licence to invoke the most expensive model in the catalogue.**
Bedrock is an API that turns permission into money. An instance permitted to call any
model is an instance whose compromise is measured in dollars per minute, and it is the
one AWS wildcard that bills you for being wrong. It is
[`infra/cloudformation/02-iam.yaml`](infra/cloudformation/02-iam.yaml) and it is
**empty by default** — `BedrockModelArns` grants nothing until you name something.

**An inference profile is a different resource from the model it fronts.** Newer models
are only available on-demand through a cross-region profile (`us.anthropic.…`), and
invoking through one requires **both** ARNs. Grant only the model and the call fails
with a `ValidationException` that never mentions permissions.

**`bedrock:ListFoundationModels` is a separate, read-only permission**, on `*` — because
"what can I invoke?" is a question about the catalogue, and you cannot enumerate a
catalogue by naming its contents in advance.

## Throttling is not an outage

The defining failure of a hosted provider, and it deserves its own error kind rather
than being folded into "unavailable":

> `ErrThrottled` means **the provider is fine, and you are over your quota.**

The distinction is not pedantry, it is a pager at 3am. If throttling were reported as
`ErrUnavailable`, then a "Bedrock is down" alarm would fire every time the platform got
*busy* — which is precisely when you least want to be woken up to look at a service
that is working perfectly.

Bedrock throttles on **tokens per minute** and **requests per minute**, per model, per
region, and the quotas are lower than most people expect on a new account. So:

- It is **retried**, with exponential backoff and full jitter — the same
  `internal/httpx` mechanics every integration uses. It is a transient condition and
  backing off is exactly the right response.
- The retry is safe for the same reason every inference retry is safe: generation has
  **no side effects** (see below).
- `ServiceQuotaExceededException` maps here too. It reads like a permanent problem and
  it is not: it is the same wall from a different angle.
- The log line says `errorKind: "throttled"`, `attempts: 3` — so a graph of throttling
  is a graph of *demand*, and it is the signal to ask AWS for a quota increase, not to
  restart anything.

**AWS SDK retries are disabled** (`WithRetryMaxAttempts(1)`). The SDK retries
throttling by default, and this integration also retries throttling — three attempts of
three is nine calls, an `attempts: 3` log line that is a lie, and a duration that
includes backoff nobody accounted for. Two retry layers do not add, they *multiply*, and
they hide each other. This integration owns the retry policy; the SDK owns the transport.

## Streaming, and the timeout that actually works

This was the part of Milestone 7 that took the most thought, and it transferred to
Bedrock unchanged — which is the strongest evidence that it was the right design.

**A total timeout is nearly useless for inference.** Set it long enough for a
legitimate slow generation on a CPU — minutes — and it will wait just as patiently
for a model that hung instantly. Set it short and it kills healthy work.

The useful question is not *"has this finished?"* but:

> **"Has it produced a single token in the last thirty seconds?"**

A slow model keeps answering yes. A wedged one does not. And only a **stream** can
answer that question at all — which is why streaming is the default, and why the
important knob is `*_IDLE_TIMEOUT` and not `*_TIMEOUT`.

```
BEDROCK_TIMEOUT       5m    total budget
BEDROCK_IDLE_TIMEOUT  60s   ← the one that matters: how long the model may go SILENT
```

The idle timer is armed before the read and **reset on every event**. A model producing
output slowly but steadily is healthy and is left alone. One that goes quiet is
`ErrStalled`, and the error says so — because *"the model went quiet"* is actionable and
*"context canceled"* is not.

For Bedrock the stall is rarer and more interesting: it means the event stream is open
and AWS has stopped sending on it. You will not see it often. You will be very glad the
platform noticed on the day you do.

## A retry is safe here — with one exception

After two milestones of being frightened of retries, this remains a relief:

| | Retrying it… |
| --- | --- |
| **M5** — an n8n trigger | can run a **workflow twice** |
| **M6** — an agent submission | can open a **second pull request** |
| **M7/M8** — an inference | costs **compute** (or a few cents), and nothing else |

Generation has **no side effects**. It reads a prompt and produces tokens. There is
nothing to deduplicate, no idempotency key, no invoice to reverse. Timeouts, stalls and
throttles are all retried, because the worst case is that you pay for the tokens twice.

That "few cents" is the one thing Bedrock changes, and it is worth being honest about:
on a hosted provider a retry is no longer *free*, it is *cheap*. A retried 100k-token
prompt is billed twice. This is a reason to bound retries (three attempts, not ten), not
a reason to abandon them — but it is why `estimatedCostUsd` is logged per call and why
`attempts` is logged next to it.

**Except once a stream has started.**

```go
// A stream that fails BEFORE the first token is retried — the caller has seen
// nothing. One that fails AFTER is ErrStreamBroken, and it is terminal.
```

The sink is a *side effect*: the caller may already have written those tokens to a
terminal, a websocket, or a file. Retrying would hand them **a second beginning**,
silently glued onto the first, and the result reads as though the model lost its mind
rather than as though the network dropped.

This applies to stalls too — a stall after output is a broken stream that broke by going
quiet. The error wraps **both**, so `errors.Is` finds `ErrStalled` (the cause, which is
what the log reports) *and* `ErrStreamBroken` (the consequence, which is what stops the
retry).

## The silent-truncation trap

The failure that does the most damage while looking most like success.

> **A model asked to read more than its context window does not refuse.** It silently
> drops the beginning of the prompt and answers confidently from what is left.

You get a plausible, wrong summary. No error is raised anywhere. Nothing in any log
says it happened. The blog post reads fine until someone notices it is about the
wrong commit.

So the platform **refuses to send it**:

```
error: prompt exceeds the model's context window: ~13334 tokens of prompt into a
       4096-token window. The model would silently drop the beginning and answer
       from the rest — summarise or chunk the input instead
```

The estimate is deliberately **pessimistic** (`CharsPerToken = 3`): code tokenises far
worse than prose, and being wrong in the direction of refusing a prompt that would have
fitted is enormously better than being wrong in the direction of silent truncation.

Which means `*_CONTEXT_TOKENS` **must be set correctly for your model**. It is the one
setting where being wrong is invisible. On Bedrock this is also the *cheap* check: an
oversized prompt refused locally costs nothing, while one sent to a per-token provider
is billed in full for an answer to a question it only half read.

## Configuration

**Choosing the provider:**

| Variable | Default | Notes |
| --- | --- | --- |
| `LLM_PROVIDER` | `ollama` | `ollama` or `bedrock`. The default keeps the prompt at home. |

**Bedrock (M8).** Note what is absent: any credential.

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `BEDROCK_MODEL_ID` | ✅ | — | e.g. `anthropic.claude-3-5-haiku-20241022-v1:0`, or a `us.`-prefixed **inference profile** for newer models. There is no sensible default. |
| `BEDROCK_REGION` | | `us-east-1` | Availability, price and quota are all **regional**. |
| `BEDROCK_CONTEXT_TOKENS` | | `200000` | **Get this right** — see above. |
| `BEDROCK_MAX_TOKENS` | | `2048` | Completion budget. On a per-token provider this is a **spend** limit, not just a length limit. |
| `BEDROCK_TEMPERATURE` | | `0.2` | **`[0, 1]` — narrower than Ollama's `[0, 2]`.** `1.5` is legal there and a validation error here; the config rejects it at start-up and says why. |
| `BEDROCK_IDLE_TIMEOUT` | | `60s` | **The important timeout.** Stall detection. |
| `BEDROCK_TIMEOUT` | | `5m` | Total budget. |
| `BEDROCK_STREAM` | | `true` | |
| `BEDROCK_RETRY_ATTEMPTS` / `_DELAY` | | `3` / `1s` | Total attempts, not retries after the first. The **SDK's own retries are disabled** so these numbers are true. |
| `BEDROCK_INPUT_COST_PER_1M_USD` | | `0` | What you are actually billed. Configuration, not a stale table in source. |
| `BEDROCK_OUTPUT_COST_PER_1M_USD` | | `0` | Typically 4–5× the input price. |
| `BEDROCK_ENDPOINT` | | — | Override. For a **VPC endpoint** (PrivateLink), or a stub in tests. |
| ~~`BEDROCK_API_KEY`~~ | | — | **Does not exist.** See [Authentication](#authentication-there-is-no-credential). |

**Ollama (M7).**

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `OLLAMA_BASE_URL` | ✅ | — | e.g. `http://10.20.1.5:11434`. |
| `OLLAMA_MODEL` | ✅ | — | There is no sensible default model. |
| `OLLAMA_CONTEXT_TOKENS` | | `8192` | **Get this right** — see above. |
| `OLLAMA_IDLE_TIMEOUT` | | `60s` | **The important timeout.** Stall detection. |
| `OLLAMA_TIMEOUT` | | `5m` | Total budget; non-streaming only. |
| `OLLAMA_MAX_TOKENS` | | `2048` | Completion budget. |
| `OLLAMA_TEMPERATURE` | | `0.2` | `[0, 2]`. Low: most of this platform's work is summarising, not inventing. |
| `OLLAMA_STREAM` | | `true` | |
| `OLLAMA_KEEP_ALIVE` | | `5m` | How long Ollama holds the model in memory. Too short and you pay the load cost on **every** request — the symptom is a large `loadMs` on every log line, not just the first. |
| `OLLAMA_RETRY_ATTEMPTS` / `_DELAY` | | `3` / `1s` | Total attempts, not retries after the first. |
| `OLLAMA_TOKEN` | | — | Only if Ollama is behind an authenticating proxy. **Ollama itself has no auth.** |
| `OLLAMA_CA_CERT` | | — | Private CA. There is no "skip TLS verification". |

## Example request and response

**The caller does not change.** This is the same code against either provider — the only
difference is which one `providers.New` built:

```go
res, err := svc.Stream(ctx, llm.Request{
	System:        "You are a precise technical writer. Answer in three bullets.",
	Prompt:        "Summarise this diff:\n" + diff,
	Purpose:       "diff-summary",
	CorrelationID: "push:delivery-abc-123",
	Options:       llm.Options{MaxTokens: 512, Temperature: 0.2},
}, func(c llm.Chunk) error {
	fmt.Print(c.Content) // tokens, as they arrive
	return nil
})
```

Underneath, Bedrock's **Converse** API is what makes that possible. `InvokeModel` takes
a per-model JSON body — Anthropic's schema is not Meta's is not Amazon's — and building
one provider on it means writing a small adapter per model family and a bug per model
family. `Converse` is one messages-shaped API across all of them, and swapping
`BEDROCK_MODEL_ID` from Claude to Llama is genuinely just a different string.

One shape difference worth knowing: **the system prompt is its own field**, not a message
with `role: "system"`. Send it as a message and Bedrock rejects the request.

```json
{
  "provider": "bedrock",
  "model": "anthropic.claude-3-5-haiku-20241022-v1:0",
  "content": "- Spot instances cut the bill by ~70%…",
  "usage": {
    "promptTokens": 120, "completionTokens": 11,
    "tokensPerSecond": 20.0
  },
  "finishReason": "stop"
}
```

There is **no `loadDuration`** on Bedrock. The model is always loaded — that is what you
are paying for.

## Errors

The sentinels are the platform's, not any provider's. A caller handles `ErrThrottled`
without knowing which provider produced it, which is the point.

| Error | Means | Retried? | Ollama | Bedrock |
| --- | --- | --- | --- | --- |
| `ErrUnavailable` | Could not reach the provider. | ✅ | ✅ | ✅ |
| `ErrTimeout` | No answer within the budget. | ✅ | ✅ | ✅ |
| `ErrStalled` | Started producing tokens, then **went quiet**. | ✅ *if no output had escaped* | ✅ | ✅ |
| `ErrThrottled` | **Over quota.** The provider is fine. | ✅ | — *(no quotas)* | ✅ |
| `ErrStreamBroken` | The stream failed **after** output reached the caller. | ❌ **never** | ✅ | ✅ |
| `ErrUnauthorized` | Credentials rejected. | ❌ | — *(no auth)* | ✅ |
| `ErrModelAccessDenied` | The model **exists** and this account may not use it. | ❌ | — | ✅ |
| `ErrModelNotFound` | No such model. Retrying will not conjure it. | ❌ | ✅ *(not pulled)* | ✅ *(wrong ID/region)* |
| `ErrContextExceeded` | The prompt would be **silently truncated**. | ❌ | ✅ | ✅ |
| `ErrEmptyCompletion` | A 200 with **zero tokens** — not a success. | ❌ | ✅ | ✅ |
| `ErrInvalidResponse` | Not what the API promised. | ❌ | ✅ | ✅ |

The three empty cells in the Ollama column are the entire argument of
[the section above](#what-a-second-provider-did-to-the-abstraction): they are not
Bedrock quirks, they are things Ollama simply **cannot do**, and a vocabulary designed
against it alone had no words for them.

Bedrock's `ValidationException` deserves a note, because it is the one exception AWS uses
for several unrelated things. The platform disambiguates by message: a context-length
complaint becomes `ErrContextExceeded`; the on-demand-throughput complaint becomes an
`ErrModelNotFound` whose message tells you the actual fix, which is not obvious:

```
error: no such model: "anthropic.claude-3-5-sonnet-20241022-v2:0" is not available for
       on-demand invocation in us-east-1. Newer models require a cross-region inference
       profile — try the "us." prefix: us.anthropic.claude-3-5-sonnet-20241022-v2:0
```

Every Bedrock error also carries the **AWS request ID**, which is the only thing AWS
Support will ask you for.

## Observability

```json
{"level":"INFO","msg":"inference requested","provider":"bedrock","correlationId":"push:delivery-abc","purpose":"diff-summary","model":"anthropic.claude-3-5-haiku-20241022-v1:0","promptChars":362,"estimatedTokens":120,"promptHash":"d675def3732b","maxTokens":512}
{"level":"INFO","msg":"inference completed","provider":"bedrock","tokensPerSecond":20.0,"promptTokens":120,"completionTokens":11,"firstTokenMs":210,"durationMs":550,"finishReason":"stop","estimatedCostUsd":0.000140,"streamed":true}
{"level":"WARN","msg":"inference failed","provider":"bedrock","errorKind":"throttled","attempts":3,"retriesExhausted":true,"requestId":"a1b2c3d4-…"}
```

The same shape for both providers, which is what makes them comparable at all. Four
numbers earn their place:

**`provider`** is on every line. When a router (M10) starts choosing per request, the
question "which provider answered *this* one?" becomes the first thing anyone asks, and
a log that cannot answer it is a log that cannot explain the bill.

**`tokensPerSecond`** is the most diagnostic number in the integration. On Ollama, below
about ten means **you are on a CPU**. On Bedrock it is a service-health signal you did
not have to instrument.

**`estimatedCostUsd`** exists because on Ollama the marginal cost of a request is zero
and on Bedrock it is not, and *nobody notices the difference until the invoice*. It is
`(promptTokens × input + completionTokens × output) / 1e6`, using the configured price.
It is an **estimate** — AWS's bill is the truth — but an estimate you can group by
`purpose` beats a truth that arrives 30 days late.

**`errorKind`** is the platform's vocabulary, not the provider's. `throttled` is not
`unavailable`, and an alarm on the latter no longer fires when the platform is merely
busy.

And `finishReason: "length"` is logged as a **warning**: it means the answer was cut off
by the token budget, and a truncated blog post looks a great deal like a finished one
until somebody reads the end of it.

## Security

**Prompts are never logged.** They contain repository content — source code, commit
messages, and on a bad day something nobody meant to commit. Logging them ships all of it
to CloudWatch. The logs carry the prompt's **size and a hash** instead, so two log lines
can be recognised as the same prompt without either containing it. The same applies to
completions, which are derived from prompts and can echo them.

**The prompt leaves the network on Bedrock.** This is not a vulnerability, it is the
deal, and it should be a decision rather than a default. It stays inside AWS (in your
account's region, over TLS, not used to train anything), which is a materially different
proposition from a third-party SaaS — but it is still *leaving*, and for a private
repository that may be the whole question. `LLM_PROVIDER=ollama` is the answer, the
default, and the reason `Capabilities.Local` is a first-class field.

**Set `BEDROCK_ENDPOINT` to a VPC endpoint** if you want the traffic never to touch the
public internet at all. PrivateLink for `bedrock-runtime` means the call leaves your VPC
for AWS's network directly, and the security group stays shut.

**No credentials, anywhere.** See [Authentication](#authentication-there-is-no-credential).
The IAM policy names the models it may invoke and nothing else, and grants **nothing** by
default.

**Ollama has no authentication.** That is not a flaw in Ollama — it is a tool designed to
run on your laptop. It does mean an Ollama reachable from a network is an open inference
endpoint for anyone who can reach it, so it belongs behind a security group that lets
nothing in, which is exactly what this platform's [network stack](infra/README.md)
provides.

## Local development

**Bedrock** — no server to run, but you do need credentials and an entitlement:

```bash
aws sso login                                   # temporary credentials, same as prod
export LLM_PROVIDER=bedrock
export BEDROCK_MODEL_ID=anthropic.claude-3-5-haiku-20241022-v1:0
export BEDROCK_REGION=us-east-1

go run ./cmd/llm models        # what this ACCOUNT may actually invoke
go run ./cmd/llm generate --prompt "Summarise EC2 Spot in three bullets."
```

`models` is the command to run first. It answers "what am I entitled to?", which is a
question about your account and not about your code, and it is the fastest way to find
out that the model you configured needs an entitlement nobody has requested.

**Ollama** — a server, and no entitlement:

```bash
brew install ollama && ollama serve
ollama pull llama3.2

export LLM_PROVIDER=ollama                      # (the default)
export OLLAMA_BASE_URL=http://localhost:11434
export OLLAMA_MODEL=llama3.2

go run ./cmd/llm generate --prompt "Summarise EC2 Spot in three bullets."
```

Both **stream**, so you watch the model think, and both print the same footer:

```
--- 11 tokens in 550ms · 20.0 tok/s · finish: stop · ~$0.000140 ---
```

You cannot tell whether a model is any good by reading its API contract. A prompt that
works beautifully against Claude produces confident nonsense on a 3B local model, and the
only way to find out is to run it and read what comes back. **Running the same prompt
through both is now one environment variable**, which is the most useful thing this
milestone built.

## Testing

```bash
go test ./internal/llm/ ./internal/ollama/ ./internal/bedrock/ ./internal/providers/
```

**No test touches AWS.** Not with a credential, not with a real region, not "just the
cheap model". A unit test that calls Bedrock is a unit test that fails on an aeroplane,
costs money in CI, and turns a code review into a permissions ticket.

Instead, the provider takes the two AWS APIs it needs as **narrow interfaces** —
`Converse`, `ConverseStream`, `ListFoundationModels` — and the tests supply fakes that
return real `smithy.OperationError`s. Which is the interesting part: **the tests exercise
the error mapping**, which is where the actual logic lives, and they do it without a
network.

| | |
| --- | --- |
| **The provider is chosen by configuration** | One env var, two backends, one interface. The claim of the milestone, asserted. |
| **The default is the one that keeps the prompt at home** | A test that fails if someone makes `bedrock` the default. |
| **`Local` is `false` and cost is non-zero** | The two facts M10 will route on. |
| **Throttling is retried** | And `AccessDenied`, `ModelNotFound`, oversized prompts are **not** — asserted by **call count**, because "we retried a permission error 3 times" is a bug you only see in the bill. |
| **AWS exceptions map to platform sentinels** | With messages that name the fix, including "you need an inference profile". |
| **The system prompt goes in `System`, not a message** | Bedrock rejects the alternative. |
| **There is no credential in the config** | The redacted output is searched for `accesskey`, `secretkey`, `sessiontoken`, `apikey`. |
| **Only the factory imports two vendors** | `internal/architecture_test.go`, using `go/build`. It fails the build, not a review. |
| **A broken stream is not retried once tokens have escaped** | Both providers. A regression would hand the caller a second beginning. |
| **An oversized prompt never reaches the provider** | It would be silently truncated — and, on Bedrock, billed. |
| **The prompt is never logged** | Nor the completion. Only sizes and a hash. |

## Supported models

**Bedrock** — anything your account is entitled to, in your region. `go run ./cmd/llm models`
is the authority; the table below is the useful subset:

| Model | Good for | Notes |
| --- | --- | --- |
| `anthropic.claude-3-5-haiku-…` | Summarising, classification | Cheap and quick. The sensible default. |
| `us.anthropic.claude-sonnet-4-…` | Reasoning, code review | **Needs an inference profile** (`us.` prefix). |
| `amazon.nova-lite-v1:0` | Bulk summarising | Very cheap. |
| `meta.llama3-…` | General prose | The same weights you can run on Ollama — which makes it the honest local-vs-hosted comparison. |

**Ollama** — anything it can serve. What is *sensible* depends on the box:

| Model | Params | Good for | Needs |
| --- | --- | --- | --- |
| `llama3.2` | 3B | Summarising, classification, short prose | CPU (slowly) or any GPU |
| `qwen2.5-coder:7b` | 7B | Reading diffs and code | GPU |
| `llama3.1:70b` | 70B | Reasoning | A GPU you are not going to put on Spot casually |

## Future providers

The abstraction now has two implementations and no caller has changed, which is the
evidence for the rest of this list:

- **Claude** (M9) — Anthropic's API directly. Frontier reasoning, `Local: false`, and a
  real API key, which will be the *third* auth model in three providers (none, IAM, and a
  secret that must be stored and rotated). Expect it to test the vocabulary again.
- **Hybrid routing** (M10) — choose per request: cheap-and-local for a summary,
  frontier-and-hosted for reasoning, and **local-only** for a private repository whose
  source may not leave. It will implement `llm.Provider` itself and sit exactly where a
  single provider sits today. `Capabilities` — `Local`, and now a real cost — is what that
  decision is made on, and both fields exist because M8 forced them to be true.
- **Fallback** — Bedrock as the backstop when the Spot GPU is
  [interrupted](infra/SPOT.md). This is now genuinely possible, and it is the shape M3
  built for: the local model vanishes with two minutes' notice, and the platform keeps
  answering, more expensively.

## Troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| `AccessDeniedException` **on Bedrock** | **Two** possible causes and they look identical: the IAM role lacks `bedrock:InvokeModel` for that model ARN, **or** the account was never granted the model in the Bedrock console under *Model access*. Check the second one first — it is the one people forget. |
| `ValidationException`: *"on-demand throughput isn't supported"* | The model needs a **cross-region inference profile**. Prefix the ID with `us.` (or `eu.`). The error message from this platform tells you exactly this. |
| `ErrThrottled`, repeatedly | You are over your Bedrock quota. It is **not** an outage. Ask for a quota increase (Service Quotas → Bedrock → tokens per minute for that model), or route the cheap work to Ollama. |
| `ErrModelNotFound` **on Bedrock** | Usually the **region**: model availability is regional, and the ID is right but the region is not. `go run ./cmd/llm models` lists what is actually there. |
| `ErrModelNotFound` **on Ollama** | It was never pulled. The error contains the exact command. |
| A **surprising Bedrock bill** | Group `estimatedCostUsd` by `purpose`. That is what the field is for. Then check `attempts` — a retried 100k-token prompt is billed twice. |
| **Single-digit tokens/sec** on Ollama | You are on a CPU. If the box has a GPU, the model is not using it. |
| `loadMs` is large on **every** Ollama request | The model is being evicted between calls. Raise `OLLAMA_KEEP_ALIVE`. |
| Temperature `1.5` is rejected | Bedrock's range is `[0, 1]`; Ollama's is `[0, 2]`. You copied the value across. The error says so. |
| The summary is **confidently about the wrong thing** | Suspect truncation. Check `*_CONTEXT_TOKENS` matches the model — set too high, and the platform will happily send a prompt the model quietly cuts in half. |
| `ErrStalled` | The model went quiet. On Ollama, often memory pressure. On Bedrock, rare — and the reason the idle timer exists. |
| `ErrStreamBroken` | The connection dropped mid-answer. Not retried, deliberately — the caller has partial output. |
| The answer stops mid-sentence | `finishReason: "length"`. Raise `*_MAX_TOKENS`. |
| It called the wrong provider entirely | `LLM_PROVIDER`. It is logged on every line, and the default is `ollama`. |
