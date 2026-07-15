// Package adapter is the edge where the loop's abstract stages meet the platform's concrete
// planes: reasoning is backed by the inference plane (internal/llm, and through it Claude or
// Ollama via the Milestone 10 router), and execution is backed by the agent runtime
// (internal/agent, and through it OpenClaw).
//
// It is the ONE place that knows both that a plan is produced by a language model and that a
// task is executed by an agent. internal/loop knows neither — it declares [loop.Planner],
// [loop.Executor] and the rest as interfaces, and this package implements them. So this is
// the analogue of internal/tools (which wires the platform's cores to the model's tools) and
// internal/providers (which wires the vendors to the provider interface): a composition leaf
// that the architecture test allows to import several things precisely because nothing
// imports it but a main.
//
// Keeping this separate from the loop core is what makes "the loop is independent of the
// provider and the runtime" a fact the compiler checks. Swap Claude for another model, or
// OpenClaw for another runtime, and only this package changes.
package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/loop"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/prompt"
)

// Reasoner implements every REASONING stage of the loop — [loop.Planner], [loop.Evaluator],
// [loop.Reflector] and [loop.Summariser] — against a single [llm.Service].
//
// One struct for all four is deliberate: they share a model, a cost model, and a prompt
// catalogue, and the loop wires the same Reasoner into four slots of [loop.Engines]. Because
// the [Runner] is single-threaded and reads [Reasoner.LastCostUSD] immediately after each
// call, one shared last-cost field is unambiguous — but it is the reason a Reasoner must not
// be shared across concurrent loops, which the doc on LastCostUSD says plainly.
//
// # Why it can decode straight into the loop's own types
//
// The loop's [loop.Plan], [loop.Evaluation] and the rest already have JSON tags and, where it
// matters, a Validate method — they were built to be the schema-shaped answer, not merely a
// value to copy one into. So Structured[loop.Plan] decodes the model's output directly into
// the thing the loop consumes, with the loop's own semantic checks (a plan whose dependencies
// resolve, an evaluation whose confidence is in range) enforced by llm.Structured's repair
// loop for free. There is no parallel set of DTOs to keep in sync, because there is no reason
// to have one.
type Reasoner struct {
	svc *llm.Service

	// taskTypes are the execution capabilities a plan may use, listed to the planner and
	// enumerated in the plan schema. They come from the executor's runtime, so the planner
	// cannot invent a task type no agent can perform.
	taskTypes []string

	// lastCostUSD is the cost of the most recent reasoning call, for [loop.Priced]. Computed
	// from the response's token usage and the provider's own price, so it is correct for
	// whichever provider the router chose — zero for a local model, real for Bedrock.
	lastCostUSD float64
}

// NewReasoner builds a reasoning adapter over an inference service. taskTypes is the set of
// execution capabilities a plan may reference — pass the executor's task types, so a plan
// cannot name work no runtime can do.
func NewReasoner(svc *llm.Service, taskTypes []string) *Reasoner {
	return &Reasoner{svc: svc, taskTypes: taskTypes}
}

// LastCostUSD reports the cost of the most recent reasoning call. It satisfies [loop.Priced],
// so the loop's cost cap accounts for the reasoning half of the bill and not only the agent
// executions. It reflects a single call and is read immediately after one, by the
// single-threaded Runner — so a Reasoner is bound to one loop at a time.
func (r *Reasoner) LastCostUSD() float64 { return r.lastCostUSD }

// Plan turns a goal into a plan. It is [loop.Planner].
func (r *Reasoner) Plan(ctx context.Context, goal loop.Goal) (loop.Plan, error) {
	p := prompt.MustLoad("loop/plan")
	text, err := p.Render(map[string]any{
		"Objective":  goal.Objective,
		"Repository": repoLine(goal.Repository),
		"TaskTypes":  strings.Join(r.taskTypes, ", "),
		"Params":     paramLines(goal.Params),
	})
	if err != nil {
		return loop.Plan{}, err
	}

	plan, res, err := llm.Structured[loop.Plan](ctx, r.svc, r.request(goal, "loop-plan", p, text), planSchema(r.taskTypes))
	r.record(res)
	if err != nil {
		return loop.Plan{}, err
	}
	return plan, nil
}

// Evaluate judges an outcome. It is [loop.Evaluator].
func (r *Reasoner) Evaluate(ctx context.Context, goal loop.Goal, task loop.Task, outcome loop.Outcome) (loop.Evaluation, error) {
	p := prompt.MustLoad("loop/evaluate")
	text, err := p.Render(map[string]any{
		"Objective":       goal.Objective,
		"TaskDescription": task.Description,
		"Instructions":    task.Instructions,
		"ExecutorSuccess": outcome.Success,
		"Output":          outcome.Output,
		"Error":           outcome.Error,
	})
	if err != nil {
		return loop.Evaluation{}, err
	}

	eval, res, err := llm.Structured[loop.Evaluation](ctx, r.svc, r.request(goal, "loop-evaluate", p, text), evaluationSchema())
	r.record(res)
	if err != nil {
		return loop.Evaluation{}, err
	}
	return eval, nil
}

// Reflect analyses a failure and proposes a change. It is [loop.Reflector].
func (r *Reasoner) Reflect(ctx context.Context, goal loop.Goal, task loop.Task, outcome loop.Outcome, eval loop.Evaluation) (loop.Reflection, error) {
	p := prompt.MustLoad("loop/reflect")
	text, err := p.Render(map[string]any{
		"Objective":        goal.Objective,
		"TaskDescription":  task.Description,
		"Instructions":     task.Instructions,
		"Output":           outcome.Output,
		"Error":            outcome.Error,
		"EvaluationReason": eval.Reason,
	})
	if err != nil {
		return loop.Reflection{}, err
	}

	refl, res, err := llm.Structured[loop.Reflection](ctx, r.svc, r.request(goal, "loop-reflect", p, text), reflectionSchema())
	r.record(res)
	if err != nil {
		return loop.Reflection{}, err
	}
	return refl, nil
}

// Summarise writes the loop's final account. It is [loop.Summariser].
func (r *Reasoner) Summarise(ctx context.Context, goal loop.Goal, state loop.State) (loop.Summary, error) {
	p := prompt.MustLoad("loop/summarise")

	// The FACTUAL outcome is stated by the loop, not invented by the model — the prompt asks
	// the model to write prose around a fact it is given, not to decide whether the loop
	// succeeded. That decision was made by the reducer and is not the summariser's to revise.
	outcome := "achieved"
	if state.Stop != loop.StopGoalAchieved && state.Stop != loop.StopNone {
		outcome = "stopped: " + string(state.Stop)
	}

	text, err := p.Render(map[string]any{
		"Objective":   goal.Objective,
		"Outcome":     outcome,
		"Completed":   taskIDs(state.Completed),
		"Failed":      taskIDs(state.Failed),
		"Iterations":  state.Iterations,
		"Reflections": len(state.ReflectionHistory),
		"CostUSD":     fmt.Sprintf("%.4f", state.CostUSD),
		"LastResult":  lastResult(state),
	})
	if err != nil {
		return loop.Summary{}, err
	}

	summary, res, err := llm.Structured[loop.Summary](ctx, r.svc, r.request(goal, "loop-summarise", p, text), summarySchema())
	r.record(res)
	if err != nil {
		return loop.Summary{}, err
	}
	return summary, nil
}

// request builds the inference request shared by every reasoning stage: the prompt, the
// correlation chain, and the provenance fields that make a generation traceable. Temperature
// is low — this is analysis, not invention — and the correlation carries the loop's chain so
// a reasoning call can be tied back to the goal that prompted it.
func (r *Reasoner) request(goal loop.Goal, purpose string, p prompt.Prompt, text string) llm.Request {
	return llm.Request{
		Prompt:              text,
		Purpose:             llm.Purpose(purpose),
		PromptName:          p.Name,
		PromptCategory:      p.Category,
		PromptVersion:       p.Version,
		CorrelationID:       goal.CorrelationID,
		WorkflowExecutionID: goal.WorkflowExecutionID,
		Options:             llm.Options{Temperature: 0, MaxTokens: 2048},
	}
}

// record captures the cost of a reasoning call from its token usage and the provider's price.
// It is provider-agnostic: the router reports the price of whoever it routed to, so this is
// correct whether the call went to Bedrock (real cost) or Ollama (zero).
func (r *Reasoner) record(res llm.Response) {
	caps := r.svc.Provider().Capabilities()
	const perMillion = 1_000_000.0
	r.lastCostUSD = float64(res.Usage.PromptTokens)/perMillion*caps.CostPer1MInputTokensUSD +
		float64(res.Usage.CompletionTokens)/perMillion*caps.CostPer1MOutputTokensUSD
}

func repoLine(repo loop.Repository) string {
	line := repo.Name
	if line == "" {
		line = repo.URL
	}
	if repo.Branch != "" {
		line += " (" + repo.Branch + ")"
	}
	return line
}

func paramLines(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	var b strings.Builder
	for k, v := range params {
		fmt.Fprintf(&b, "- %s: %s\n", k, v)
	}
	return strings.TrimSpace(b.String())
}

func taskIDs(outcomes []loop.Outcome) string {
	ids := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		ids = append(ids, o.TaskID)
	}
	if len(ids) == 0 {
		return "(none)"
	}
	return strings.Join(ids, ", ")
}

func lastResult(s loop.State) string {
	if len(s.Completed) == 0 {
		return ""
	}
	return s.Completed[len(s.Completed)-1].Output
}
