package loop

import "context"

// The stage engines. Each is a small interface DECLARED here and implemented at the edge, in
// internal/loop/adapter — the same dependency-inversion move llm makes with Provider,
// ToolRunner and Formatter, and for the same payoff: the loop's logic depends on these
// abstractions, not on internal/llm or internal/agent, so it can be tested against fakes with
// no model and no runtime, and a new provider or a new agent runtime changes an adapter
// without the loop noticing.
//
// The split into four reasoning interfaces plus one executor is not decoration. It is the
// milestone's "each stage independently testable", made structural: a test of the planner
// does not need an evaluator, and a fake that only implements Evaluate can be used to drive
// the reducer through an evaluation without standing up the rest.
//
// # Why the reasoning engines and the executor are different KINDS of thing
//
// Planner, Evaluator, Reflector and Summariser are REASONING — single-shot inference, no
// side effects, safe to retry, cheap. In the adapter they are backed by the platform's own
// inference plane (llm.Service.Structured), which routes to Claude or Ollama via Milestone
// 10 and does not care which.
//
// [Executor] is EXECUTION — it makes something happen in the world, it costs real money, it
// takes minutes to hours, and it is not safe to retry blindly. In the adapter it is backed by
// the agent runtime (OpenClaw). Keeping it a separate interface is what stops the loop from
// ever treating "do the work" and "think about the work" as the same operation — which is the
// exact conflation Milestone 6 spent a milestone separating.

// Planner turns a goal into an executable plan. It is the loop's first stage and its main
// piece of up-front reasoning: understand the objective, break it into tasks, order them.
type Planner interface {
	Plan(ctx context.Context, goal Goal) (Plan, error)
}

// Executor performs one task and reports what happened. It is the ONLY stage that changes
// anything in the world, and its result is an [Outcome] rather than an error — because a task
// failing is a normal thing the loop reasons about, not an exception it unwinds on.
//
// An implementation owns the mapping from a loop [Task] to whatever actually runs it, and — as
// importantly — the mapping from that runtime's failures to [Outcome.Transient], which is what
// the loop's retry decision rests on.
//
// # Why attempt is a parameter
//
// attempt is the loop's iteration count at the point of this execution — distinct for every
// call, including retries of the same task. An executor backed by an idempotent runtime
// (OpenClaw, which recognises a repeated submission and returns the EXISTING execution rather
// than starting a second) needs it: a genuine loop RETRY is a deliberate new attempt and must
// get a fresh execution, so the attempt is folded into the idempotency key. Without it, a
// retry would idempotently receive the same failed execution it was trying to move past. It
// is the one piece of loop state the executor cannot do without, so it is passed explicitly
// rather than smuggled onto the task.
type Executor interface {
	Execute(ctx context.Context, goal Goal, task Task, attempt int) (Outcome, error)
}

// Evaluator judges an outcome and produces the loop's routing decision. It is a reasoning
// step, and it is where "the agent exited cleanly" is turned into the more useful "the agent
// did the right thing" — or not.
type Evaluator interface {
	Evaluate(ctx context.Context, goal Goal, task Task, outcome Outcome) (Evaluation, error)
}

// Reflector analyses a failure and proposes a change for the next attempt. It is optional —
// the loop runs without it when reflection is disabled — and it is the stage that lets the
// loop's behaviour improve without its code changing.
type Reflector interface {
	Reflect(ctx context.Context, goal Goal, task Task, outcome Outcome, eval Evaluation) (Reflection, error)
}

// Summariser writes the loop's final account from its finished state. A reasoning step, run
// once, at the end.
type Summariser interface {
	Summarise(ctx context.Context, goal Goal, state State) (Summary, error)
}

// Priced is an OPTIONAL capability an engine may implement to report what its last call cost,
// in USD. It is a separate interface, checked with a type assertion, rather than a return
// value on every method — so that a fake engine in a test does not have to fake a cost, and a
// reasoning engine that knows its token bill (the adapter's, which reads it off the
// llm.Response) can report it without changing any interface signature. It is the same
// optional-capability pattern the standard library uses for http.Flusher and io.WriterTo:
// the common path stays simple, and the richer behaviour is available to those that want it.
//
// Cost reported here feeds [StepResult.Cost] and thus the loop's cost cap. An engine that
// does not implement it simply contributes zero, which is correct for a fake and merely
// imprecise for a real engine that has not bothered — the cap is a guard, and a guard reading
// a slightly low number errs toward letting the loop run, which the iteration and time caps
// still bound.
type Priced interface {
	// LastCostUSD reports the cost of this engine's most recent call. It is read immediately
	// after the call, by the single-threaded Runner, so "most recent" is unambiguous.
	LastCostUSD() float64
}

// pricedCost reads an engine's last cost if it reports one, or zero.
func pricedCost(engine any) float64 {
	if p, ok := engine.(Priced); ok {
		return p.LastCostUSD()
	}
	return 0
}

// Engines bundles the five stages a [Runner] needs. Reflector may be nil when reflection is
// disabled in [Config]; the reducer never asks for a reflection in that case, so the nil is
// never dereferenced — but [Engines.validate] refuses the contradictory combination of
// reflection enabled and no reflector, at construction, rather than at the first failure.
type Engines struct {
	Planner    Planner
	Executor   Executor
	Evaluator  Evaluator
	Reflector  Reflector
	Summariser Summariser
}
