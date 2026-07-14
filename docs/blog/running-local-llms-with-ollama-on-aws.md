# Running Local LLMs with Ollama on AWS

> **Milestone 7 — Ollama Integration.**
> This milestone integrates Ollama and builds the provider abstraction that Bedrock
> (M8), Claude (M9) and a router (M10) will implement next. It does not deploy Ollama,
> pull models, or configure a GPU — that lives in `ollama-on-aws`. The code is in
> [`internal/llm`](../../internal/llm) and [`internal/ollama`](../../internal/ollama).

*Audience: engineers wiring a local model into a real system, and anyone who has
watched a small model produce a confident, beautifully-formatted, completely wrong
summary and wondered where it went wrong.*

---

## Contents

- [A claim I made last milestone](#a-claim-i-made-last-milestone)
- [Why run a model yourself](#why-run-a-model-yourself)
- [The provider abstraction, and why now](#the-provider-abstraction-and-why-now)
- [The timeout that actually works](#the-timeout-that-actually-works)
- [A retry is safe here — and that is new](#a-retry-is-safe-here--and-that-is-new)
- […until the first token escapes](#until-the-first-token-escapes)
- [The failure that looks like success](#the-failure-that-looks-like-success)
- [Do not log the prompt](#do-not-log-the-prompt)
- [The number that tells you everything](#the-number-that-tells-you-everything)
- [Lessons learned](#lessons-learned)
- [What comes next](#what-comes-next)

---

## A claim I made last milestone

[Milestone 6](integrating-openclaw-into-an-ai-agent-platform.md) says, in bold, in the
code comments, in the README, and on the architecture diagram:

> **This platform calls no model. The agent does.**

And now here is a milestone whose entire subject is the platform calling a model. So
either I was wrong then, or I am cheating now, and it is worth being precise rather
than quietly widening the claim and hoping nobody re-reads it.

**The agent's inference is still the agent's.** When OpenClaw reasons, it calls its own
model, behind its own boundary. Nothing in this milestone is in that path. Swapping the
agent's model is still a change in `openclaw-on-aws` that this repository does not
notice. That sentence was true and remains true.

**What has changed is that not everything worth doing with a model needs an agent.**

"Summarise this diff." "Write release notes from these commits." "Classify this event."
These are single-shot: one prompt, one completion. No shell. No tools. No reasoning
loop. Sending them through an agent means paying for an **errand** when what you wanted
was a **function call** — and an agent, as Milestone 6 established at length, is
expensive, non-deterministic, and comes with a step budget and an idempotency key.

So there are two consumers of inference and they are genuinely different things:

| | Who calls the model | Shape |
| --- | --- | --- |
| **Agent plane** (M6) | OpenClaw, itself | An errand: tools, a loop, minutes |
| **Inference plane** (M7) | The platform | A function call: prompt in, tokens out |

The repository's scope has said this from the start, which is a small mercy: it lists
*"provider abstraction over model backends"* under what this repository owns, and gives
`ollama-on-aws` the deployment while keeping *"the provider abstraction that calls it"*
here.

I have gone back and corrected the Milestone 6 wording rather than leaving a
contradiction lying around for someone to find. A repository whose documentation quietly
disagrees with itself is worse than one that admits a distinction got sharper.

## Why run a model yourself

The usual arguments are cost and latency. Neither is the real one.

**The real one is that the prompt does not leave.**

This platform's prompts are full of *somebody's source code*. Diffs, whole files, commit
messages, and — on a bad day — something nobody meant to commit. A hosted provider means
all of that crosses the internet to a third party. TLS protects it in transit and
changes nothing about the fact that you have **sent them your source**.

For a public repository, who cares. For a private one it is the entire question, and it
is why `Local` is a first-class field on the provider's capabilities rather than a
footnote:

```go
type Capabilities struct {
	Local                    bool  // does the prompt LEAVE? the field that matters
	MaxContextTokens         int
	CostPer1MInputTokensUSD  float64  // 0 for local: the cost is the instance
	...
}
```

The honest trade-offs:

| | Local (Ollama) | Hosted (Bedrock, Claude) |
| --- | --- | --- |
| The prompt leaves | ❌ **no** | ✅ yes |
| Cost | The instance, paid whether you use it or not | Per token, paid only when you do |
| Quality | 3B–13B. Good at summarising; bad at reasoning | Frontier |
| Speed | GPU: fast. **CPU: single-digit tokens/sec** | Fast |

And a point that is easy to miss: **a small model is not a small version of a big
model.** A 3B model is genuinely good at "summarise this diff in three bullets" and
genuinely bad at "reason about whether this architecture is sound". Give it the second
kind of work and it produces something *confident and wrong* — which is worse than a
refusal, because a refusal is visible.

That asymmetry is the whole reason Milestone 10 exists. Not every request should go to
the same model, and the decision has to be made on facts — is it local, what does it
cost, how much context does it have — which is exactly what `Capabilities` is for.

## The provider abstraction, and why now

The usual justification for an interface is "we might swap the implementation one day",
which is a promise nobody collects on. I am generally suspicious of it.

This one is different, because **the swap is the roadmap**: Bedrock is Milestone 8,
Claude is Milestone 9, and Milestone 10 routes between them per request. A provider
abstraction retro-fitted at Milestone 10, on top of three call sites that each learned
Ollama's JSON shape and its NDJSON streaming quirks, is a rewrite. Added now, it is an
interface with one implementation and nothing else changes.

```go
type Provider interface {
	Name() string
	Capabilities() Capabilities
	Models(ctx context.Context) ([]Model, error)
	Generate(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request, sink Sink) (Response, error)
}
```

The `Service` above it does what must be identical for *every* provider — validate,
check the prompt fits, carry the correlation, time it, log it. If each provider checked
context windows its own way, the same prompt would be accepted by one and silently
truncated by another, which is a bug you would never find.

And the mechanical test that the seam is real: **`internal/llm` does not import
`internal/ollama`.** If that dependency ever points the other way, the abstraction is a
decoration.

## The timeout that actually works

Here is the part that took the most thought, and it is not obvious until you have been
bitten.

**A total timeout is nearly useless for inference.**

Think about what value you would set. A generation on a CPU can legitimately take four
minutes. So the timeout has to be at least that — and now a model that hung *instantly*
is one you will wait four minutes to find out about. Set it shorter and you kill
healthy work. There is no good value. The knob is the wrong knob.

The useful question is not *"has this finished?"* It is:

> **"Has it produced a single token in the last thirty seconds?"**

A slow model keeps answering yes. A wedged one does not. And **only a stream can answer
that question at all** — which is the real reason streaming is the default here, ahead
of any argument about user experience.

So the idle timer is armed before the read and **reset on every token**:

```go
stall := time.AfterFunc(c.cfg.IdleTimeout, func() {
	stalled.Store(true)
	cancel() // makes the in-flight read fail
})
...
// A token arrived: the model is alive. Give it another idle window.
stall.Reset(c.cfg.IdleTimeout)
```

A model producing output slowly but steadily is healthy and is left alone — there is a
test that fails if a slow-but-steady stream is ever killed. One that goes quiet is
`ErrStalled`, and the error says exactly that, because *"the model went quiet"* is
actionable and *"context canceled"* is not.

## A retry is safe here — and that is new

After two milestones of being frightened of retries, this is genuinely a relief, and it
is worth saying out loud because the instinct built up over those milestones is now
*wrong*.

| Retrying… | costs |
| --- | --- |
| **M5** an n8n trigger | the workflow runs **twice** |
| **M6** an agent submission | a **second pull request**, and a second model bill |
| **M7** an inference | **compute.** That is all. |

Generation has **no side effects**. It reads a prompt and produces tokens. There is
nothing to deduplicate, no idempotency key, no invoice, nothing to be sorry about. So
the retryable set is *wider* than in either previous milestone: timeouts and even stalls
are retried, because the worst case is that we pay for the compute twice.

Two milestones of paranoia, and the correct answer here is "just retry it". Noticing
that — rather than reflexively copying the previous milestone's caution — is most of
what design is.

## …until the first token escapes

With one exception, and it is the sharp edge of this milestone.

**Once a stream has emitted a token, it can no longer be retried.**

The sink is a *side effect*. The caller may have already written those tokens to a
terminal, a websocket, or a file. Retry, and you hand them **a second beginning**,
silently glued onto the first:

```
Spot instances cut the biSpot instances cut the bill by 70%.
```

which reads as though the model lost its mind rather than as though the network dropped.

So the retry loop covers the request and the response headers, and stops the moment the
first token escapes. After that, any failure is `ErrStreamBroken`, and it is terminal.
The test asserts it by **call count**, because that is the only way to catch a
regression that would otherwise look fine:

```go
if n := atomic.LoadInt32(&calls); n != 1 {
	t.Errorf("Ollama was called %d times — a stream that emitted tokens must NOT be retried, "+
		"or the caller receives a second beginning appended to the first", n)
}
```

And this is where running it caught a bug the tests had not — see
[Lessons learned](#lessons-learned).

## The failure that looks like success

If you take one thing from this post, take this.

> **A model asked to read more than its context window does not refuse. It silently
> drops the beginning of the prompt and answers confidently from what is left.**

Send a 13,000-token diff to a model with a 4,096-token window, and you do not get an
error. You get a summary. It is well-formatted, plausible, fluent — and about the wrong
part of the diff, because the model never saw the first two-thirds of it. Nothing in any
log says this happened. Nothing anywhere raises an error. The blog post reads fine until
somebody notices it is describing the wrong commit.

That is the worst failure mode there is: **wrong, confident, and silent.**

So the platform refuses to send it:

```
error: prompt exceeds the model's context window: ~13334 tokens of prompt into a
       4096-token window. The model would silently drop the beginning and answer
       from the rest — summarise or chunk the input instead
```

The estimate is deliberately **pessimistic** — three characters per token, when English
prose averages nearer four. Code tokenises far worse than prose: punctuation,
identifiers, and indentation all cost tokens. Guessing low means we occasionally refuse a
prompt that would have fitted, and that is *enormously* the better way to be wrong.

Which makes `OLLAMA_CONTEXT_TOKENS` the one setting in this integration where **being
wrong is invisible**. Set it too high and the platform will cheerfully hand the model
prompts it quietly halves.

## Do not log the prompt

It is the obvious thing to log. It is the thing you want at 3am. And it is repository
content: source code, commit messages, and on a bad day something nobody meant to commit.
Logging it ships all of that to CloudWatch, where it is retained, indexed, and readable by
anyone with log access.

So the logs carry the prompt's **size and a hash** instead — enough to recognise two log
lines as the same prompt without either containing it:

```json
{"msg":"inference requested","promptChars":362,"estimatedTokens":120,"promptHash":"d675def3732b","purpose":"diff-summary"}
```

The same applies to the completion, which is derived from the prompt and can echo it back
verbatim. There is a test that fails if either leaks.

## The number that tells you everything

```json
{"msg":"inference completed","tokensPerSecond":3.5,"firstTokenMs":5,"loadMs":300,"finishReason":"stop"}
```

**`tokensPerSecond` is the most diagnostic number in the whole integration.** Below about
ten, you are on a CPU — and every inference the platform expected to take seconds is
about to take minutes. You can see that from a log line, without logging in to anything,
which is exactly what you want at the moment somebody says "the blog generator seems
slow".

The CLI just says it outright:

```
--- 7 tokens in 1.078s · 3.5 tok/s · load 300ms · finish: stop ---
note: 3.5 tok/s is CPU-speed. If this box has a GPU, the model is not using it.
```

Two more earn their place. `loadMs` on *every* request (rather than only the first) means
the model is being evicted between calls — that is `OLLAMA_KEEP_ALIVE`, not a mystery.
And `finishReason: "length"` is logged as a **warning**, because it means the answer was
cut off by the token budget, and a truncated blog post looks a great deal like a finished
one until somebody reads the end of it.

## Lessons learned

**Running it found a bug the tests could not.** The stall path worked — a stalled stream
was detected in about a second, exactly as designed. But the error it reported was
`stream broke after output had begun: refusing to resend a stream that has already
produced output`, with `attempts: 2`.

Which is *technically* true and completely useless. What actually happened is that the
model went quiet. Two things were wrong: a stall after output was being classified as
retryable (so it retried, and the retry hit a defensive guard), and the wrapper used
`%v` instead of `%w`, which **threw the cause away** — so `errors.Is(err, ErrStalled)`
was false and the log could only say "the stream broke".

The fix is that a stall after output is terminal for the same reason a broken stream is
(it *is* a broken stream; it broke by going quiet), and the error wraps **both**: the
cause, so the log says *the model stalled*, and the consequence, so the retry policy
knows not to. Every unit test passed before the fix, because each tested one half.

**Two milestones of paranoia had made me too careful.** My first instinct was an
idempotency key for inference, because the last two integrations needed one. They needed
one because they had *side effects*. Generation does not. Recognising when the previous
milestone's hard-won lesson does not apply is harder than learning it in the first place.

**The obvious log line is the dangerous one.** Logging the prompt is the single most
useful thing you could do for debugging and the single worst thing you could do for
security, and those two facts do not announce themselves at the same time.

## What comes next

**Milestone 8 — Amazon Bedrock.** The same interface, `Local: false`, a real per-token
price, and models that can actually reason. Which immediately raises the question this
milestone deliberately does not answer: *which one should a given request go to?*

That is **Milestone 10 — hybrid routing**, and every design decision here was made with
it in view. `Capabilities` exists so the router has facts to route on: is it local (may
this prompt leave?), what does it cost, how much context does it have. A summary of a
public diff goes to the cheap local model. A judgement call goes to a frontier model. A
private repository's source goes **local, or nowhere.**

Deliberately not built here: prompt versioning (a prompt is code and should be reviewed
like code), a fallback path when the local GPU is interrupted — which is exactly the
shape [Milestone 3](reducing-ai-infrastructure-costs-with-ec2-spot-instances.md) built —
and any form of RAG, which needs a vector store and is its own milestone.

Until then: the platform can run a model it owns, on hardware it controls, without the
prompt ever leaving the network — and it will tell you, from a single log line, when that
model is quietly running on a CPU.

---

*The implementation is in [`internal/llm`](../../internal/llm) (the provider abstraction)
and [`internal/ollama`](../../internal/ollama) (the client), driven by
[`cmd/llm`](../../cmd/llm). The reference is [INFERENCE.md](../../INFERENCE.md), and the
diagrams are in [ollama-diagrams.md](../architecture/ollama-diagrams.md).*
