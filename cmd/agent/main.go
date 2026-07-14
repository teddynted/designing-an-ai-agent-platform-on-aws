// Command agent submits tasks to the platform's OpenClaw agent runtime, and
// follows them.
//
// It exists for the same reason cmd/workflow does, plus one that is specific to
// agents: an agent run **costs money and takes minutes**, so the ability to look at
// one, follow it, and *stop* it from a terminal is not a convenience. It is the
// only thing standing between "the prompt was wrong" and "the prompt was wrong for
// forty minutes".
//
//	agent list                                    what is wired up
//	agent submit blog-draft --repo … --sha …      start one, return immediately
//	agent status <execution-id>                   where is it
//	agent result <execution-id>                   what did it produce
//	agent watch  <execution-id>                   follow it to the end
//	agent cancel <execution-id>                   STOP SPENDING
//	agent run    blog-draft --repo …              submit and wait (see the warning)
//
// Configuration comes from the environment (see internal/openclaw). Nothing is
// hard-coded and no secret is ever printed.
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
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/openclaw"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(exitCode(err))
	}
}

// Exit codes are distinct so a script can tell a misconfiguration from an outage
// from an agent that simply did a bad job — without parsing text.
const (
	exitUsage     = 1
	exitConfig    = 2
	exitAuth      = 3
	exitUnavail   = 4
	exitAgentFail = 5
	exitRejected  = 6
	exitOther     = 7
)

func exitCode(err error) int {
	switch {
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, openclaw.ErrConfig), errors.Is(err, agent.ErrUnknownTask), errors.Is(err, agent.ErrInvalidRequest):
		return exitConfig
	case errors.Is(err, agent.ErrUnauthorized):
		return exitAuth
	case errors.Is(err, agent.ErrUnavailable), errors.Is(err, agent.ErrTimeout):
		return exitUnavail
	case errors.Is(err, agent.ErrOutputRejected):
		// Distinct on purpose: this is not "the agent failed", it is "the agent
		// produced something we refused to publish", and somebody needs to look.
		return exitRejected
	case errors.Is(err, agent.ErrAgentFailed), errors.Is(err, agent.ErrExecutionTimeout):
		return exitAgentFail
	default:
		return exitOther
	}
}

var errUsage = errors.New("usage")

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errUsage
	}

	// Ctrl-C cancels the in-flight HTTP call and the polling loop. It does NOT
	// cancel the agent — that is `agent cancel`, and conflating them would mean a
	// stray Ctrl-C silently killed a twenty-minute run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "list":
		return list()
	case "submit":
		return submit(ctx, args[1:], false)
	case "run":
		return submit(ctx, args[1:], true)
	case "status":
		return status(ctx, args[1:])
	case "result":
		return result(ctx, args[1:])
	case "watch":
		return watch(ctx, args[1:])
	case "cancel":
		return cancel(ctx, args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

func service(level slog.Level) (*agent.Service, openclaw.Config, error) {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := openclaw.ConfigFromEnv()
	if err != nil {
		return nil, openclaw.Config{}, err
	}
	runtime, err := openclaw.New(cfg, log)
	if err != nil {
		return nil, openclaw.Config{}, err
	}
	return agent.NewService(runtime, log), cfg, nil
}

func list() error {
	_, cfg, err := service(slog.LevelWarn)
	if err != nil {
		return err
	}
	pretty, _ := json.MarshalIndent(cfg.Redacted(), "", "  ")
	fmt.Printf("OpenClaw integration\n%s\n\ntasks:\n", pretty)
	for _, task := range cfg.Tasks() {
		name, _ := cfg.AgentFor(task)
		fmt.Printf("  %-22s → %s\n", task, name)
	}
	return nil
}

func submit(ctx context.Context, args []string, wait bool) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	var (
		correlation  = fs.String("correlation", "", "correlation ID — the GitHub delivery this is for. Defaults to a timestamp; see the warning")
		workflowExec = fs.String("workflow-execution", "", "the n8n execution that asked for this")
		instructions = fs.String("instructions", "", "what the agent should do (required)")
		repo         = fs.String("repo", "", "repository, owner/name (required)")
		repoURL      = fs.String("repo-url", "", "repository URL (defaults to https://github.com/<repo>)")
		branch       = fs.String("branch", "main", "branch")
		sha          = fs.String("sha", "", "commit SHA")
		message      = fs.String("message", "", "commit message")
		maxSteps     = fs.Int("max-steps", 0, "step budget (0 = the configured default)")
		maxDuration  = fs.Duration("max-duration", 0, "wall-clock budget (0 = the configured default)")
		params       = fs.String("params", "", "task parameters, key=value,key=value")
		timeout      = fs.Duration("wait-timeout", 30*time.Minute, "how long to follow it, for `run`")
		verbose      = fs.Bool("v", false, "log at debug level")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: agent %s <task-type> [flags]\n\nflags:\n", map[bool]string{true: "run", false: "submit"}[wait])
		fs.PrintDefaults()
	}

	// The task type comes off the front BEFORE the flags are parsed. Go's flag
	// package stops at the first positional argument, so parsing them the other way
	// round silently ignores every flag — which is how Milestone 5's CLI shipped a
	// bug that no unit test could see.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fs.Usage()
		return fmt.Errorf("%w: which task?", errUsage)
	}
	taskType := agent.TaskType(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		return errUsage
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	svc, cfg, err := service(level)
	if err != nil {
		return err
	}

	req := agent.Request{
		CorrelationID:       *correlation,
		WorkflowExecutionID: *workflowExec,
		Task: agent.Task{
			Type:         taskType,
			Instructions: *instructions,
			Repository: agent.Repository{
				Name:          *repo,
				URL:           *repoURL,
				Branch:        *branch,
				CommitSHA:     *sha,
				CommitMessage: *message,
			},
			Parameters: parsePairs(*params),
			Limits: agent.Limits{
				MaxSteps:       orInt(*maxSteps, cfg.Limits.MaxSteps),
				MaxDuration:    orDuration(*maxDuration, cfg.Limits.MaxDuration),
				MaxOutputBytes: cfg.Limits.MaxOutputBytes,
			},
		},
	}
	if req.Task.Repository.URL == "" && req.Task.Repository.Name != "" {
		req.Task.Repository.URL = "https://github.com/" + req.Task.Repository.Name
	}
	if req.CorrelationID == "" {
		// A generated correlation means a generated idempotency key, which means
		// OpenClaw cannot tell this from a brand-new request. Fine for a smoke test,
		// wrong for a re-run — and expensive, because the re-run is a second agent.
		req.CorrelationID = fmt.Sprintf("cli-%d", time.Now().UnixNano())
		fmt.Fprintf(os.Stderr,
			"note: no --correlation given, so this submit gets a fresh idempotency key (%s).\n"+
				"      To RE-RUN an existing request safely, pass its original --correlation.\n\n",
			req.CorrelationID)
	}

	exec, err := svc.Submit(ctx, req)
	if err != nil {
		return err
	}

	fmt.Printf("\nsubmitted  %s\nexecution  %s\nagent      %s\nstatus     %s\n",
		exec.TaskType, exec.ID, exec.Agent, exec.Status)
	fmt.Printf("budget     %d steps · %s\n", req.Task.Limits.MaxSteps, req.Task.Limits.MaxDuration)

	if !wait {
		fmt.Printf("\nfollow it:  agent watch %s\nstop it:    agent cancel %s\n", exec.ID, exec.ID)
		return nil
	}

	fmt.Printf("\nwaiting (Ctrl-C stops WATCHING, not the agent — use `agent cancel %s`)\n\n", exec.ID)
	res, err := svc.Wait(ctx, exec.ID, agent.WaitPolicy{Interval: cfg.PollInterval, Timeout: *timeout})
	if err != nil {
		return err
	}
	printResult(res)
	return nil
}

func status(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("%w: agent status <execution-id>", errUsage)
	}
	svc, _, err := service(slog.LevelWarn)
	if err != nil {
		return err
	}
	exec, err := svc.Status(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("execution  %s\nagent      %s\nstatus     %s\nsteps      %d\ncost       $%.4f\n",
		exec.ID, exec.Agent, exec.Status, exec.Steps, exec.Cost)
	if exec.Error != "" {
		fmt.Printf("error      %s\n", exec.Error)
	}
	return nil
}

func result(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("%w: agent result <execution-id>", errUsage)
	}
	svc, _, err := service(slog.LevelWarn)
	if err != nil {
		return err
	}
	res, err := svc.Result(ctx, args[0])
	if err != nil {
		return err
	}
	printResult(res)
	return nil
}

func watch(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("%w: agent watch <execution-id>", errUsage)
	}
	svc, cfg, err := service(slog.LevelInfo)
	if err != nil {
		return err
	}
	res, err := svc.Wait(ctx, args[0], agent.WaitPolicy{Interval: cfg.PollInterval, Timeout: 30 * time.Minute})
	if err != nil {
		return err
	}
	printResult(res)
	return nil
}

func cancel(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("%w: agent cancel <execution-id>", errUsage)
	}
	svc, _, err := service(slog.LevelInfo)
	if err != nil {
		return err
	}
	return svc.Cancel(ctx, args[0])
}

func printResult(res agent.Result) {
	fmt.Printf("\n%s %s in %s (%d steps, $%.4f)\n",
		res.TaskType, res.Status, res.Duration(), res.Steps, res.Cost)
	if len(res.Output.Artifacts) > 0 {
		fmt.Println("\nartifacts:")
		for _, a := range res.Output.Artifacts {
			fmt.Printf("  %-30s %s\n", a.Path, a.URI)
		}
	}
	if res.Output.Content != "" {
		fmt.Printf("\n--- output (%d bytes) ---\n%s\n", len(res.Output.Content), res.Output.Content)
	}
}

func parsePairs(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func orInt(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

func orDuration(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}

func usage() {
	fmt.Fprint(os.Stderr, `agent — run the platform's OpenClaw tasks

  agent list                          what is wired up
  agent submit <task> [flags]         start one; returns immediately
  agent run    <task> [flags]         start one and follow it to the end
  agent status <execution-id>         where is it
  agent watch  <execution-id>         follow it
  agent result <execution-id>         what did it produce
  agent cancel <execution-id>         stop it spending

An agent run takes minutes and costs money. `+"`submit`"+` is the honest shape:
start it, and let something durable (n8n) do the waiting.

Configuration comes from the environment:

  OPENCLAW_BASE_URL   where OpenClaw is     (required)
  OPENCLAW_TOKEN      the credential        (required, never printed)
  OPENCLAW_AGENTS     task=agent,...        (required, unless a default is set)
  OPENCLAW_MAX_STEPS / OPENCLAW_MAX_DURATION   the budget (defaults 40 / 20m)

See AGENTS.md.
`)
}
