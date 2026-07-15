# Hybrid AI Routing — One Interface, Two Machines

The platform runs its own inference against a self-hosted **Ollama** (Milestone 7) and
a managed **Amazon Bedrock** (Milestone 8, with **Claude** in Milestone 9). Milestone 10
stops making that an either/or. It runs **both**, and chooses between them **per request**:

```bash
LLM_PROVIDER=router
LLM_ROUTER_PROVIDERS=ollama,bedrock
```

The cheap, high-volume work — *"summarise this diff"* — stays on the local GPU, which is
already paid for by the hour and where **the prompt never leaves the network**. The work
that is worth a frontier model — *"draft the release notes"* — goes to Claude. When one
provider is down, the other takes over. And a request that must not leave the VPC is
**refused** rather than sent to a hosted model, whatever else is happening.

> **The router is not a new kind of thing.** It **is** an `llm.Provider` — the same
> interface Ollama and Bedrock implement — so nothing above it changed. `llm.Service`, the
> tool loop, the prompt catalogue and every caller are holding one interface and cannot tell
> there are two models behind it. Milestone 7 predicted this exact outcome, in
> `internal/providers`, before there was anything to route.

> **This repository deploys no model.** Ollama's instance, GPU and models belong to
> `ollama-on-aws`; Bedrock is AWS's to run. This repository owns **the routing layer that
> chooses between them** — and it never learns which providers exist, which is what makes
> adding a third a one-line change.

The *why* is in the blog post,
[Building Hybrid AI Workflows with Ollama and Amazon Bedrock](docs/blog/building-hybrid-ai-workflows-with-ollama-and-amazon-bedrock.md).

## Contents

- [Why hybrid, and why per request](#why-hybrid-and-why-per-request)
- [The router is a provider](#the-router-is-a-provider)
- [The request lifecycle](#the-request-lifecycle)
- [Preference vs constraint: the idea the milestone turns on](#preference-vs-constraint-the-idea-the-milestone-turns-on)
- [Routing strategies](#routing-strategies)
- [The fallback strategy](#the-fallback-strategy)
- [Health: what makes fallback affordable](#health-what-makes-fallback-affordable)
- [The three things that must never fail over](#the-three-things-that-must-never-fail-over)
- [Capabilities: a union, except the one that is a guarantee](#capabilities-a-union-except-the-one-that-is-a-guarantee)
- [Configuration](#configuration)
- [IAM and security](#iam-and-security)
- [Observability](#observability)
- [Adding a provider](#adding-a-provider)
- [Local development](#local-development)
- [Testing](#testing)
- [Troubleshooting](#troubleshooting)

## Why hybrid, and why per request

A single provider forces a single compromise. Local inference is private and, on hardware
you are already renting, effectively free — but a 7B model is not Claude. A hosted frontier
model is far more capable — but every prompt leaves the network and every token is billed.
Picking one for the whole platform means either paying Claude's price to summarise a diff,
or handing your release notes to a 3B model. Both are the wrong trade for **half** the work.

The platform's inference is not homogeneous, and that is the whole opening:

| Work | Volume | Value of a better model | Right home |
| --- | --- | --- | --- |
| Summarise a diff, classify an event | high | low — a 7B model is fine | **Ollama** |
| Draft release notes, write a post | low | high — visibly better output | **Bedrock (Claude)** |
| Anything touching a private repo | any | — | **Ollama** (the prompt must not leave) |

The GPU is paid for hourly whether it is busy or idle, so the cheap work is *free* on Ollama
in the strict sense that moving it elsewhere saves nothing — while the expensive work is
worth paying for. Routing **per request** is what lets the platform take the good half of
each provider instead of the average of both. It is also higher-availability than either
alone: a local GPU and a managed API do not fail at the same time or for the same reasons,
so each is the other's backup.

## The router is a provider

This is the entire design, and it is one sentence: **`router.Router` implements
`llm.Provider`.**

```
                         ┌───────────────────────────────┐
   llm.Service  ───────► │        llm.Provider           │  ◄─── it holds ONE of these
   (validate, log,       └───────────────────────────────┘       and cannot tell which
    correlate, time)                    ▲
                          ┌─────────────┼─────────────┐
                          │             │             │
                    ollama.Client  bedrock.Client  router.Router
                                                        │  ← also an llm.Provider…
                                          ┌─────────────┴─────────────┐
                                    ollama.Client              bedrock.Client
```

Everything above the interface — the service that logs and correlates, the tool loop, the
prompt catalogue, the CLI, n8n — talks to an `llm.Provider` and does not care what is behind
it. A `Router` goes in the slot a single client used to occupy, and the platform cannot tell
the difference. That is the only test of a provider abstraction that means anything, and it
is enforced mechanically: `internal/architecture_test.go` fails the build if the router ever
imports a vendor, or if `router.Router` ever stops satisfying `llm.Provider`.

## The request lifecycle

```
Workflow (n8n)
   │
   ▼
OpenClaw / the platform            ← neither knows a router exists
   │
   ▼
llm.Service                        ← validates, checks the context window, correlates, logs
   │
   ▼
router.Router  ── CONSTRAIN ──►    which providers CAN serve this at all?   (a hard gate)
   │            ── SELECT   ──►    of those, which one SHOULD?              (the strategy)
   │            ── ORDER    ──►    and if it fails, who is next?           (health + fallback)
   │            ── EXECUTE  ──►    try them, in order, at most once each
   ▼
Selected provider  →  model inference  →  structured response
   │
   ▼
Workflow completion
```

The order is load-bearing. **Constraints are applied first, to every request, before any
strategy or configuration is consulted** — because a strategy is an opinion about which
provider is *better*, and a constraint is a statement about which providers are
*permissible*. Letting a preference overrule a constraint is how a platform configured for
cost ends up sending a private repository to a hosted model during an outage.

## Preference vs constraint: the idea the milestone turns on

Two request fields both influence *where* a request runs, and the difference between them is
the most important idea in the whole milestone.

```go
req.Provider     = "bedrock"   // a PREFERENCE made explicit — "send this one to Bedrock"
req.RequireLocal = true        // a CONSTRAINT — "this prompt may not leave the network"
```

- **A preference bends.** If the named provider cannot do the job — Ollama asked for tools,
  a small model asked for a 100k-token prompt — the router says so and picks one that can.
  Pretending otherwise produces a confident wrong answer, which is worse than a reroute.
  *(A pinned `req.Provider` is the exception that does not silently reroute — see below —
  but it still gives way to a hard constraint.)*
- **A constraint does not bend.** `RequireLocal` refuses any hosted provider — whatever the
  configuration says, whatever the strategy prefers, **and whether or not fallback is
  enabled**. If that leaves nothing to serve the request, the request is **refused**, and
  being refused is the correct outcome. The thing on the other side of that decision is not
  a slower answer or a bigger bill; it is somebody's source code in a third party's service.

`req.Provider` (the manual override) sits in between: it pins the request to one provider and
will **not** fall back — someone who names a provider would rather have an error than a
surprise — but if that provider cannot serve the request it is refused, not quietly sent
elsewhere.

> The strongest privacy control is not a field at all. `LLM_ROUTER_PROVIDERS=ollama` builds
> **no Bedrock client in the process**, so a prompt cannot reach Bedrock by misconfiguration
> or by a bug in a strategy — there is nothing to send it with. A guarantee enforced by
> absence beats one enforced by an `if`.

## Routing strategies

Set with `LLM_ROUTER_STRATEGY`. There are two, and between them they cover every selection
mode the brief asks for — because several of those "modes" are one mechanism with different
configuration.

### `fixed` (the default)

Every request goes to `LLM_ROUTER_DEFAULT` (which defaults to the first provider listed).

```bash
LLM_ROUTER_STRATEGY=fixed  LLM_ROUTER_DEFAULT=ollama    # always Ollama
LLM_ROUTER_STRATEGY=fixed  LLM_ROUTER_DEFAULT=bedrock   # always Bedrock
```

"Always Ollama", "always Bedrock", and "provider selected by configuration" are **not three
strategies**. They are this one strategy and two values of one variable. The boring option is
the default on purpose: the first thing you want from a router is that it be predictable.

### `purpose`

Route on what the inference is **for** — `llm.Purpose`, which every request has carried since
Milestone 7 (`release-notes`, `diff-summary`, `change-triage`).

```bash
LLM_ROUTER_STRATEGY=purpose
LLM_ROUTER_RULES=release-notes=bedrock,diff-summary=ollama
```

An unruled purpose falls through to the default. **"Route by workflow" and "route by task
type" are this same lookup** — the workflow is what *sets* the purpose (n8n triggers
`release-notes`, and `release-notes` is the purpose the request carries), so adding a
separate field to route on would mean adding a field nothing populates in order to look like
it does something the platform can already do.

This is the four lines of code that collect the economics: cheap, high-volume purposes to the
free local GPU, expensive low-volume ones to Claude.

### What is *not* a strategy

A capability that no provider has is **refused**, not routed. A request for tools, with only
Ollama enabled, returns `ErrNoProvider` naming each provider and why — it is not sent to a
model that will ignore the tools and answer from memory. Capability-aware and context-aware
routing are not features anyone had to build: they fall out of the constraint gate, which
excludes any provider that cannot serve the request before the strategy is ever consulted.

## The fallback strategy

`LLM_ROUTER_FALLBACK=true` (the default). When the chosen provider fails, the router tries the
next one that can serve the request.

```
Bedrock throttled under load        Ollama (GPU) reclaimed as Spot
        │                                   │
        ▼                                   ▼
   fall back to Ollama                 fall back to Bedrock
```

Fallback is **direction-agnostic** and it is the payoff of running two providers: a managed
API and a local GPU do not fail together. Bedrock throttles you exactly when you are busiest;
the Spot GPU is reclaimed with two minutes' notice (see [infra/SPOT.md](infra/SPOT.md)). Each
event is uncorrelated with the other, and fallback is what turns that into availability.

**It cannot loop.** The fallback chain is built as a subset of the enabled providers with
each appearing **at most once**, and the executor walks it forwards and stops. There is no
depth counter to forget and no visited-set to maintain — a duplicate in
`LLM_ROUTER_PROVIDERS` is rejected at start-up precisely so the chain cannot try a provider
twice.

**It does not fall over from a deterministic failure.** A malformed request, a prompt that is
too big, a schema the model violated — asking a second, more expensive model the same bad
question gets the same answer and a second bill. The router only fails over from failures
that are the *provider's* fault (unavailable, timeout, throttled, stalled, auth), where
"somebody else might succeed" is actually true.

Set `LLM_ROUTER_FALLBACK=false` when you would rather have a clean, cheap failure than a
silent and more expensive success.

## Health: what makes fallback affordable

Fallback works fine without health tracking — and the platform is unusable anyway. Here is
the arithmetic that makes health non-optional:

```
BEDROCK_TIMEOUT        = 2m   (default)
BEDROCK_RETRY_ATTEMPTS = 3    (default)
```

A provider that is down does not fail fast. It fails **slowly, three times**, and only then
does the router try the other one. Without memory, *every single request* pays several
minutes to rediscover an outage that is already hours old — before it even starts the work.
A ten-second summarisation now takes six minutes. The fallback is functioning; the platform
is down.

So the router **remembers**. After `LLM_ROUTER_HEALTH_THRESHOLD` consecutive provider-level
failures (default 2), a provider is moved to the **back** of the chain for
`LLM_ROUTER_HEALTH_COOLDOWN` (default 30s). The cost of an outage is paid a couple of times,
not on every request.

Two design choices are worth stating because the obvious alternatives are traps:

- **Demoted, never removed.** An unhealthy provider goes behind the others; it is never taken
  out. A breaker that *removes* providers has a state — "everything is unhealthy" — in which
  it refuses all traffic, and that state is reachable from something as ordinary as a DNS
  blip failing one request to each provider. A router with two working models returning
  errors to everyone because it decided the world ended is the exact failure a breaker exists
  to prevent. The failure mode of demotion is milder: a request occasionally goes to a
  provider that is still slow.
- **It observes; it does not poll.** Health comes from the real requests the platform was
  making anyway. There is no goroutine probing Bedrock every ten seconds — that costs money
  on an idle platform and tells you nothing about the request you are *about* to make.
  `llm route` runs an active probe when a human actually wants one.

When the cooldown expires, exactly **one** request is allowed through to test whether the
provider is back (a half-open probe), so a recovering provider is retried by one request
rather than stampeded by all of them at once.

## The three things that must never fail over

A router's real danger is not picking the wrong provider. It is that a **retry happens where
retrying is unsafe** — and Milestone 9 built exactly such places. The router refuses all
three *structurally*, rather than by trusting a provider to have returned the right error.

1. **A stream that has already emitted a token.** The caller holds the beginning of an
   answer; a second provider would hand them the beginning of a *different* one, concatenated
   onto the first, with no error anywhere. The router wraps the sink and **counts what
   actually reached the caller** — if anything did, it will not fail over, *whatever error
   came back*. It does not rely on the provider having wrapped it as `ErrStreamBroken`,
   because a provider written next year is one forgotten error-wrap away from this bug.

2. **A conversation in which a tool has already run.** The world has moved — an n8n workflow
   is running, a pull request exists. "Try Bedrock instead" means doing it again. This is
   `llm.ErrEffectsCommitted`, and it is terminal.

3. **A tool-using conversation cannot change provider *at all*, even between turns, even when
   nothing failed.** This is the subtle one, and the bug that would otherwise have shipped.
   `llm.Service.Converse` runs a tool loop by calling `Generate` once per turn, replaying the
   whole conversation each time — so to the router those turns look like independent requests.
   The naive router sends turn 1 to Bedrock and turn 4 to Ollama, carrying:
   - **Claude's signed reasoning block** — an opaque, Bedrock-issued signature that Bedrock
     *demands back verbatim* on the next turn, and that Ollama has never seen and cannot
     produce;
   - **Bedrock's tool-call IDs** — to Ollama, references to calls it never made;
   - **half a chain of thought from a frontier model**, handed to a 7B one to finish.

   So a conversation is **pinned** to the provider that started it and does not fall over. If
   that provider dies mid-conversation, the request fails — and failing is right, because the
   conversation's state cannot migrate, so there is nothing to fail over *to*. The caller
   retries from the top, which is safe exactly when no `Write` tool has run — which is the
   distinction Milestone 9 already gave them in `llm.ErrEffectsCommitted`.

   It is **detected, not declared**: a request whose history contains an assistant turn with
   tool calls or reasoning *is* a continuation, whether or not anyone remembered to flag it. A
   rule that cannot be forgotten beats a `ConversationID` field that can — and it keeps
   routing's vocabulary out of `internal/llm`, which must not know a router exists.

## Capabilities: a union, except the one that is a guarantee

A router reports the fleet's capabilities, and the rule for combining several providers into
one answer is not "merge them":

- **A capability is a union** — the router can do it if **any** provider can. It reports
  `Tools: true` because Bedrock can, even though Ollama cannot; a tool request is routed to
  Bedrock. Reporting the *intersection* would mean that adding a small local model *removed*
  the platform's ability to use tools.
- **`Local` is a guarantee, so it is an intersection** — it means *the prompt does not leave
  the network*, and a router that might send this one to Bedrock cannot promise that. It is
  true only when **every** enabled provider is local. Getting this backwards is a security
  bug, not a rounding error.
- **Cost is reported pessimistically** — the *worst* price in the fleet, because the tool
  loop's budget uses it, and under-estimating lets a conversation run past a cap somebody set
  deliberately.

## Configuration

Everything is an environment variable. The router holds **no endpoint, region, model ID,
timeout or credential** — those belong to the providers (`OLLAMA_*`, `BEDROCK_*`) and stay
there, because a router that had to be told Bedrock's region would be a router that knew
Bedrock exists.

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `LLM_PROVIDER` | ✅ | `ollama` | Set to `router` to turn on routing. |
| `LLM_ROUTER_PROVIDERS` | | *(all known)* | Enabled providers, in preference order: `ollama,bedrock`. **The strongest privacy control** — an omitted provider is not built. |
| `LLM_ROUTER_STRATEGY` | | `fixed` | `fixed` or `purpose`. |
| `LLM_ROUTER_DEFAULT` | | *(first listed)* | Where requests land when the strategy has no opinion. Must be enabled. |
| `LLM_ROUTER_RULES` | ¹ | — | `purpose=provider,…` for the `purpose` strategy. |
| `LLM_ROUTER_FALLBACK` | | `true` | Fail over when a provider is down. |
| `LLM_ROUTER_HEALTH_THRESHOLD` | | `2` | Consecutive failures before a provider is demoted. Min 1. |
| `LLM_ROUTER_HEALTH_COOLDOWN` | | `30s` | How long a demoted provider stays at the back. |

¹ Required when `LLM_ROUTER_STRATEGY=purpose` — a purpose strategy with no rules is a fixed
strategy in a costume, and the config refuses to load rather than let you find that out by
watching every request go to the default.

Plus the providers' own configuration — see [INFERENCE.md](INFERENCE.md). A router over both
needs a valid Ollama **and** a valid Bedrock configuration, and if either will not build,
**the router will not boot**: "degraded" is a state you fall into, not one you start in.

## IAM and security

The router adds **no new permissions and no new credentials.** It calls nothing itself — each
provider authenticates as it always did:

- **Ollama** — reached over the VPC network; an optional bearer token behind a reverse proxy
  (`OLLAMA_TOKEN`). The prompt never leaves the network.
- **Amazon Bedrock** — AWS IAM, resolved through the SDK's default credential chain (the EC2
  instance role in production). **There is no static key.** The two actions the instance role
  needs are unchanged from Milestone 8:

  ```json
  {
    "Effect": "Allow",
    "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"],
    "Resource": "arn:aws:bedrock:*::foundation-model/anthropic.claude-*"
  }
  ```

  Scoped to the model — least privilege — and separate from the per-model **access grant**,
  which is an entitlement requested in the Bedrock console, not an IAM action. See
  [INFERENCE.md](INFERENCE.md#the-two-permissions-bedrock-needs).

The security properties that matter to routing:

- **`RequireLocal` is a hard egress control**, enforced before any strategy or fallback. A
  prompt that must not leave cannot leave — even during an outage, even with fallback on.
- **A disabled provider is not built**, so a provider you have not enabled cannot be reached
  by any code path.
- **Inputs and outputs are validated by `llm.Service`, once, above the router** — the same
  context-window check and capability refusal every provider already got, so routing cannot
  create a path that skips them.
- **The routing table holds no secret**, and `llm route` prints it in full to prove it.

## Observability

Structured logs, designed for CloudWatch. Every line `llm.Service` writes carries
`provider=router` — which is why `llm.Response.Provider` exists to record **who actually
answered**, without which no bill or latency spike could be attributed.

```json
{"level":"INFO","msg":"route selected","correlationId":"push:delivery-abc-123","workflowExecutionId":"n8n-exec-42","purpose":"release-notes","strategy":"purpose","provider":"bedrock","reason":"purpose \"release-notes\" is routed to bedrock","chain":["bedrock","ollama"],"fallbackEnabled":true,"excluded":[]}
{"level":"WARN","msg":"the provider failed; falling over to the next one","provider":"bedrock","next":"ollama","error":"llm provider is throttling us","errorKind":"throttled","durationMs":812}
{"level":"INFO","msg":"route completed","servedBy":"ollama","model":"llama3.2","durationMs":2140,"attempts":1,"fallback":true,"attempted":["bedrock","ollama"]}
```

The fields a routing question actually needs:

| Field | Answers |
| --- | --- |
| `strategy`, `reason` | *Why* did this go where it went? Not just where. |
| `provider` / `servedBy` | Who was chosen, and who actually answered (they differ on a fallback). |
| `chain`, `attempted` | The fallback order, and what was really tried. |
| `fallback: true` | This request was served by a **backup** — the primary is in trouble. |
| `excluded` | Which providers were ruled out by the constraint gate, and why. |
| `errorKind` | The sentinel's name (`throttled`, `no_provider`, …), stable for alerts. |

Two CloudWatch Logs Insights queries you will actually run:

```
fields correlationId, purpose, servedBy, fallback, durationMs
| filter msg = "route completed"
| stats count() as requests, sum(fallback) as fell_back by servedBy, purpose
```

```
fields provider, errorKind
| filter msg like /falling over/ or errorKind = "no_provider"
| stats count() by provider, errorKind
```

`errorKind = "no_provider"` is the one to alert on differently: it is **not an outage**. It
means no provider can serve the request and none will until a human changes the configuration
— a retry loop that mistook it for `unavailable` would ask the fleet the same impossible
question forever.

## Adding a provider

This is the claim the milestone is really making. To add Amazon Nova, Mistral, an OpenAI or
Azure OpenAI client:

1. Write a type that implements `llm.Provider` (five methods), in its own `internal/…`
   package. It reports its own `Capabilities` — is it local, what does it cost, how big is
   its window, can it use tools — and the router reads *those*, never the package's name.
2. Add one `case` to `build` in `internal/providers`.

That is the whole list. **The router is not in it, and neither is the service, the tool loop,
or any caller** — they all take the interface. The router routes between whatever map it is
handed and would route between five providers, or two Bedrock models, without a line
changing. `internal/architecture_test.go` proves the router cannot import a vendor, so this
stays true rather than merely being intended.

## Local development

```bash
export LLM_PROVIDER=router
export LLM_ROUTER_PROVIDERS=ollama,bedrock
export LLM_ROUTER_STRATEGY=purpose
export LLM_ROUTER_RULES=release-notes=bedrock,diff-summary=ollama

# the providers' own config still applies:
export OLLAMA_BASE_URL=http://localhost:11434
export OLLAMA_MODEL=llama3.2
export BEDROCK_MODEL_ID=us.anthropic.claude-3-5-haiku-20241022-v1:0

go run ./cmd/llm route                                   # the table + a health probe
go run ./cmd/llm route --no-probe                        # the table only, no network

go run ./cmd/llm generate --purpose diff-summary  --prompt "…"   # → ollama (a rule)
go run ./cmd/llm generate --purpose release-notes --prompt "…"   # → bedrock (a rule)
go run ./cmd/llm generate --provider bedrock      --prompt "…"   # pin it; no fallback
go run ./cmd/llm generate --local                 --prompt "…"   # refuse to leave the network
```

`generate` prints `--- served by: bedrock ---` when the router sent it somewhere other than
the configured provider, so you can watch a routing decision happen.

## Testing

```bash
go test ./internal/router/                    # the logic, against fakes, in milliseconds
go test ./internal/ -run Architecture         # the seam: the router cannot import a vendor
go test -race ./...

# real Ollama + real Bedrock, opt-in:
OLLAMA_BASE_URL=… OLLAMA_MODEL=… BEDROCK_MODEL_ID=… \
  go test -tags=integration ./internal/router/ -v
```

The whole router is tested against fake providers with **no HTTP anywhere** — because routing
is a *decision*, and a decision can be checked with a struct literal. What the tests pin down:

| | |
| --- | --- |
| **A router is an `llm.Provider`** | It goes into `llm.Service` and the service cannot tell. |
| **`RequireLocal` is never traded away** | Not by a strategy, not by the default, and **not by fallback when the local provider is down** — the single worst thing this package could do. |
| **A capability nobody has is refused** | `ErrNoProvider`, naming each provider and why — not sent to a model that will fake it. |
| **Fallback works both directions, and cannot loop** | Each provider tried at most once; the last error still wraps through. |
| **A deterministic failure does not fall over** | A bad request is not retried against a second, pricier model. |
| **A stream that has spoken is never failed over** | Even when the provider returns a plain `ErrUnavailable` rather than `ErrStreamBroken`. |
| **A conversation never changes provider** | The signed reasoning block and tool-call IDs cannot migrate; a fresh turn with the same tools still routes normally. |
| **A pin does not fall over, and is refused if it cannot serve** | An override that gets overridden is not one. |
| **A failing provider is demoted next request** | And a missing *model* does **not** condemn the provider that answered honestly. |
| **The active probe generates no tokens** | It lists models — the cheapest call that exercises the whole path. |

## Troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| `llm route` says "not a router" | `LLM_PROVIDER` is not `router`. A single provider is a valid setup; routing is opt-in. |
| **`ErrNoProvider`** on every request | Not an outage. No enabled provider can do what is asked — read the reasons in the error (it names each one). Usually: tools requested with only Ollama enabled, or a prompt bigger than every window. Fix the configuration, not the code. |
| `RequireLocal` requests fail | No **local** provider is enabled. It is refused rather than sent to a hosted one — that is the point. Enable Ollama in `LLM_ROUTER_PROVIDERS`, or, if this prompt may leave, do not set `--local`. |
| A pinned request errors instead of falling back | By design. `req.Provider` (`--provider`) does not fall over — drop it to allow routing. |
| Everything is slow when one provider is down | The health cooldown has not tripped yet, or `LLM_ROUTER_HEALTH_THRESHOLD` is high, so requests still try the dead provider first and pay its timeout. Lower the threshold, or wait for the breaker. |
| The router will not boot | A member did not build. A router needs **every** enabled provider valid — usually a missing `BEDROCK_MODEL_ID`. The error names the offender. |
| A conversation failed mid-way and did not fall over | Also by design — a tool-using conversation carries provider-specific state that cannot migrate. Retry from the beginning; safe iff no `Write` tool ran (`ErrEffectsCommitted` tells you which). |
| `llm route` shows a provider `reachable but has no models` | It is up and empty — an Ollama nobody ran `ollama pull` on. Pull a model. |
| Logs all say `provider=router` and nothing else | Look at `servedBy` on the `route completed` line — that is who actually answered. |
