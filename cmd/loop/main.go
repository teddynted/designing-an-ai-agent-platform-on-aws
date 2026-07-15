// Command loop runs the autonomous agent loop against a goal.
//
// It wires the two planes the loop orchestrates — the inference plane (for planning,
// evaluation, reflection and the summary) and the agent runtime (for executing tasks) — into
// the loop's stage interfaces, and drives the reducer to completion with the synchronous
// [loop.Runner].
//
//	loop run --goal "…" --repo owner/name           plan, execute, evaluate, reflect, summarise
//	loop config                                      show the loop, reasoning and runtime config
//	loop plan --goal "…" --repo owner/name           just plan, and print the plan (no execution)
//
// # What it needs configured
//
// Everything the loop touches, it touches through those two planes, so it needs both
// configured:
//
//	LLM_PROVIDER / LLM_ROUTER_*   which model reasons (Milestone 7-10)
//	OPENCLAW_*                    which runtime executes (Milestone 6)
//	LOOP_*                        the shape of the loop (Milestone 11)
//
// # The warning that matters
//
// This command BLOCKS on agent executions, each of which takes minutes to hours. That is
// fine for a CLI. It is NOT how the loop runs in production — there, n8n drives the same
// reducer without blocking. Do not wrap this command in anything request-scoped. See
// ROUTING.md's sibling, LOOP.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/agent"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/loop"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/loop/adapter"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/openclaw"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/providers"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(exitCode(err))
	}
}

// Exit codes, distinct so a script can tell a misconfiguration from a loop that ran and
// stopped short from one that crashed.
const (
	exitUsage    = 1
	exitConfig   = 2
	exitStopped  = 3 // the loop ran and stopped before the goal (a bound, a human, a failure)
	exitOtherErr = 6
)

func exitCode(err error) int {
	switch {
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, loop.ErrConfig), errors.Is(err, providers.ErrConfig),
		errors.Is(err, openclaw.ErrConfig), errors.Is(err, loop.ErrInvalidGoal):
		return exitConfig
	case errors.Is(err, loop.ErrStopped):
		return exitStopped
	default:
		return exitOtherErr
	}
}

var errUsage = errors.New("usage")

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errUsage
	}

	// Ctrl-C cancels the loop. Note what that does and does not stop: it cancels the
	// controller and any inference in flight, but an agent execution that has already started
	// is running on OpenClaw and keeps running until it finishes or is cancelled there. The
	// loop is not the agent's off switch — `agent cancel` is.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "run":
		return runLoop(ctx, args[1:])
	case "plan":
		return planOnly(ctx, args[1:])
	case "config":
		return showConfig(ctx)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

// buildInfo describes what was wired, for `config` to print without a caller having to reach
// into three packages' config types.
type buildInfo struct {
	Provider   string
	LoopConfig loop.Config
	Tasks      []string
}

// build wires everything: the inference service (for reasoning), the agent service (for
// execution), the two adapters, and the runner. It is the composition root — the one place a
// main is allowed to know about every plane at once.
func build(ctx context.Context, level slog.Level) (*loop.Runner, buildInfo, error) {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	loopCfg, err := loop.ConfigFromEnv()
	if err != nil {
		return nil, buildInfo{}, err
	}

	// The inference plane — whichever provider LLM_PROVIDER names, router included.
	provider, info, err := providers.New(ctx, log)
	if err != nil {
		return nil, buildInfo{}, err
	}
	svc := llm.NewService(provider, log)

	// The agent runtime — OpenClaw.
	ocCfg, err := openclaw.ConfigFromEnv()
	if err != nil {
		return nil, buildInfo{}, err
	}
	runtime, err := openclaw.New(ocCfg, log)
	if err != nil {
		return nil, buildInfo{}, err
	}
	agentSvc := agent.NewService(runtime, log)

	// The adapters bind the loop's stages to the planes.
	executor := adapter.NewExecutor(agentSvc, adapter.DefaultExecutorConfig())
	reasoner := adapter.NewReasoner(svc, executor.TaskTypes())

	engines := loop.Engines{
		Planner:    reasoner,
		Executor:   executor,
		Evaluator:  reasoner,
		Reflector:  reasoner,
		Summariser: reasoner,
	}

	runner, err := loop.NewRunner(engines, loopCfg, log)
	if err != nil {
		return nil, buildInfo{}, err
	}

	return runner, buildInfo{Provider: info.Provider, LoopConfig: loopCfg, Tasks: executor.TaskTypes()}, nil
}

func runLoop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	goalText, repo, repoURL, branch, sha, correlation, workflowExec, agentID, params, verbose := goalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	g, err := buildGoal(*goalText, *repo, *repoURL, *branch, *sha, *correlation, *workflowExec, *agentID, *params)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	runner, _, err := build(ctx, level)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n--- running the loop toward: %s ---\n\n", g.Objective)

	state, runErr := runner.Run(ctx, g)

	// Print the account whether the loop achieved the goal or stopped short — a stopped loop
	// still did work worth reporting.
	printSummary(state)

	return runErr
}

// planOnly runs just the planning stage and prints the plan. It is for iterating on the
// planning prompt without spending money on executions — the loop's equivalent of `llm
// generate`, and the reason planning is a stage you can call in isolation.
func planOnly(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	goalText, repo, repoURL, branch, sha, correlation, workflowExec, agentID, params, verbose := goalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	g, err := buildGoal(*goalText, *repo, *repoURL, *branch, *sha, *correlation, *workflowExec, *agentID, *params)
	if err != nil {
		return err
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	provider, _, err := providers.New(ctx, log)
	if err != nil {
		return err
	}
	svc := llm.NewService(provider, log)

	ocCfg, err := openclaw.ConfigFromEnv()
	if err != nil {
		return err
	}
	runtime, err := openclaw.New(ocCfg, log)
	if err != nil {
		return err
	}
	taskTypes := adapter.NewExecutor(agent.NewService(runtime, log), adapter.DefaultExecutorConfig()).TaskTypes()

	plan, err := adapter.NewReasoner(svc, taskTypes).Plan(ctx, g)
	if err != nil {
		return err
	}

	fmt.Printf("plan (%d tasks): %s\n\n", len(plan.Tasks), plan.Rationale)
	for i, t := range plan.Tasks {
		deps := ""
		if len(t.DependsOn) > 0 {
			deps = "  after: " + strings.Join(t.DependsOn, ", ")
		}
		fmt.Printf("  %d. [%s] %s%s\n     %s\n", i+1, t.Type, t.ID, deps, t.Description)
	}
	return nil
}

func showConfig(ctx context.Context) error {
	_, info, err := build(ctx, slog.LevelWarn)
	if err != nil {
		return err
	}
	out := map[string]any{
		"loop":              info.LoopConfig.Redacted(),
		"reasoningProvider": info.Provider,
		"taskTypes":         info.Tasks,
	}
	pretty, _ := json.MarshalIndent(out, "", "  ")
	fmt.Printf("loop configuration\n%s\n", pretty)
	return nil
}

func printSummary(s loop.State) {
	fmt.Fprintf(os.Stderr, "\n--- loop %s (%s) ---\n", s.Phase, orNone(string(s.Stop)))
	fmt.Fprintf(os.Stderr, "iterations: %d · completed: %d · failed: %d · reflections: %d · cost: $%.4f\n",
		s.Iterations, len(s.Completed), len(s.Failed), len(s.ReflectionHistory), s.CostUSD)

	if s.Summary != nil {
		fmt.Printf("\n%s\n", s.Summary.Narrative)
		if s.Summary.Result != "" {
			fmt.Printf("\n--- result ---\n%s\n", s.Summary.Result)
		}
	}
}

// goalFlags registers the flags shared by run and plan. Returning them as a bundle keeps the
// two commands' flag sets identical, which is the point — the difference between them is what
// they DO with the goal, not how they take it.
func goalFlags(fs *flag.FlagSet) (goal, repo, repoURL, branch, sha, correlation, workflowExec, agentID, params *string, verbose *bool) {
	goal = fs.String("goal", "", "the objective, in plain language (required)")
	repo = fs.String("repo", "", "repository, owner/name (required)")
	repoURL = fs.String("repo-url", "", "repository URL (defaults to https://github.com/<repo>)")
	branch = fs.String("branch", "main", "branch")
	sha = fs.String("sha", "", "commit SHA")
	correlation = fs.String("correlation", "", "correlation ID, to tie this to a GitHub delivery (default: derived)")
	workflowExec = fs.String("workflow-execution", "", "the n8n execution that asked for this")
	agentID = fs.String("agent-id", "", "the logical agent this loop acts as, for the logs")
	params = fs.String("params", "", "objective parameters, key=value,key=value")
	verbose = fs.Bool("v", false, "log every stage at debug level")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: loop %s --goal \"…\" --repo owner/name\n\nflags:\n", fs.Name())
		fs.PrintDefaults()
	}
	return
}

func buildGoal(objective, repo, repoURL, branch, sha, correlation, workflowExec, agentID, params string) (loop.Goal, error) {
	if strings.TrimSpace(objective) == "" {
		return loop.Goal{}, fmt.Errorf("%w: --goal is required", errUsage)
	}
	if strings.TrimSpace(repo) == "" {
		return loop.Goal{}, fmt.Errorf("%w: --repo is required", errUsage)
	}
	if repoURL == "" {
		repoURL = "https://github.com/" + repo
	}
	if correlation == "" {
		correlation = fmt.Sprintf("loop-cli-%d", time.Now().UnixNano())
		fmt.Fprintf(os.Stderr, "note: no --correlation given; using %q\n", correlation)
	}

	return loop.Goal{
		Objective:           objective,
		Repository:          loop.Repository{Name: repo, URL: repoURL, Branch: branch, CommitSHA: sha},
		CorrelationID:       correlation,
		WorkflowExecutionID: workflowExec,
		AgentID:             agentID,
		Params:              parseParams(params),
	}, nil
}

func parseParams(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "no stop reason"
	}
	return s
}

func usage() {
	fmt.Fprint(os.Stderr, `loop — run the autonomous agent loop (Milestone 11)

  loop run --goal "…" --repo owner/name    plan → execute → evaluate → reflect → summarise
  loop plan --goal "…" --repo owner/name   just plan, and print it (no execution, no spend)
  loop config                              show the loop, reasoning and runtime configuration
  loop <command> -h                        the flags

The loop REASONS with the inference plane (LLM_PROVIDER / LLM_ROUTER_*) and EXECUTES with the
agent runtime (OPENCLAW_*). Its own shape is LOOP_*:

  LOOP_MAX_ITERATIONS    the hard cap on execution attempts       (default 12)
  LOOP_MAX_RETRIES       attempts per failed task                 (default 2)
  LOOP_MAX_REPLANS       times the plan may be rebuilt            (default 2)
  LOOP_TIMEOUT           the whole loop's wall clock              (default 30m)
  LOOP_MAX_COST_USD      the cost cap                             (default 5)
  LOOP_REFLECTION        learn from a failure before retrying     (default true)

This command BLOCKS on agent executions. That is fine for a CLI and wrong for a Lambda —
in production, n8n drives the same loop without blocking. See LOOP.md.
`)
}
