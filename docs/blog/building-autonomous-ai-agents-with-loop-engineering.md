# Building Autonomous AI Agents with Loop Engineering

> **Milestone 11 — Loop Engineering.**
> This milestone gives the platform an explicit, bounded, recoverable **agent loop**: a state
> machine that takes a goal and plans, executes, evaluates, reflects, retries, adapts and
> stops. It builds a planning engine, an execution engine, an evaluation engine, a reflection
> engine, a retry framework, a loop controller and serialisable state. It does **not** build
> multi-agent collaboration, long-term memory, RAG, or Knowledge Bases — those are later. The
> code is in [`internal/loop`](../../internal/loop) and
> [`internal/loop/adapter`](../../internal/loop/adapter).

*Audience: engineers who have wired an LLM into a `for` loop, watched it spend $40 overnight,
and concluded that "autonomous" is a synonym for "unbounded". This is the milestone about
making the loop explicit enough to bound, observe, and stop — which turns out to be an
engineering problem, not a prompting one.*

---

## Contents

- [The loop is the product, and the model is a component](#the-loop-is-the-product-and-the-model-is-a-component)
- [Three loops, and the trap of conflating them](#three-loops-and-the-trap-of-conflating-them)
- [The controller is a reducer](#the-controller-is-a-reducer)
- [Where the work actually happens](#where-the-work-actually-happens)
- [Stopping conditions are the feature](#stopping-conditions-are-the-feature)
- [Retry, and the one bit everyone gets wrong](#retry-and-the-one-bit-everyone-gets-wrong)
- [Reflection: changing behaviour without changing code](#reflection-changing-behaviour-without-changing-code)
- [Recovery: a Spot instance can vanish mid-thought](#recovery-a-spot-instance-can-vanish-mid-thought)
- [The loop knows neither a model nor a runtime](#the-loop-knows-neither-a-model-nor-a-runtime)
- [Who drives the loop](#who-drives-the-loop)
- [Testing an autonomous agent without a GPU or an AWS account](#testing-an-autonomous-agent-without-a-gpu-or-an-aws-account)
- [Lessons learned](#lessons-learned)
- [What comes next](#what-comes-next)

## The loop is the product, and the model is a component

There is a version of this milestone that is about fifty lines long: call a model, ask it for
a plan, `for` over the tasks, call the model again to check each one, and let it decide when to
stop. It works in a demo. Then it fails in one of the ways autonomous agents fail — it loops on
a task it cannot do, or it declares success on a draft that misses the point, or it runs all
night because "keep going" was the model's decision to make and the model likes to keep going —
and every one of those failures is invisible until the bill or the pull request arrives.

The thing that was missing is not a better prompt. It is that the *loop* — the decision to
continue, to retry, to give up, to ask a human — was **inside the model**, where it could not
be bounded, inspected, or tested. Loop Engineering is the decision to take the loop *out* of
the model and make it a program: an explicit state machine the platform owns, where "should we
continue?" is a function you can read, and "stop after $5" is enforced by code rather than
requested in a system prompt and hoped for.

The model is still essential — it plans, it judges, it reflects, and those are genuinely hard
and genuinely its strength. But it is a *component* the loop calls, not the loop itself. That
inversion is the whole milestone.

## Three loops, and the trap of conflating them

By this point the platform already had two loops, and the first real design decision was
refusing to let this be a third copy of either:

- **The agent's loop (Milestone 6)** runs inside OpenClaw. Submit one open-ended task and the
  agent reasons, uses its shell, and produces output — all behind an HTTP boundary. The
  platform never sees the turns.
- **The tool loop (Milestone 9)** runs inside a single inference. A model calls tools and reads
  results within one conversation. The model drives; it is one reasoning task that takes a few
  round trips.
- **This loop (Milestone 11)** runs *above* both. It decomposes a goal into tasks, runs each
  (as a whole agent execution or a reasoning step), evaluates, reflects, and decides whether to
  continue — explicitly, where the platform can bound and observe it.

The trap is that all three can be described as "an AI thing that repeats until done", and if you
build this one as though OpenClaw were not already an agent, you end up with two agents nested
inside each other, the platform reaching *into* the reasoning it spent Milestone 6 putting
*behind* a boundary. So the rule I held to: this loop **orchestrates** OpenClaw executions; it
does not become one. It is the level that decides *which* tasks and *whether to continue* —
which is orchestration, the platform's job — while OpenClaw does the executing, which is the
agent's.

## The controller is a reducer

The core of the milestone is two pure functions:

```go
Decide(state, config, now) Action            // what should happen next?
Advance(state, config, result, now) State     // fold an action's result back in
```

`Decide` looks at the state and returns an `Action` — plan, execute this task, evaluate,
reflect, summarise, or stop. A **Runner** performs the action (calling a model, or an agent) and
hands the result to `Advance`, which produces the next state. Neither `Decide` nor `Advance`
does any I/O. The loop is a conversation between them, and the Runner is the interpreter.

I did not start here. I started with a `for` loop with everything on the stack, and I rewrote it
into a reducer when I noticed that three separate requirements were all really the same
requirement: *the loop's decisions must be separable from the loop's actions.*

- **Stopping conditions "must always be enforced."** In a `for` loop with the checks sprinkled
  through the phases, "always" is a promise every phase has to keep, and the one that forgets is
  the one that spends the money. In a reducer, the checks go at the top of `Decide`, which runs
  before *every* action — one road all traffic travels. It is the same move `llm.Service` makes
  by checking the context window in one place so no provider can forget.
- **"Recovery from interruptions where practical."** A `for` loop's state is on a goroutine's
  stack, and a Spot reclaim vaporises it. A reducer's state is a value — serialise it, reload
  it, call `Decide`, and the loop resumes.
- **"Each stage independently testable."** A pure `Decide` is tested with a struct literal. No
  model, no agent, no clock — you build the state you want to ask about and assert the action.

Three requirements, one shape. That is usually the sign you have found the right shape.

## Where the work actually happens

The loop orchestrates; it does not do the work. Each stage is delegated across a boundary the
platform already had:

- **Plan, evaluate, reflect, summarise** are *reasoning* — single-shot, structured, no side
  effects, safe to retry, cheap. They go to the inference plane (`llm.Structured`), which routes
  to Claude or Ollama via Milestone 10 and does not care which. These decode straight into the
  loop's own types — `Structured[loop.Plan]`, `Structured[loop.Evaluation]` — because those
  types already have JSON tags and validation. There is no parallel set of DTOs, because there
  is no reason for one.
- **Execute a task** is the one step that changes the world: an OpenClaw execution, expensive,
  slow, and not safe to retry blindly. It goes to the agent runtime.

Keeping those two as different interfaces — a `Reasoner` and an `Executor` — is what stops the
loop from ever treating "think about the work" and "do the work" as the same operation, which is
the exact conflation Milestone 6 spent a milestone separating. The evaluator can be wrong for
free; the executor being wrong opens a pull request.

## Stopping conditions are the feature

I want to be blunt about this: for an autonomous agent, the stopping conditions are not
safety rails bolted onto the feature. **They are the feature.** Anyone can write the loop that
keeps going; the engineering is in the loop that knows when to stop.

There are eight, all enforced in `Decide` before any action:

```
goal-achieved · max-iterations · max-retries · max-replans
timeout · cost-exceeded · human-required · critical-failure
```

Two are worth dwelling on. **`cost-exceeded` counts both halves of the bill** — the agent
executions *and* the reasoning steps — because a cost cap blind to the reasoning is a cost cap
that lies, and on a tool-using reasoning step the reasoning is not the cheap half. **`max-iterations`
is the backstop**: even if every other bound were misconfigured to zero, a loop that can only
execute a bounded number of times cannot run forever. It is the guarantee of termination, and
it is the one bound I would never let an operator disable.

And the loop **summarises before it stops** — even when a bound aborts it — because a loop that
stopped short still did work, spent money, and owes an account of both.

## Retry, and the one bit everyone gets wrong

The retry framework has the usual knobs: max attempts, exponential backoff, a cap. The bit that
matters, and the bit the fifty-line version gets wrong, is *which failures to retry*.

The rule is the same one the whole platform uses, and it is the router's `canFailOver` wearing a
different hat: **only a transient failure is worth retrying.** A runtime that was unreachable or
timed out might succeed next time. A task that *ran and failed on its merits* — the agent
produced something we rejected, the objective is impossible — will fail exactly the same way on
the next attempt, and retrying it just spends the money twice before arriving at the same wall.

So the executor's most important job is one boolean: mapping OpenClaw's errors to
`Outcome.Transient`. Get it wrong in the optimistic direction and the loop burns its whole retry
budget re-running a doomed task. The test I care about most in the adapter is the table that
pins every error to the right side of that line.

There is a second subtlety the idempotency machinery forced. OpenClaw is idempotent — submit the
same request twice and it returns the *existing* execution rather than starting a second. That is
exactly right for a transport retry, and exactly wrong for a loop retry, which genuinely wants a
*fresh* execution of a task that just failed. So the correlation the executor sends folds in the
attempt number: a transport retry within one submit stays idempotent, and a loop retry across
attempts gets a new execution. One line, and it is the line between "the loop retried" and "the
loop got handed back the failure it was trying to move past".

## Reflection: changing behaviour without changing code

Reflection is the stage that earns the word "engineering" the most, because it is where the loop
gets *better* between attempts. When a task fails and will be retried, the reflector analyses why
and rewrites the task's instructions — a sharper, corrected prompt — for the next attempt.

The requirement was that reflection "improve agent behaviour without changing application code",
and the reducer makes that literal: the revised instructions are data that flows through the
state and replaces the failed task's instructions. The loop behaves differently on attempt two,
and not one line of the loop changed.

The one thing I was careful about is the security boundary. It would be natural for the reflector
to read the failed output — which may contain repository content — and fold it into the new
instructions. That is the Milestone 6 prompt-injection hazard through a new door: repository text
laundered into an instruction the agent then obeys. So the reflection prompt is explicit that the
failed output is *material to analyse*, not *instructions to import*, and the revised instructions
are authored on the platform's side of the boundary. Reflection sharpens the platform's own
instruction; it does not paraphrase the repository's.

## Recovery: a Spot instance can vanish mid-thought

This platform runs its compute on EC2 Spot, which can be reclaimed with two minutes' notice. A
loop that takes an hour *will* sometimes be halfway through when its instance is taken away. So
"recovery from interruptions where practical" is not a nice-to-have here; it is Tuesday.

Because the state is a serialisable value, recovery is: persist it after each step, and on a
fresh process, load it and call `Decide`. The reducer resumes where it stopped. The detail that
makes this actually safe is the *pending outcome*: when a loop is reclaimed right after an
expensive execution but before evaluating it, the outcome of that execution rides along in the
serialised state — so the reload evaluates the result it already has, rather than re-running the
agent to get it again. Re-running the expensive, side-effecting step is the one thing recovery
must not do, and the test that the pending outcome survives a JSON round-trip is the test that it
does not.

## The loop knows neither a model nor a runtime

The claim the milestone makes is that the loop is independent of the provider and the runtime —
that swapping Claude for another model, or OpenClaw for another agent, changes an adapter and not
the loop. And the way that claim dies is a good engineer in a hurry: the planner needs a plan, and
`llm.Structured` is *right there*, so `internal/loop` imports `internal/llm`, and now the loop
cannot be tested without a model and cannot be reasoned about without the AWS SDK in the build.

So `internal/loop` declares its stages as interfaces — `Planner`, `Executor`, `Evaluator`,
`Reflector`, `Summariser` — and imports neither `internal/llm` nor `internal/agent`.
`internal/loop/adapter` implements them against the real planes. And this is a test, not a
paragraph:

```go
func TestTheLoopKnowsNeitherAModelNorARuntime(t *testing.T) {
    deps := transitiveImports(t, module+"loop")
    for _, forbidden := range []string{"llm", "agent", "ollama", "bedrock", "openclaw", "router"} {
        if deps[module+forbidden] {
            t.Errorf("internal/loop imports internal/%s …", forbidden)
        }
    }
}
```

The reward is concrete and I collected it the same afternoon: the entire controller — every
stopping condition, the retry machinery, the whole reducer — is tested against fake engines that
are struct literals, with no model and no OpenClaw anywhere. A test of "does the loop stop at the
cost cap" runs in microseconds and needs no credentials, because the loop does not know what a
credential is.

## Who drives the loop

The reducer never waits, and that turned out to be the key to fitting an hours-long loop into a
platform that must not block. There are two drivers, and they call the same two functions:

- **The `loop.Runner`** is synchronous — it blocks on each agent execution. That is correct for
  the CLI and for tests, and it carries the exact warning `agent.Service.Wait` does: never run it
  in a Lambda or a webhook handler, because you would hold a request-scoped process open for an
  hour and lose the run when the instance is reclaimed.
- **In production, n8n drives the loop.** Call `Decide`, submit the agent execution it asks for,
  let a durable wait node poll it, call `Advance` with the outcome — across a run that may last
  hours, on infrastructure built to survive restarts. That driver is an n8n workflow, not code in
  this repository, in exactly the way the waiting for a *single* agent execution has been since
  Milestone 6.

The same reducer runs under both. The synchronous Runner is the reference driver and the test
harness; n8n is the production one. Writing the controller so it does not care which is what let
"the loop takes hours" and "the platform never blocks" both be true.

## Testing an autonomous agent without a GPU or an AWS account

Everything above compounds into a test suite that is, for a milestone about autonomous AI agents,
almost suspiciously mundane. The reducer is pure, so its tests are tables. The engines are
interfaces, so the Runner is tested against fakes. The adapters map real errors, so they are
tested against a fake runtime with no HTTP. The whole of `internal/loop` tests in about a second,
with no model, no agent, and no cloud — and that is not a shortcut, it is the proof that the loop
is genuinely decoupled from the things that would make it slow and expensive to test. An
autonomous agent you can only test by running it is an autonomous agent you will not test.

## Lessons learned

- **Take the loop out of the model.** The model plans and judges well; it decides "keep going"
  badly, because keeping going is what it does. Make the loop a program you can bound and read.
- **A reducer buys three requirements with one shape** — always-enforced stops, recoverable
  state, and independently testable stages all fall out of separating decisions from actions.
- **Stopping conditions are the feature, not the guardrail**, and `max-iterations` is the one
  that guarantees termination when everything else is misconfigured.
- **Retry only transient failures.** The executor's error-to-`transient` mapping is the whole
  retry framework's foundation, and the optimistic mistake is the expensive one.
- **Idempotency and intentional retry are opposites** — fold the attempt into the key so a loop
  retry is a fresh execution while a transport retry stays idempotent.
- **Reflection is behaviour change as data**, and it is where repository content most wants to
  sneak across the instruction boundary. Keep it on the platform's side.
- **Recovery means the pending outcome survives**, or a reclaim re-runs the expensive step.
- **Decouple the loop from the model and the runtime with a test**, and the reward is a test
  suite that needs neither.

## What comes next

The loop was built to make the milestones after it cheap, and the seams are shaped for them:

- **Human-in-the-loop approval (Milestone 12-14)** already has its signal: `humanRequired` stops
  the loop cleanly, ready to become an n8n approval gate between "drafted" and "published".
- **Dynamic and cost-aware planning strategies** are new `Planner` implementations behind the
  same interface — the loop does not move.
- **Parallel execution** is a driver change; the plan's dependency graph is already there.
- **Long-term memory, MCP servers, tool registries, RAG** sit behind the stage interfaces or
  beneath the execution boundary — a memory-backed evaluator or a retrieval-augmented planner is
  an adapter, and the reducer never notices.
- **Multi-agent systems** are a chain of loops, which is an n8n workflow — exactly the point of
  having an orchestrator.

A goal in, a bounded and observable and recoverable loop that pursues it, and a summary out —
with the model doing the reasoning it is good at, inside a loop the platform can actually control.
That is the difference between an autonomous agent and a `for` loop with a model in it, and it is,
in the end, an engineering difference.
