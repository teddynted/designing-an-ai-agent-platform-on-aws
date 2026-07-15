# Extensibility — the reference

**Milestone 17.** How the platform is extended with new AI providers, MCP servers,
vector databases, and services nobody has thought of yet — and, more importantly, why
none of those extensions touch the code that already exists. This is an
**architecture-only** milestone: it ships no infrastructure and no new runnable code.
It ships the *design* for extension, grounded in the seams the platform has enforced
since Milestone 7, and it names the extension points precisely enough that adding one
is a recipe rather than a research project. It is the operational companion to the
blog post,
[Extending an AI Agent Platform with New AI Providers and Services](docs/blog/extending-an-ai-agent-platform-with-new-ai-providers-and-services.md),
and the diagrams in
[extensibility-diagrams.md](docs/architecture/extensibility-diagrams.md).

> **The one idea.** The platform is extensible because the arrow points *inward*.
> Every integration is a **core** that owns the platform's side of a boundary — its
> types, its errors, its interface — and a **client** that implements that interface
> against one vendor. The core knows nothing about the client. So a new provider, a
> new tool, a new store is a new implementation of an existing interface plus one line
> in one factory — and the router, the loop, the service, and every caller are
> untouched. This is not aspiration: it is a set of build-failing tests in
> [`internal/architecture_test.go`](internal/architecture_test.go). Extensibility you
> can *assert* is extensibility a compiler defends.

---

## Contents

- [Why this milestone is architecture only](#why-this-milestone-is-architecture-only)
- [The extension model in one picture](#the-extension-model-in-one-picture)
- [The seams that already exist](#the-seams-that-already-exist)
- [Recipe: add a new LLM provider](#recipe-add-a-new-llm-provider)
- [Extension: MCP servers](#extension-mcp-servers)
- [Extension: vector databases](#extension-vector-databases)
- [Extension: any future AI service](#extension-any-future-ai-service)
- [What the extension points are, exactly](#what-the-extension-points-are-exactly)
- [What must NOT become an extension point](#what-must-not-become-an-extension-point)
- [Well-Architected](#well-architected)
- [Explicitly deferred](#explicitly-deferred)
- [Future improvements](#future-improvements)

> **On scope.** *Architecture only.* The Go snippets below are **design**, not code
> that ships in this milestone — they show the shape a future implementation takes,
> using the exact interfaces the platform already enforces. Where a type does not yet
> exist (a `VectorStore`, an MCP client), the snippet is a proposal drawn to fit the
> existing pattern, and it is labelled as such.

---

## Why this milestone is architecture only

Every prior milestone that shipped a capability also shipped the infrastructure to run
it. This one deliberately does not, and the reason is the honest one: **the
extensibility work was already done, milestone by milestone, and the remaining job is
to name it.** The provider abstraction (Milestone 7) was built before there was a
second provider specifically so the third and fourth would be cheap. The architecture
tests (Milestones 7–13) were written so the seams could not silently rot. What was
missing was not a mechanism — it was a document that says, in one place, *here is how
you extend this platform, here is the recipe, and here is why it costs one file and
not ten.*

Building a new provider or a vector store to prove the point would be inventing a
concrete integration this milestone's scope does not call for, and the platform has
enough real integrations already to prove the seam holds. So this milestone ships the
map, not another building. It is the difference between *"you could extend this"* and
*"here is precisely where, and what it will and will not cost you."*

Consequently there is **no `NN-extensibility.yaml` stack and no Makefile target** —
those exist for milestones that deploy infrastructure, and this one deploys nothing.
That is a deliberate deviation from the milestone template, not an omission.

## The extension model in one picture

Everything below is a consequence of one rule the platform has followed since
Milestone 5:

```
   internal/<core>     ← the PLATFORM's side of a boundary
        ▲                 (its types, its errors, its Service, its interface)
        │ implements
   internal/<client>   ← ONE vendor's implementation of that interface
                          (its HTTP, its auth, its retries, its quirks)
```

The dependency points **inward**: the client imports the core; the core never imports
the client. The platform's own vocabulary — `llm.Request`, `llm.ErrThrottled`,
`workflow.Service` — knows nothing about any specific vendor. And exactly one leaf
package, [`internal/providers`](internal/providers), is allowed to know that more
than one vendor exists; it is the single place a new one is registered.

That is the whole extension model. A new AI provider, a new tool, a new store all
follow it. The rest of this document is that sentence applied to four concrete cases.

## The seams that already exist

The platform already has these seams, each a core interface with one or more vendor
clients behind it, each guarded by a build-failing test:

| Core (platform side) | Interface | Clients today | Guarded by |
| --- | --- | --- | --- |
| [`internal/llm`](internal/llm) | `llm.Provider` | `ollama`, `bedrock` | `TestTheCoreNeverDependsOnItsVendor`, `TestTheRouterDoesNotKnowWhichProvidersExist` |
| [`internal/workflow`](internal/workflow) | `workflow.Service` | `n8n` | `TestTheCoreNeverDependsOnItsVendor` |
| [`internal/agent`](internal/agent) | `agent.Service` | `openclaw` | `TestTheCoreNeverDependsOnItsVendor` |
| [`internal/llm`](internal/llm) (tools) | `llm.ToolRunner` | `tools` | `TestTheInferencePlaneDoesNotKnowWhatAToolDoes` |
| [`internal/llm`](internal/llm) (format) | `llm.Formatter` | `format` | `TestTheInferencePlaneDoesNotKnowWhatYAMLIs` |
| [`internal/loop`](internal/loop) | `Planner`/`Executor`/… | `loop/adapter` | `TestTheLoopKnowsNeitherAModelNorARuntime` |

The pattern is identical every time, which is the point: a new extension is not a new
*kind* of thing, it is the same kind of thing again. And [`internal/router`](internal/router)
proves the payoff — it routes between a `map[string]llm.Provider` and contains the
strings `"ollama"` and `"bedrock"` *only inside comments*. Add a fifth provider and
the router does not change, because it was never allowed to know the first two.

## Recipe: add a new LLM provider

This is the canonical extension, and it is genuinely three steps. Say you want Amazon
Nova, Mistral, or an OpenAI-compatible endpoint:

**1. Write a client that implements [`llm.Provider`](internal/llm/llm.go).** Five
methods — `Name`, `Capabilities`, `Models`, `Generate`, `Stream` — in a new
`internal/<vendor>` package. It owns its transport, its auth, its retries, and the
translation of its failures into `llm`'s shared errors (`ErrThrottled`,
`ErrModelAccessDenied`, …). It imports `internal/llm` and nothing else of the
platform's.

```go
// internal/nova/nova.go  (design sketch)
type Client struct { /* AWS SDK, config, logger */ }

func (c *Client) Name() string                 { return "nova" }
func (c *Client) Capabilities() llm.Capabilities {
    return llm.Capabilities{
        Local:            false,   // the prompt leaves the network — the router must see this
        Streaming:        true,
        MaxContextTokens: 300_000,
        Tools:            true,
        StructuredOutput: true,
        Reasoning:        false,
    }
}
func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) { /* … */ }
// Models, Stream …
```

**2. Add one `case` to [`internal/providers`](internal/providers/providers.go).** The
factory is the *one* place that knows the catalogue. Add the vendor to `Known` and a
`case "nova":` to `build`. That is the entire wiring.

**3. Nothing else.** Not the router, not `llm.Service`, not the tool loop, not the
prompt catalogue, not a single CLI. `TestTheRouterDoesNotKnowWhichProvidersExist` and
`TestOnlyTheFactoryKnowsAboutMoreThanOneVendor` will fail the build if you were
tempted to. The router discovers what the new provider can do by asking
`Capabilities()` — is it local, what does it cost, can it use tools — so a provider
written next year answers the same questions without the router learning its name.

The `Capabilities` struct is the forward-looking contract that makes this work.
Because a router *asks the interface* rather than switching on a vendor package, the
only thing a new provider must do to be routable is answer honestly about itself. See
[ROUTING.md](ROUTING.md).

## Extension: MCP servers

The **Model Context Protocol** standardises how a model is given tools and context by
an external server. It is the highest-leverage future extension, because the platform
already has the seam it needs — twice over — and MCP maps cleanly onto both.

The platform's tools are already an abstraction: [`internal/tools`](internal/tools)
implements `llm.ToolRunner`, and the inference plane knows only a tool's *schema* and
whether it is a `Write` tool — never what it does (`TestTheInferencePlaneDoesNotKnowWhatAToolDoes`).
So there are two honest ways to add MCP, and they extend different seams:

- **MCP as a tool source (client side).** An MCP client that connects to external MCP
  servers, lists their tools, and exposes them through the existing `llm.ToolRunner`
  interface. The model calls them exactly as it calls `run_workflow` today; the loop
  cannot tell an MCP-backed tool from a native one, which is precisely the property
  that makes it safe to add. The `Write`/read distinction — the one fact the loop uses
  to decide *may this be retried?* — must be preserved: an MCP tool that changes the
  world is a `Write` tool, and mislabelling it re-opens the exact double-execution
  hazard [`internal/llm`](internal/llm/llm.go) documents at length.

```go
// internal/mcp — design sketch. It implements the platform's existing interface;
// it does not invent a new one.
type ToolSource struct { /* MCP client, connected servers */ }

// Adapt each MCP tool into an llm.ToolSpec + a runner, classifying it Write/read
// so the loop's retry rule stays correct.
func (s *ToolSource) Tools() []llm.ToolSpec { /* … */ }
func (s *ToolSource) Run(ctx context.Context, call llm.ToolCall) (llm.ToolResult, error) { /* … */ }
```

- **The platform as an MCP server (server side).** Expose the platform's *own*
  capabilities — trigger a workflow, submit an agent task — as an MCP server other
  agents can use. This is the "a platform other people can build agents on" goal, and
  it reuses the same tool registry that already fronts `workflow.Service` and
  `agent.Service`. The security boundary is the same one the tool loop already
  enforces: an external agent may call only the tools the registry exposes, under the
  same budgets and validation, and a `Write` is still a `Write`.

MCP is deliberately its *own* future milestone (it is on the roadmap, sequenced after
this one) because the protocol has real surface — transport, capability negotiation,
auth, the trust boundary around a tool an outside agent may invoke. This milestone's
job is to establish that MCP lands on `llm.ToolRunner` and the tool registry, not on a
new parallel mechanism, so that when it is built it extends a seam rather than growing
one.

## Extension: vector databases

Retrieval-augmented generation and long-term agent memory both need a vector store,
and the platform does not have one yet. Adding it is the same move as adding a
provider: define the platform's side of the boundary as a small interface, put a
vendor client behind it, and let exactly one factory know which vendor is configured.

```go
// internal/memory — design sketch. The platform's side of the retrieval boundary.
// The core owns the interface and the errors; a client (pgvector, OpenSearch,
// Pinecone, S3 Vectors) implements it. The arrow points inward, as always.
type Document struct {
    ID       string
    Content  string
    Metadata map[string]string
}

type Match struct {
    Document Document
    Score    float32
}

type Store interface {
    Name() string
    Upsert(ctx context.Context, docs []Document, vectors [][]float32) error
    Query(ctx context.Context, vector []float32, k int) ([]Match, error)
}

// Embedding is a SEPARATE concern, and it is one the platform already has: producing
// vectors is inference, so the embedding model is an llm.Provider capability, not a
// property of the store. This keeps "which model embeds" and "where vectors live" as
// two independent extension points rather than one welded pair.
```

Two design commitments make this fit the platform rather than fight it:

1. **Embedding is inference, not storage.** The vector store holds and searches
   vectors; it does not produce them. Producing them is an `llm.Provider` job (an
   embeddings capability), so the store never learns which model embeds, and the model
   never learns where vectors are kept. Two seams, not one.
2. **The store is a core with clients.** `pgvector` on RDS, OpenSearch, Amazon S3
   Vectors, or a managed service are all *clients* behind `memory.Store`. Swapping one
   for another is a factory change, exactly as swapping Ollama for Bedrock is — and a
   new architecture test (`internal/memory` never imports its client) would guard it,
   exactly as the existing ones do.

RAG then composes from parts the platform already has: retrieve with `memory.Store`,
embed with an `llm.Provider`, and hand the retrieved context to the same inference
plane and tool loop that exist today. No new orchestration plane; a new leaf and a
composition.

## Extension: any future AI service

The test of an extension model is whether it works for the thing you have not thought
of. The recipe is invariant:

1. **Name the platform's side of the boundary** as a small interface in a new
   `internal/<core>` package — the *fewest* methods that capture what the platform
   needs, in the platform's own vocabulary and errors.
2. **Put one vendor client behind it** in `internal/<client>`, implementing the
   interface and translating the vendor's failures into the core's errors.
3. **Register it in one leaf factory**, the only package allowed to know the catalogue.
4. **Guard the seam with an architecture test** so it cannot rot on a Tuesday.

If a proposed extension does not fit this shape, that is a signal worth heeding — it
usually means the boundary was drawn in the wrong place, or that the "extension" is
really a change to a core, which is a different and more expensive thing.

## What the extension points are, exactly

The stable interfaces a new integration plugs into, and where each lives:

| Extension point | Interface | Add a… |
| --- | --- | --- |
| **Model backend** | `llm.Provider` | LLM provider, embeddings provider, a hosted or local model |
| **Tool source** | `llm.ToolRunner` | native tool, or an MCP client exposing external tools |
| **Workflow engine** | `workflow.Service` | a different orchestrator behind n8n's seam |
| **Agent runtime** | `agent.Service` | a different agent runtime behind OpenClaw's seam |
| **Artifact validator** | `llm.Formatter` | a validator for a new artifact format |
| **Loop stage** | `loop.Planner`/`Executor`/… | a different planner or executor for the agent loop |
| **Retrieval store** *(proposed)* | `memory.Store` | a vector database for RAG / memory |
| **Provider selection** | routing strategy (config) | a new policy for choosing between providers |

Every row is the same shape: a narrow interface, owned by a core, implemented by a
client, registered in a factory, guarded by a test.

## What must NOT become an extension point

An extension model is defined as much by what it refuses to make pluggable. Some
things are singular on purpose, and turning them into extension points would trade the
platform's coherence for a flexibility nobody needs:

- **The catalogue.** Exactly one package (`internal/providers`) knows which vendors
  exist. "Let each package register itself" sounds flexible and is how you get three
  places that must agree and eventually don't.
- **The retry/side-effect rules.** Whether a step may be retried is decided in the
  core by one fact (is it a `Write`?), not delegated to a vendor. A client that could
  declare its own effects safe would re-open the double-execution hazard the platform
  spent Milestones 5, 6 and 9 closing.
- **The `RequireLocal` constraint.** No configuration, strategy, or new provider may
  make a request that must stay local leave the network. A constraint is not a
  preference, and an extension point that let a plugin override it would be a security
  regression wearing an abstraction.
- **Observability.** [`internal/observability`](internal/observability) is a leaf that
  imports nothing of the platform's; every extension emits telemetry *through* it and
  none may fork it. One logging standard, or none.

## Well-Architected

Mapped to the pillars an extensibility milestone serves — Operational Excellence and
Performance Efficiency.

| Principle | Pillar | How the platform honours it |
| --- | --- | --- |
| **Make frequent, small, reversible changes** | Operational Excellence | Adding a provider is one client package plus one factory line — a small change with a small blast radius, reverted by removing a `case`. |
| **Anticipate failure / guard the design** | Operational Excellence | The architecture is enforced by build-failing tests, so a well-meaning import cannot silently weld a core to a vendor. |
| **Evolve architectures through experimentation** | Operational Excellence | A new provider can be added, routed to for a fraction of traffic, and removed, without touching the callers — the seam makes experiments cheap. |
| **Go global / use the technology that best fits** | Performance Efficiency | The provider interface lets each workload use the best backend — local for privacy, managed for scale, a specialist model for a niche — behind one abstraction. |
| **Use serverless and managed services** | Performance Efficiency | New AI services (Bedrock models, managed vector stores, MCP servers) plug in as clients, so the platform adopts managed capability without owning its operation. |
| **Consider mechanical sympathy** | Performance Efficiency | `Capabilities` lets the router match a request to a provider that can actually serve it, rather than discovering a mismatch as a confident wrong answer. |

## Explicitly deferred

Stated plainly so the boundary of this **architecture-only** milestone is unambiguous:

- **No MCP implementation.** MCP is its own future milestone; this milestone
  establishes that it lands on `llm.ToolRunner` and the tool registry.
- **No vector store implementation.** The `memory.Store` interface here is a design
  sketch, not shipped code.
- **No new provider.** The recipe is proven by the two providers that already exist;
  this milestone adds no third.
- **No infrastructure.** No CloudFormation stack, no Makefile target — this milestone
  deploys nothing, by design.
- **No multi-tenancy.** Isolating multiple tenants on one deployment is a real future
  concern named in the roadmap; the extension model is a precondition for it, not the
  thing itself.

## Future improvements

- **Build the MCP integration** — a client that adapts external MCP servers into
  `llm.ToolRunner`, and a server that exposes the platform's own tools to outside
  agents, under the existing budget and `Write`-classification boundary.
- **Build the retrieval plane** — a `memory.Store` with a first client (pgvector or
  Amazon S3 Vectors), an embeddings capability on the provider interface, and RAG
  composed from the two.
- **A provider capability registry** — publish each provider's `Capabilities` on a
  health endpoint so operators can see the whole fleet's abilities at a glance.
- **Routing-strategy plugins** — make the selection policy (cost, latency, capability)
  configurable per environment without a code change, extending the one seam that is
  configuration rather than an interface today.
- **A conformance test kit** — a reusable suite any new `llm.Provider` (or
  `memory.Store`) can run to prove it honours the interface's contract, so a
  third-party client is correct by construction, not by inspection.
