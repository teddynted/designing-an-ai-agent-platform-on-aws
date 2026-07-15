# Extending an AI Agent Platform with New AI Providers and Services

> **Milestone 17 — Future Extensions.**
> This milestone ships an *architecture*, not an integration. It documents — in one
> place, grounded in code the platform has enforced since Milestone 7 — how you add a
> new LLM provider, an MCP server, or a vector database, and why each of those costs
> one new file and one factory line rather than a refactor. The reference is
> [EXTENSIBILITY.md](../../EXTENSIBILITY.md); the diagrams are
> [extensibility-diagrams.md](../../docs/architecture/extensibility-diagrams.md). It
> builds **no** new provider, **no** MCP server, **no** vector store, and **no**
> infrastructure — those are the integrations the architecture is *for*, and each is
> its own future milestone. The deliverable is the map and the recipe.

*Audience: engineers who have inherited a codebase where "add another provider" turned
out to mean editing forty files, and who suspect — correctly — that the alternative is
not a plugin framework but a boundary drawn in the right place and defended by a test.*

---

## Contents

- [Extensibility is not a feature you add; it is a boundary you defend](#extensibility-is-not-a-feature-you-add-it-is-a-boundary-you-defend)
- [Why this milestone builds nothing](#why-this-milestone-builds-nothing)
- [The arrow points inward](#the-arrow-points-inward)
- [Adding a provider, in three steps and no more](#adding-a-provider-in-three-steps-and-no-more)
- [The router proves the claim](#the-router-proves-the-claim)
- [MCP: the extension the platform is already shaped for](#mcp-the-extension-the-platform-is-already-shaped-for)
- [Vector databases: embedding is inference, storage is not](#vector-databases-embedding-is-inference-storage-is-not)
- [What must never become an extension point](#what-must-never-become-an-extension-point)
- [Lessons learned](#lessons-learned)
- [What comes next](#what-comes-next)

## Extensibility is not a feature you add; it is a boundary you defend

There is a version of "make it extensible" that means building a plugin system: a
registry, a lifecycle, a manifest format, a sandbox. It is almost always the wrong
version. It front-loads enormous complexity to buy flexibility you might never use,
and it tends to produce exactly the coupling it was meant to prevent, because a plugin
API is a surface every plugin and every caller has to agree on forever.

There is another version, and it is the one this platform has been building
quietly since Milestone 5, one integration at a time. It says: extensibility is what
you get for free when the boundaries are in the right place and nothing is allowed to
reach across them. You do not add it at the end. You *notice*, at the end, that it is
already there — and this milestone is that noticing, written down.

## Why this milestone builds nothing

Every previous milestone that shipped a capability shipped the infrastructure to run
it. This one deploys nothing, and that is not a smaller milestone — it is an honest
one. The extensibility mechanism was built incrementally and on purpose: the provider
interface arrived at Milestone 7, *before* there was a second provider, precisely so
the third would be cheap. The architecture tests arrived alongside each seam. What was
missing was never a mechanism. It was a single document that says: *here is how you
extend this platform, here is the recipe, and here is the bill.*

Building a throwaway third provider to "prove" the seam would be inventing a concrete
integration the scope does not call for — and the platform already has four real
integrations behind these boundaries, which is a better proof than a fifth toy one. So
this milestone ships the map, not another building. There is no CloudFormation stack
and no Makefile target, because there is nothing to deploy, and pretending otherwise
to satisfy a template would be the kind of dishonesty the rest of this project has
worked to avoid.

## The arrow points inward

Here is the entire model, and everything else is a consequence of it. Each integration
is two packages:

- A **core** — the platform's side of a boundary. Its types, its errors, its `Service`,
  its interface. `internal/llm`, `internal/workflow`, `internal/agent`.
- A **client** — one vendor's implementation of that interface. Its HTTP, its auth, its
  retries, its quirks. `internal/ollama`, `internal/bedrock`, `internal/n8n`,
  `internal/openclaw`.

And one rule: **the dependency points inward.** The client imports the core; the core
never imports the client. The platform's own vocabulary — `llm.Request`,
`llm.ErrThrottled` — knows nothing about any vendor. That is what makes the interface a
*seam* rather than a decoration, and it is what lets every `Service` be tested against
a fake with no HTTP server anywhere.

The reason this holds on a Tuesday, when a good engineer in a hurry is tempted to add
one import "just to get at Bedrock's config type," is that it is not a convention. It
is a test that fails the build:

```go
// internal/architecture_test.go
func TestTheCoreNeverDependsOnItsVendor(t *testing.T) {
    for _, seam := range seams {
        deps := transitiveImports(t, module+seam.core)
        if deps[module+seam.client] {
            t.Errorf("internal/%s imports internal/%s … a core that knows about "+
                "its vendor is not an abstraction — it is a vendor with extra steps",
                seam.core, seam.client)
        }
    }
}
```

An architecture rule checked only when someone remembers is a rule that gets broken by
a reasonable-looking import that compiles and passes every other test. So the platform
does not rely on memory. It relies on `go test`.

## Adding a provider, in three steps and no more

The canonical extension. You want Amazon Nova, Mistral, or an OpenAI-compatible
endpoint. The whole job:

**One:** write a client that implements `llm.Provider` — five methods: `Name`,
`Capabilities`, `Models`, `Generate`, `Stream`. It translates its own failures into
the platform's shared error vocabulary (`ErrThrottled`, `ErrModelAccessDenied`, …) so
a caller can classify a failure without importing a vendor to ask.

**Two:** add one `case` to `internal/providers` — the single leaf package allowed to
know that more than one vendor exists — and add the name to its `Known` list.

**Three:** there is no three. Not the router, not `llm.Service`, not the tool loop,
not the prompt catalogue, not one CLI. If you were tempted to touch any of them, a
test stops you.

The forward-looking part, designed at Milestone 7 when it could have been left out, is
the `Capabilities` struct: is the provider local, what does it cost, how big is its
context window, can it call tools, can it be held to a schema. A router does not switch
on a vendor's package name to learn these things — it *asks the interface*. Which means
a provider written next year answers the same questions without the router ever
learning its name.

That one decision — *ask the interface, don't switch on the type* — is the difference
between a platform you extend and a platform you refactor.

## The router proves the claim

The claim "adding a provider does not change the routing layer" is cheap to write in a
README and expensive to be wrong about, so the platform makes it a test rather than a
paragraph:

```go
func TestTheRouterDoesNotKnowWhichProvidersExist(t *testing.T) {
    deps := transitiveImports(t, module+"router")
    for _, vendor := range []string{"ollama", "bedrock", "n8n", "openclaw"} {
        if deps[module+vendor] {
            t.Errorf("internal/router imports internal/%s … the router must not know "+
                "which providers exist", vendor)
        }
    }
}
```

`internal/router` imports `internal/llm` and the standard library, and nothing else of
the platform's. The strings `"ollama"` and `"bedrock"` appear in it only inside
comments. It is handed a `map[string]llm.Provider` and routes between whatever it is
given — it would route between five providers, or between two Bedrock models, without a
line changing. That is not an aspiration the documentation asserts; it is a property
the compiler enforces, and it is the real product of this whole milestone.

## MCP: the extension the platform is already shaped for

The Model Context Protocol standardises how a model is handed tools and context by an
external server, and it is the highest-leverage extension on the roadmap — because the
platform already has the seam it needs, and MCP maps onto it from both directions.

The platform's tools already live behind `llm.ToolRunner`, and the inference plane
knows exactly two things about any tool: its schema, and whether it is a `Write` tool.
It does not know what a tool *does* — `TestTheInferencePlaneDoesNotKnowWhatAToolDoes`
guarantees it. So MCP does not need a new mechanism; it needs to land on this one:

- **As a tool source**, an MCP client connects to external MCP servers and exposes
  their tools through `llm.ToolRunner`. The model calls them exactly as it calls
  `run_workflow` today, and the loop cannot tell the difference — which is precisely
  why it is safe. The one thing that must survive the adaptation is the `Write`/read
  classification, because that single bit is what the loop uses to answer the only
  question it owns: *may this be retried?* An MCP tool that changes the world is a
  `Write` tool, and mislabelling it re-opens the double-execution hazard the platform
  spent three milestones closing.
- **As a server**, the platform exposes its own capabilities — trigger a workflow,
  submit an agent task — as an MCP server other agents can call, reusing the same tool
  registry, the same budgets, the same validation. That is the "a platform other
  people can build agents on" goal, reached by extending a seam rather than growing
  one.

MCP is deliberately its own future milestone, because the protocol has real surface —
transport, capability negotiation, the trust boundary around a tool an outside agent
may invoke. This milestone's contribution is to fix *where it lands*: on
`llm.ToolRunner` and the existing registry, not on a parallel path. Deciding that now
is what keeps the eventual implementation an extension instead of a fork.

## Vector databases: embedding is inference, storage is not

RAG and long-term agent memory both want a vector store, which the platform does not
have. Adding one is the same move as adding a provider — a small core interface, a
vendor client behind it, one factory that knows which is configured:

```go
// internal/memory — design sketch, not shipped in this milestone.
type Store interface {
    Name() string
    Upsert(ctx context.Context, docs []Document, vectors [][]float32) error
    Query(ctx context.Context, vector []float32, k int) ([]Match, error)
}
```

The design commitment that makes it fit the platform rather than fight it: **embedding
is inference, and storage is not.** Producing a vector is a model call — an
`llm.Provider` capability — so the store never learns which model embeds, and the
model never learns where vectors live. Two independent extension points, not one welded
pair. `pgvector` on RDS, OpenSearch, and Amazon S3 Vectors are then all *clients*
behind `memory.Store`, swappable by a factory change exactly as Ollama and Bedrock are,
and guarded by a new architecture test exactly as they are. RAG becomes a composition
of parts that already exist — retrieve, embed, hand the context to the same inference
plane and tool loop — rather than a new plane to build.

Keeping those two concerns apart is the kind of decision that looks like pedantry until
the day you want to change your embedding model without re-indexing your store, or move
your vectors to a cheaper backend without re-embedding a corpus. Then it is the
difference between an afternoon and a quarter.

## What must never become an extension point

An extension model is defined by what it refuses to make pluggable. Some things are
singular on purpose:

- **The catalogue.** One package knows which vendors exist. "Let each package register
  itself" is how you get three lists that must agree and eventually don't.
- **The retry / side-effect rule.** Whether a step may be retried is decided in the
  core by one fact — is it a `Write`? — never delegated to a vendor that could declare
  its own effects safe.
- **The `RequireLocal` constraint.** No plugin, strategy, or configuration may send a
  prompt that must stay local out to a third party. A constraint is not a preference,
  and an extension point that could override it would be a security regression wearing
  an abstraction.
- **Observability.** One logging and metrics standard, imported by everything,
  forkable by nothing.

A flexible system is not one where everything is pluggable. It is one where the right
things are, and the load-bearing things are conspicuously not.

## Lessons learned

- **Extensibility is a boundary, not a framework.** You do not build a plugin system;
  you draw the seam in the right place and forbid anything from reaching across it. The
  payoff is collected years later, as a `case` statement instead of a refactor.
- **Ask the interface; do not switch on the type.** The router learns what a provider
  can do from `Capabilities()`, so a provider built next year is routable without the
  router knowing its name. A switch on a vendor package cannot say that.
- **An architecture rule that isn't a test is a wish.** The seams hold because
  `go test` fails the build when they don't — not because a document asked nicely.
- **The best time to build an abstraction is before the second implementation.** Added
  at Milestone 7 with one provider, it was an interface. Added at Milestone 10 on top
  of three call sites that each learned Ollama's JSON, it would have been a rewrite.
- **A milestone can ship a map instead of a building.** The honest deliverable here is
  the recipe and the boundary, proven by integrations that already exist — not a toy
  fifth one built to look busy.

## What comes next

The architecture points directly at its own next milestones, in order of leverage:
**MCP** first — a client that adapts external MCP tools onto `llm.ToolRunner`, and a
server that exposes the platform's own tools to outside agents under the existing
budget and `Write`-classification boundary. Then the **retrieval plane** — a
`memory.Store` with a first vector-DB client, an embeddings capability on the provider
interface, and RAG composed from the two. Then the further providers the recipe makes
cheap, and eventually the **multi-tenancy** the extension model is a precondition for.

Each is a new client behind an existing seam, plus a factory line, plus a test — which
is exactly the claim this milestone exists to make, and exactly why it could make it
without building any of them yet. The reference, with the full recipe and the complete
extension map, is [EXTENSIBILITY.md](../../EXTENSIBILITY.md).
