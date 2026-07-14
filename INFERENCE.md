# Inference — Ollama, and the Provider Abstraction

The platform runs its own inference against a **self-hosted Ollama**, behind a
provider interface that Bedrock (M8), Claude (M9), and a router (M10) will
implement next.

- **Generate** — one prompt, one completion
- **Stream** — tokens as they arrive, because a generation you cannot watch is
  indistinguishable from a hang
- **Discover** — what models the box actually has
- **Refuse** — a prompt that does not fit, before the model silently truncates it

> **This repository does not deploy Ollama.** The instance, the GPU, and the models
> on disk belong to `ollama-on-aws`. This repository owns *the provider abstraction
> that calls it* — which the repository scope has said from the beginning.

The *why* is in the blog post,
[Running Local LLMs with Ollama on AWS](docs/blog/running-local-llms-with-ollama-on-aws.md).

## Contents

- [Wait — Milestone 6 said the platform calls no model](#wait--milestone-6-said-the-platform-calls-no-model)
- [Who does what](#who-does-what)
- [Why local inference](#why-local-inference)
- [The provider abstraction](#the-provider-abstraction)
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
| **The platform** (M7, this) | The platform does | A function call: prompt in, tokens out |

This is the *inference plane* the architecture has had on paper since Milestone 1,
and the provider abstraction is listed under "what this repository owns" in the
[repository scope](README.md#repository-scope).

## Who does what

| | Owns |
| --- | --- |
| **n8n** (M5) | **Orchestration.** What happens, in what order, and the waiting. |
| **OpenClaw** (M6) | **Agentic execution.** Open-ended tasks with tools. Calls its own model. |
| **This package** (M7) | **The platform's own inference**, and the provider abstraction. |
| **Ollama** | **Local model serving.** Deployed by `ollama-on-aws`. |
| **Bedrock / Claude** (M8, M9) | Hosted inference, behind the same interface. |
| **The router** (M10) | Choosing between them per request. |

## Why local inference

The usual arguments are cost and latency. Neither is the real one.

**The real one is that the prompt does not leave.** This platform's prompts are full
of *somebody's source code* — diffs, files, commit messages, and occasionally
something nobody meant to commit. A hosted provider means all of that crosses the
internet to a third party, and no amount of TLS changes the fact that you have sent
them your source.

For a public repository, fine. For a private one, it is the whole question — and it
is why `Capabilities.Local` exists as a first-class field that a future router can
route on.

The other trade-offs, honestly:

| | Local (Ollama) | Hosted (Bedrock, Claude) |
| --- | --- | --- |
| **The prompt leaves** | ❌ no | ✅ yes |
| **Cost** | The instance, paid whether you use it or not | Per token, paid only when you do |
| **Quality** | A 3B–13B model. Good at summarising; not good at reasoning | Frontier models |
| **Speed** | GPU: fast. **CPU: single-digit tokens/sec** | Fast |
| **Availability** | Yours to keep up | Someone else's problem |

**Local models are not small versions of big models.** A 3B model is genuinely good
at "summarise this diff in three bullets" and genuinely bad at "reason about whether
this architecture is sound". Sending it the second kind of work produces something
confident and wrong, which is worse than a refusal. That asymmetry is the entire
reason Milestone 10 exists.

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
promise nobody collects on. This one is different: **the swap is the roadmap.**
Bedrock is Milestone 8, Claude is Milestone 9, and Milestone 10 routes between them
per request. A provider abstraction retro-fitted at Milestone 10, on top of three
call sites that each learned Ollama's JSON, is a rewrite. Added now, it is an
interface with one implementation.

`Capabilities` is deliberately small — the facts a router needs, and nothing else:

```go
type Capabilities struct {
	Local                    bool  // does the prompt LEAVE? the field that matters
	Streaming                bool
	MaxContextTokens         int
	CostPer1MInputTokensUSD  float64  // 0 for local: the cost is the instance
	CostPer1MOutputTokensUSD float64
}
```

`internal/llm` does not import `internal/ollama`. That is the test that the seam is
real rather than decorative.

## Streaming, and the timeout that actually works

This is the part of the milestone that took the most thought.

**A total timeout is nearly useless for inference.** Set it long enough for a
legitimate slow generation on a CPU — minutes — and it will wait just as patiently
for a model that hung instantly. Set it short and it kills healthy work.

The useful question is not *"has this finished?"* but:

> **"Has it produced a single token in the last thirty seconds?"**

A slow model keeps answering yes. A wedged one does not. And only a **stream** can
answer that question at all — which is why streaming is the default, and why the
important knob is `OLLAMA_IDLE_TIMEOUT` and not `OLLAMA_TIMEOUT`.

```
OLLAMA_TIMEOUT       5m    total budget — only used for non-streaming calls
OLLAMA_IDLE_TIMEOUT  60s   ← the one that matters: how long the model may go SILENT
```

The idle timer is armed before the read and **reset on every token**. A model
producing output slowly but steadily is healthy and is left alone. One that goes
quiet is `ErrStalled`, and the error says so — because *"the model went quiet"* is
actionable and *"context canceled"* is not.

## A retry is safe here — with one exception

After two milestones of being frightened of retries, this is a relief and worth
saying out loud:

| | Retrying it… |
| --- | --- |
| **M5** — an n8n trigger | can run a **workflow twice** |
| **M6** — an agent submission | can open a **second pull request** and spend real money |
| **M7** — an inference | costs **compute**, and nothing else |

Generation has **no side effects**. It reads a prompt and produces tokens. There is
nothing to deduplicate, no idempotency key, no invoice. Timeouts *and stalls* are
both retried, because the worst case is that we pay for the compute twice.

**Except once a stream has started.**

```go
// A stream that fails BEFORE the first token is retried — the caller has seen
// nothing. One that fails AFTER is ErrStreamBroken, and it is terminal.
```

The sink is a *side effect*: the caller may already have written those tokens to a
terminal, a websocket, or a file. Retrying would hand them **a second beginning**,
silently glued onto the first, and the result reads as though the model lost its
mind rather than as though the network dropped.

This applies to stalls too — a stall after output is a broken stream that broke by
going quiet. The error wraps **both**, so `errors.Is` finds `ErrStalled` (the cause,
which is what the log reports) *and* `ErrStreamBroken` (the consequence, which is
what stops the retry).

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

The estimate is deliberately **pessimistic** (`CharsPerToken = 3`): code tokenises
far worse than prose, and being wrong in the direction of refusing a prompt that
would have fitted is enormously better than being wrong in the direction of silent
truncation.

Which means `OLLAMA_CONTEXT_TOKENS` **must be set correctly for your model**. It is
the one setting where being wrong is invisible.

## Configuration

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `OLLAMA_BASE_URL` | ✅ | — | e.g. `http://10.20.1.5:11434`. |
| `OLLAMA_MODEL` | ✅ | — | There is no sensible default model. |
| `OLLAMA_CONTEXT_TOKENS` | | `8192` | **Get this right** — see above. |
| `OLLAMA_IDLE_TIMEOUT` | | `60s` | **The important timeout.** Stall detection. |
| `OLLAMA_TIMEOUT` | | `5m` | Total budget; non-streaming only. |
| `OLLAMA_MAX_TOKENS` | | `2048` | Completion budget. Unbounded generation on hardware billed by the hour is a model free to ramble at your expense. |
| `OLLAMA_TEMPERATURE` | | `0.2` | Low: most of this platform's work is summarising, not inventing. |
| `OLLAMA_STREAM` | | `true` | |
| `OLLAMA_KEEP_ALIVE` | | `5m` | How long Ollama holds the model in memory. Too short and you pay the load cost on **every** request — the symptom is a large `loadMs` on every log line, not just the first. |
| `OLLAMA_RETRY_ATTEMPTS` / `_DELAY` | | `3` / `1s` | Total attempts, not retries after the first. |
| `OLLAMA_TOKEN` | | — | Only if Ollama is behind an authenticating proxy. **Ollama itself has no auth** — see [Security](#security). |
| `OLLAMA_CA_CERT` | | — | Private CA. There is no "skip TLS verification". |

## Example request and response

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

```json
{
  "model": "llama3.2",
  "content": "- Spot instances cut the bill by ~70%…",
  "usage": {
    "promptTokens": 120, "completionTokens": 7,
    "tokensPerSecond": 3.5, "loadDuration": "300ms"
  },
  "finishReason": "stop"
}
```

## Errors

| Error | Means | Retried? |
| --- | --- | --- |
| `ErrUnavailable` | Could not reach Ollama. | ✅ |
| `ErrTimeout` | No answer within the budget. | ✅ |
| `ErrStalled` | Started producing tokens, then **went quiet**. | ✅ *if no output had escaped* |
| `ErrStreamBroken` | The stream failed **after** output reached the caller. | ❌ **never** |
| `ErrModelNotFound` | Not pulled. Retrying will not download it. | ❌ |
| `ErrContextExceeded` | The prompt would be **silently truncated**. | ❌ |
| `ErrEmptyCompletion` | A 200 with **zero tokens** — not a success. | ❌ |
| `ErrInvalidResponse` | Not JSON, or an error inside a `200`. | ❌ |

## Observability

```json
{"level":"INFO","msg":"inference requested","provider":"ollama","correlationId":"push:delivery-abc","purpose":"diff-summary","model":"llama3.2","promptChars":362,"estimatedTokens":120,"promptHash":"d675def3732b","maxTokens":512}
{"level":"INFO","msg":"inference completed","tokensPerSecond":3.5,"promptTokens":120,"completionTokens":7,"firstTokenMs":5,"loadMs":300,"durationMs":1078,"finishReason":"stop","outputChars":34,"streamed":true}
```

Three numbers earn their place:

**`tokensPerSecond`** is the most diagnostic number in the integration. Below about
ten, **you are on a CPU** — and every inference the platform expects to take seconds
is about to take minutes. You can see that from a log line, without logging in to
anything.

**`loadMs`** on *every* request (rather than only the first) means the model is being
evicted between calls. That is `OLLAMA_KEEP_ALIVE`, not a mystery.

**`firstTokenMs`** is the latency a human actually experiences, and it separates "the
model is slow" from "the model was not loaded".

**`purpose`** makes "what is this platform spending its tokens on?" answerable — the
first question anyone asks when the GPU bill arrives, and the last one an unlabelled
log can answer.

And `finishReason: "length"` is logged as a **warning**: it means the answer was cut
off by the token budget, and a truncated blog post looks a great deal like a finished
one until somebody reads the end of it.

## Security

**Prompts are never logged.** They contain repository content — source code, commit
messages, and on a bad day something nobody meant to commit. Logging them ships all
of it to CloudWatch. The logs carry the prompt's **size and a hash** instead, so two
log lines can be recognised as the same prompt without either containing it. The same
applies to completions, which are derived from prompts and can echo them.

**Ollama has no authentication.** That is not a flaw in Ollama — it is a tool designed
to run on your laptop. It does mean an Ollama reachable from a network is an open
inference endpoint for anyone who can reach it, so it belongs behind a security group
that lets nothing in, which is exactly what this platform's
[network stack](infra/README.md) provides. `OLLAMA_TOKEN` exists only for the case
where a proxy in front of it does authenticate.

The config refuses to send a **token over plain HTTP to a public host**, while
allowing plain HTTP to a private one — because the rule is about credentials crossing
the internet, not about the scheme.

## Local development

```bash
brew install ollama && ollama serve      # or: docker run -p 11434:11434 ollama/ollama
ollama pull llama3.2

export OLLAMA_BASE_URL=http://localhost:11434
export OLLAMA_MODEL=llama3.2

go run ./cmd/llm models        # what is on the box
go run ./cmd/llm check         # is the configured model actually there?
go run ./cmd/llm generate --prompt "Summarise EC2 Spot in three bullets."
```

It **streams**, so you watch the model think. And it tells you when it is being slow
for a reason you can fix:

```
--- 7 tokens in 1.078s · 3.5 tok/s · load 300ms · finish: stop ---
note: 3.5 tok/s is CPU-speed. If this box has a GPU, the model is not using it.
```

You cannot tell whether a model is any good by reading its API contract. A prompt
that works beautifully against a 70B model produces confident nonsense on a 3B one,
and the only way to find out is to run it and read what comes back. That is what this
CLI is for.

## Testing

```bash
go test ./internal/llm/ ./internal/ollama/
```

Ollama is mocked with `httptest`, including real NDJSON streaming. What the tests pin
down:

| | |
| --- | --- |
| **A broken stream is not retried once tokens have escaped** | Asserted by *call count* — a regression that retries would hand the caller a second beginning. |
| **A failure before the first token IS retried** | And the caller sees exactly one answer, not three. |
| **A stall is detected, and reset by every token** | A slow-but-steady model is not killed; a silent one is. |
| **A stall after output is terminal**, and still reports `stalled` as the cause. |
| **An oversized prompt never reaches the provider** | It would be silently truncated. |
| **The prompt is never logged** | Nor the completion. Only sizes and a hash. |
| **An empty completion is a failure** | A 200 with zero tokens is not a success. |
| **A missing model says how to fix it** | And is not retried — asking again will not pull it. |
| **The seam** | The `Service` runs entirely against a fake provider, with no HTTP. |

## Supported models

Anything Ollama can serve. What is *sensible* depends on the box:

| Model | Params | Good for | Needs |
| --- | --- | --- | --- |
| `llama3.2` | 3B | Summarising, classification, short prose | CPU (slowly) or any GPU |
| `qwen2.5-coder:7b` | 7B | Reading diffs and code | GPU |
| `mistral` / `gemma2:9b` | 7–9B | General prose | GPU |
| `llama3.1:70b` | 70B | Reasoning | A GPU you are not going to put on Spot casually |

**This milestone does not pull models.** `ollama pull <model>` on the host — the error
message tells you so, with the exact command.

## Future providers

The abstraction exists for these, and none of them is a change to a caller:

- **Amazon Bedrock** (M8) — hosted, in your account, `Local: false`.
- **Claude** (M9) — frontier reasoning, `Local: false`, real per-token cost.
- **Hybrid routing** (M10) — choose per request: cheap-and-local for a summary,
  frontier-and-hosted for reasoning, and **local-only** for a private repository
  whose source may not leave. `Capabilities` is what that decision is made on.
- **Fallback** — a hosted provider as the backstop when the local GPU is interrupted,
  which is exactly the shape [Milestone 3](infra/SPOT.md) built for.

## Troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| **Single-digit tokens/sec** | You are on a CPU. If the box has a GPU, the model is not using it — check the Ollama host, not this integration. |
| `loadMs` is large on **every** request | The model is being evicted between calls. Raise `OLLAMA_KEEP_ALIVE`. |
| `ErrModelNotFound` | It was never pulled. The error contains the exact command. |
| The summary is **confidently about the wrong thing** | Suspect truncation. Check `OLLAMA_CONTEXT_TOKENS` matches the model — if it is set too high, the platform will happily send a prompt the model quietly cuts in half. |
| `ErrStalled` | The model went quiet. Often memory pressure: it is swapping, or another model was loaded and evicted it. |
| `ErrStreamBroken` | The connection dropped mid-answer. Not retried, deliberately — the caller has partial output. |
| The answer stops mid-sentence | `finishReason: "length"`. Raise `OLLAMA_MAX_TOKENS`. |
| `ErrEmptyCompletion` | A 200 with no tokens. Usually a prompt truncated to nothing, or a model that failed to load. |
