# Building Hybrid AI Workflows with Ollama and Amazon Bedrock

> **Milestone 10 — Hybrid AI Routing.**
> This milestone lets the platform run a self-hosted **Ollama**
> ([Milestone 7](running-local-llms-with-ollama-on-aws.md)) and a managed **Amazon Bedrock**
> ([Milestone 8](adding-amazon-bedrock-to-an-ai-agent-platform.md), with **Claude** in
> [Milestone 9](integrating-claude-into-an-ai-agent-platform.md)) **at the same time**, and
> choose between them **per request**. It builds a routing layer, a fallback mechanism, and a
> health circuit breaker. It does **not** build RAG, Knowledge Bases, Bedrock Agents,
> Guardrails, cost-aware or latency-aware routing, or model benchmarking — those are later.
> The code is in [`internal/router`](../../internal/router) and
> [`internal/providers`](../../internal/providers).

*Audience: engineers who have two ways to run a model and are about to write an `if` that
picks one. This is the milestone where that `if` grows a fallback path, a circuit breaker,
and three separate ways to corrupt an answer — and where almost none of it ends up in the
`if`.*

---

## Contents

- [The design was finished three milestones ago](#the-design-was-finished-three-milestones-ago)
- [The router is a provider, and that is the whole trick](#the-router-is-a-provider-and-that-is-the-whole-trick)
- [Preference is not constraint](#preference-is-not-constraint)
- [Four strategies that are really two](#four-strategies-that-are-really-two)
- [Fallback is easy; affordable fallback is the work](#fallback-is-easy-affordable-fallback-is-the-work)
- [The three retries that must never happen](#the-three-retries-that-must-never-happen)
- [The bug that would have shipped](#the-bug-that-would-have-shipped)
- [A capability is a union; a guarantee is not](#a-capability-is-a-union-a-guarantee-is-not)
- [The router never learns a vendor's name](#the-router-never-learns-a-vendors-name)
- [Testing a router without a GPU or an AWS account](#testing-a-router-without-a-gpu-or-an-aws-account)
- [Lessons learned](#lessons-learned)
- [What comes next](#what-comes-next)

## The design was finished three milestones ago

The most satisfying thing about this milestone is how little of it was new. When I added
Ollama in Milestone 7 — with exactly one provider and nothing to route — I wrote this in
`internal/providers`, and then left it alone for three milestones:

> It is not a router. It picks a provider from configuration, once, at start-up, and then
> gets out of the way. Choosing a provider *per request* … is Milestone 10, and it will
> implement `llm.Provider` itself and sit exactly where a single provider sits today.
> **Nothing above will notice.**

Nothing above noticed. `llm.Service`, the tool loop, the prompt catalogue, the CLI, n8n —
none of them changed. The router went into the slot an Ollama client used to occupy, and the
platform could not tell the difference. That is not a coincidence and it is not luck; it is
the interest paid on an abstraction that was built one implementation early, on purpose,
against the day there would be two.

The lesson I keep relearning on this platform: the time to write the seam is *before* you can
prove you need it, because afterwards it is a rewrite. Milestone 10 is the milestone where I
found out whether the bet from Milestone 7 paid off. It did — the whole "provider abstraction"
grew by four struct fields and one error.

## The router is a provider, and that is the whole trick

`router.Router` implements `llm.Provider` — the same five-method interface Ollama and Bedrock
implement. So a router is a provider that happens to hold other providers:

```go
type Router struct {
    providers map[string]llm.Provider  // it is handed these; it does not build them
    order     []string
    strategy  Strategy
    health    *Health
}

func (r *Router) Generate(ctx context.Context, req llm.Request) (llm.Response, error) { … }
func (r *Router) Stream(ctx context.Context, req llm.Request, sink llm.Sink) (llm.Response, error) { … }
```

Everything else follows from that one decision. Because the router is *just* a provider, the
service above it still validates every request, still checks the context window, still logs
and correlates — once, for all providers, exactly as before. The router does not re-implement
any of it, and cannot accidentally skip it. And because it is handed a `map[string]llm.Provider`
rather than a configuration, it has no way to construct an Ollama or a Bedrock client — which
turns out to be the thing that keeps it honest for the rest of the milestone.

## Preference is not constraint

Here is the distinction the whole milestone turns on, and the one I most wanted to get right.
Two request fields both affect *where* a request runs:

```go
req.Provider     = "bedrock"   // a PREFERENCE — "send this one to Bedrock"
req.RequireLocal = true        // a CONSTRAINT — "this prompt may not leave the network"
```

They feel similar and they are not the same kind of thing at all. A preference is an opinion
about which provider is *better*. A constraint is a statement about which providers are
*permissible*. And the difference has to be visible in the code, because the failure modes are
not comparable:

- If a preference cannot be honoured — Ollama asked for tools it does not have — the right
  move is to route elsewhere and say so. Forcing it produces a confident wrong answer.
- If a constraint cannot be honoured — `RequireLocal` and every provider is hosted — the right
  move is to **refuse**. There is no "route elsewhere" that is correct, because the whole point
  of the constraint is that "elsewhere" is where the prompt must not go.

So the router applies constraints *first*, to every request, before any strategy or fallback
runs. The test I care about most in the package is the one that turns fallback on, takes the
local provider down, leaves a perfectly good hosted provider running, and asserts that a
`RequireLocal` request is **refused anyway**:

```go
// Fallback is ENABLED, a working provider is right there, and the router MUST refuse it.
if bedrock.count() != 0 {
    t.Fatal("the router FELL BACK to a hosted provider with a prompt that required local " +
        "inference. An outage is not a reason to relax a privacy constraint")
}
```

An outage is not a reason to send somebody's source code to a third party. That sentence is
the whole security posture of a hybrid platform, and if it is not enforced in code it is just
a sentence.

The strongest version of the control, though, is not a runtime check at all. `LLM_ROUTER_PROVIDERS=ollama`
builds no Bedrock client in the process — so a prompt cannot reach Bedrock by misconfiguration
or by a bug in a strategy, because there is nothing in memory to send it with. A guarantee
enforced by absence beats one enforced by an `if`.

## Four strategies that are really two

The brief asked for six selection modes: always Ollama, always Bedrock, by configuration, by
workflow, by task type, and a manual override. I shipped two strategies and a field, because
most of those "modes" are one mechanism wearing different clothes.

"Always Ollama", "always Bedrock", and "by configuration" are `fixed` with different values of
`LLM_ROUTER_DEFAULT`:

```bash
LLM_ROUTER_STRATEGY=fixed  LLM_ROUTER_DEFAULT=ollama    # always Ollama
LLM_ROUTER_STRATEGY=fixed  LLM_ROUTER_DEFAULT=bedrock   # always Bedrock
```

"By workflow" and "by task type" are both `purpose` — routing on `llm.Purpose`, the field
every request has carried since Milestone 7 so that logs could answer "what is this platform
spending its tokens on?". It turns out the question "what is this *for*?" is exactly the right
routing key, because it is the same question that decides both what the work costs and where it
should run:

```bash
LLM_ROUTER_STRATEGY=purpose
LLM_ROUTER_RULES=release-notes=bedrock,diff-summary=ollama
```

The workflow is what *sets* the purpose — n8n triggers `release-notes`, and that is the purpose
the request carries — so "route by workflow" needs no new field. Adding one would mean
populating a `Request.Workflow` that duplicates information already present, in order to look
like it does something the platform can already do. And the manual override is a field, not a
strategy, because it has to beat whatever strategy is configured.

Shipping four classes that differ only by a string would have made the routing layer look
richer than it is, and it would have been the same code four times. The extensibility that
matters is not "how many strategies ship" — it is whether a fifth can be added without touching
anything else, which is what `Strategy` being a two-method interface is for.

The one design decision I want to defend: `Strategy.Select` returns **no error**. It is handed a
non-empty list of candidates that are *already* known to be capable of serving the request — the
router applied the constraints first — so there is no failure left for it to have. A routing
layer sits in front of every inference the platform makes; a strategy that could return an error
would be a brand-new way for the entire inference plane to go down, added in exchange for
nothing. The type system removes the option.

## Fallback is easy; affordable fallback is the work

Fallback itself is a `for` loop over a chain of providers. I had it working in an afternoon, and
the platform was still unusable, and the reason is arithmetic:

```
BEDROCK_TIMEOUT        = 2m   (default)
BEDROCK_RETRY_ATTEMPTS = 3    (default)
```

A provider that is down does not fail *fast*. It fails slowly, three times, over several
minutes, and only then does the loop move on. Without any memory, **every single request** pays
that cost to rediscover an outage that is already hours old — and it pays it *before* starting
the actual work, so a ten-second summarisation now takes six minutes. The fallback is
functioning perfectly. The platform is down.

So the router needs a circuit breaker, and building one taught me that the obvious circuit
breaker is dangerous. The obvious design *removes* an unhealthy provider from rotation. But that
gives the breaker a state — "everything is unhealthy" — in which it refuses all traffic, and
that state is reachable from something as small as a DNS blip failing one request to each
provider at the same moment. A router with two working models behind it, returning errors to
everyone because it decided on the strength of two timeouts that the world had ended, is the
exact outage a breaker exists to prevent.

So my breaker *demotes* rather than removes: an unhealthy provider goes to the back of the
chain, never out of it. The worst case of demotion is that a request occasionally goes to a
provider that is still slow. The worst case of removal is total outage caused by the component
whose only job is preventing one. Those are not comparable.

The other thing I resisted was a background health-poller. It is tempting — a goroutine that
pings each provider every ten seconds and keeps a fresh health map. But it costs money on an
idle platform, it tells you nothing about whether the *next* request will work, and it is one
more thing to get wrong. So health is observed from the real traffic the platform was making
anyway. The only active probe is `llm route`, and a human runs that.

## The three retries that must never happen

Here is where a router stops being a load balancer. A router's real danger is not picking the
wrong provider — it is that a **retry happens somewhere retrying is unsafe**. And Milestone 9,
by giving the model tools and reasoning, had quietly built three such places while I was not
looking.

**One: a stream that has already emitted a token.** The caller holds the beginning of an
answer. Fail over now and a second provider hands them the beginning of a *different* answer,
silently concatenated onto the first. `llm.ErrStreamBroken` exists to signal this, and a correct
provider returns it — but I refused to rely on that. The router wraps the sink and counts what
actually reached the caller, and will not fail over if the count is non-zero *whatever error
came back*, because "the provider will return the right error" is not a good enough reason to
risk a corrupted answer from a provider I might not have written.

**Two: a conversation in which a `Write` tool has run.** The world has moved — an n8n workflow
is running, a pull request exists. This is `llm.ErrEffectsCommitted`, it is terminal, and
Milestone 9 already established it.

**Three** is the one worth its own section.

## The bug that would have shipped

`llm.Service.Converse` runs a tool loop by calling `Generate` once per turn, replaying the whole
conversation each time. From the router's point of view, those turns are *separate, independent
requests*. Nothing in the type system connects them. So the obvious router — route each request,
fall over when one fails — will cheerfully send turn 1 to Bedrock and, when Bedrock hiccups on
turn 4, send turn 4 to Ollama.

That is not a slightly worse answer. It is incoherent, in three separate ways:

- **Claude's reasoning is signed.** The `ReasoningBlock` carries an opaque, Bedrock-issued
  signature that Bedrock *demands back verbatim* on the next turn. Ollama has never seen it,
  cannot verify it, and will not produce one — so the turn after the switch cannot be continued
  at all.
- **Tool-call IDs belong to the provider that issued them.** The results being replayed are
  keyed to IDs Bedrock invented; to Ollama they are references to calls it never made.
- **The models are different models.** Half a chain of thought from a frontier model, handed to
  a 7B one to finish, produces something that reads like reasoning and is not.

So a conversation in progress is **pinned** to the provider that started it, and does not fall
over. If that provider dies mid-conversation, the request *fails* — and failing is correct,
because the conversation's state cannot migrate, so there is nothing to fail over to. The caller
retries from the top, which is safe exactly when no `Write` tool has run, which is the
distinction Milestone 9 already handed them.

The part I am most pleased with is that this is *detected*, not declared. My first instinct was
a `Request.ConversationID` set by the tool loop — but that would mean `internal/llm`, which must
not know routing exists, carrying a field whose only purpose is to inform a router. Detection
needs nothing added: a request whose history contains an assistant turn with tool calls or
reasoning *is* a continuation, whether or not anyone remembered to flag it. A rule that cannot be
forgotten beats a field that can.

This is the failure that would have shipped. It passes every unit test that mocks a single
provider. It only appears when a real conversation spans a real fallback, which is exactly the
situation you cannot reproduce on a laptop and would first meet in production, as a Claude
conversation that inexplicably falls apart on its fourth turn once a month.

## A capability is a union; a guarantee is not

A router has to answer `Capabilities()` for the whole fleet, and the rule for combining
providers is not "merge them". It is two rules pointing in opposite directions:

```go
caps.Tools = caps.Tools || c.Tools        // a CAPABILITY: the router can if ANY provider can
caps.Local = caps.Local && c.Local        // a GUARANTEE:  the router promises only if ALL do
```

The union is obvious once stated: the router reports `Tools: true` because Bedrock can use
tools, and it routes tool requests there. Reporting the intersection would mean that adding a
small local model *removed* the platform's ability to use tools — an absurd thing for adding a
provider to do.

`Local` is the opposite, and getting it backwards is a security bug rather than a rounding
error. `Local` means *the prompt does not leave the network*. A router that might send this one
to Bedrock cannot promise that, so it is true only when *every* enabled provider is local. A
router that reported `Local` because one of its providers happened to be would be lying about the
single thing anyone relies on it for.

Cost, I report pessimistically — the worst price in the fleet — because the tool loop's budget
uses it, and under-estimating lets a conversation run past a cap somebody set on purpose.

## The router never learns a vendor's name

The claim the milestone is really making is that adding a provider does not change the routing
layer. And the way that claim dies is not a bad architect — it is a good engineer in a hurry. The
router needs to know Bedrock supports tools and Ollama does not. The fact is *right there* in
`internal/bedrock`. One import gets it. It compiles. Every other test passes. And the routing
layer now cannot be built without the AWS SDK, cannot be tested without stubbing Bedrock, and
cannot route to a provider that does not exist yet.

The correct answer is the one the platform has had since Milestone 7: ask the interface.
`llm.Capabilities` is exactly the set of facts a router needs — is it local, what does it cost,
how big is its window, can it use tools — reported *by* the provider. A provider written next
year answers the same questions without the router learning its name.

So this is a test, not a paragraph:

```go
func TestTheRouterDoesNotKnowWhichProvidersExist(t *testing.T) {
    deps := transitiveImports(t, module+"router")
    for _, vendor := range []string{"ollama", "bedrock", "n8n", "openclaw"} {
        if deps[module+vendor] {
            t.Errorf("internal/router imports internal/%s …", vendor)
        }
    }
}
```

The strings "ollama" and "bedrock" appear in `internal/router` only inside comments. Adding
Amazon Nova, or Mistral, or an OpenAI client, means writing an `llm.Provider` and adding one
`case` to `internal/providers`. The router is not in the list of files you touch, and neither is
the service, the tool loop, or any caller — and the build fails if that ever stops being true.

## Testing a router without a GPU or an AWS account

The entire router is tested against fake providers, with no HTTP anywhere, in milliseconds —
because routing is a *decision*, and a decision can be checked with a struct literal. The fakes
are a name and a set of capabilities, which is all the router ever knew about a provider anyway:

```go
func local(name string) *fake  { return &fake{name: name, caps: llm.Capabilities{Local: true, MaxContextTokens: 8192}} }
func hosted(name string) *fake { return &fake{name: name, caps: llm.Capabilities{Tools: true, MaxContextTokens: 200_000, CostPer1MInputTokensUSD: 3}} }
```

That is not a testing convenience I bolted on; it is the same seam the architecture test
enforces, used from the other side. A router that could only be tested by standing up a real
GPU and a real AWS account would be a router that had learned what its providers *were* — and
the test being this cheap is the proof that it did not.

The questions a fake cannot answer — does a request really land on a local model on one machine
and a managed model in AWS on another, through one interface — get an integration test behind a
build tag, opt-in, that stands up both real providers. It is the one test file in the package
allowed to import a vendor, and only because it is asking the one question a fake cannot.

## Lessons learned

- **Build the seam one implementation early.** The router cost four struct fields and one error
  because the interface it needed was written in Milestone 7. Added now, on top of call sites
  that each knew a vendor's JSON, it would have been a rewrite.
- **A preference and a constraint are different types of thing.** Model them the same and you
  will eventually relax a privacy guarantee to improve availability, which is exactly backwards.
- **Fallback is a loop; affordable fallback is a circuit breaker** — and a circuit breaker that
  *removes* providers is a new way to cause the outage it exists to prevent. Demote, never
  remove.
- **The dangerous retry is the one that looks safe.** A stream that has spoken, and a
  conversation that has run a tool, both look like ordinary requests to a router. Refuse them
  structurally, not by trusting an upstream error string.
- **Detect state you cannot afford to have forgotten.** A conversation *is* a continuation if its
  history says so; a flag someone has to remember to set is a flag someone will forget.
- **Combine capabilities as a union and guarantees as an intersection**, and know which is which,
  because `Local` is the one where getting it backwards is a security bug.

## What comes next

The router was built to make later milestones cheap, and the shape is deliberately ready for
them without pretending to implement them:

- **Cost-aware and latency-aware routing** are new `Strategy` implementations. The candidates
  already carry cost in their `Capabilities`, and the interface already returns a `Decision` with
  a reason. Neither touches the router or the providers.
- **More Bedrock models, and more providers** — Amazon Nova, Meta Llama, Mistral, an OpenAI or
  Vertex AI client — are each one `llm.Provider` and one `case`. The routing layer does not move.
- **Policy-based routing and multi-model ensembles** are, again, strategies — the constraint gate
  and the fallback executor are the parts that would stay.
- **Monitoring** (Milestone 13) already has its raw material: every routing decision logs its
  reason, who was chosen, who actually answered, and whether it was a fallback.

Two models, one interface, chosen per request — with the cheap work staying home on hardware
already paid for, and the expensive work going where it is worth the money. That is the hybrid
platform the last four milestones were building toward, and the routing layer that finally joins
them up turned out to be, almost entirely, the interface I had already written.
