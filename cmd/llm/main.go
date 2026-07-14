// Command llm runs inference against the platform's configured LLM provider.
//
// It exists for the reasons the other integration CLIs do — four things break in
// production and none of them can be mocked: the endpoint is wrong, the model was never
// pulled, the network does not permit it, and the prompt does not fit — plus one that is
// specific to inference:
//
//	You cannot tell whether a model is any good by reading its API contract.
//
// A summarisation prompt that works beautifully against a 70B model produces confident
// nonsense on a 3B one, and the only way to find that out is to run it and read what
// comes back. This is the thing you run while iterating on a prompt, and it streams, so
// you can watch the model think rather than waiting to find out.
//
//	llm models                       what is on the box
//	llm check                        does the configured model exist?
//	llm generate --prompt "…"        stream a completion
//	llm generate --prompt-file p.txt --no-stream
//
// Configuration comes from the environment (see internal/ollama).
package main

import (
	"bufio"
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
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/format"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/n8n"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/openclaw"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/prompt"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/providers"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/tools"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/workflow"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(exitCode(err))
	}
}

// Exit codes are distinct so a script can tell a misconfiguration from an outage from a
// model that simply is not there.
const (
	exitUsage    = 1
	exitConfig   = 2
	exitNoModel  = 3
	exitUnavail  = 4
	exitTooBig   = 5
	exitOtherErr = 6
)

func exitCode(err error) int {
	switch {
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, providers.ErrConfig), errors.Is(err, llm.ErrInvalidRequest),
		errors.Is(err, llm.ErrUnsupported):
		return exitConfig
	case errors.Is(err, llm.ErrModelNotFound):
		return exitNoModel
	case errors.Is(err, llm.ErrContextExceeded):
		// Distinct: this is not an outage and not a typo. The prompt is too big, and the
		// fix is to chunk or summarise it — which a caller can automate on this code.
		return exitTooBig
	case errors.Is(err, llm.ErrUnavailable), errors.Is(err, llm.ErrTimeout), errors.Is(err, llm.ErrStalled):
		return exitUnavail
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

	// Ctrl-C cancels the generation.
	//
	// For `generate` that is free: a single inference has no side effects, so stopping it
	// costs nothing but the tokens already produced.
	//
	// For `converse` it is NOT free, and Milestone 9 is where that stopped being true. If
	// the model has already called run_workflow, the workflow is running, and Ctrl-C stops
	// the conversation — not the workflow. The loop says so when it happens.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "models":
		return models(ctx)
	case "check":
		return check(ctx)
	case "generate":
		return generate(ctx, args[1:])
	case "converse":
		return converse(ctx, args[1:])
	case "triage":
		return triage(ctx, args[1:])
	case "compose":
		return compose(ctx, args[1:])
	case "prompts":
		return listPrompts()
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

// service builds whichever provider LLM_PROVIDER names.
//
// Note what this function does NOT do: name a vendor. Milestone 8 added Bedrock, and the
// only line in this CLI that changed is this one — everything below still talks to an
// llm.Service, which talks to an llm.Provider. That is the abstraction being worth the
// trouble, rather than merely being tidy.
//
// It returns the provider's REDACTED configuration (not a vendor type), so `models` can
// print what the platform is about to talk to without this file learning what a
// bedrock.Config looks like.
func service(ctx context.Context, level slog.Level) (*llm.Service, providers.Info, error) {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	provider, info, err := providers.New(ctx, log)
	if err != nil {
		return nil, providers.Info{}, err
	}
	return llm.NewService(provider, log), info, nil
}

func models(ctx context.Context) error {
	svc, info, err := service(ctx, slog.LevelWarn)
	if err != nil {
		return err
	}

	pretty, _ := json.MarshalIndent(info.Redacted, "", "  ")
	fmt.Printf("provider: %s\n%s\n\n", svc.Provider().Name(), pretty)

	list, err := svc.Models(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no models on this instance — pull one:  ollama pull llama3.2")
		return nil
	}

	fmt.Printf("%-28s %-10s %-10s %s\n", "MODEL", "PARAMS", "QUANT", "SIZE")
	for _, m := range list {
		fmt.Printf("%-28s %-10s %-10s %.1f GB\n",
			m.Name, orDash(m.ParameterSize), orDash(m.Quantization), float64(m.SizeBytes)/1e9)
	}

	caps := svc.Provider().Capabilities()
	fmt.Printf("\nlocal: %v  (the prompt does not leave the network)\ncontext window: %d tokens\n",
		caps.Local, caps.MaxContextTokens)
	return nil
}

// check is meant for start-up, and for the moment after someone changes OLLAMA_MODEL.
// A model that was never pulled is a configuration error, and finding out about it while
// a user waits is strictly worse than finding out while nobody does.
func check(ctx context.Context) error {
	svc, info, err := service(ctx, slog.LevelInfo)
	if err != nil {
		return err
	}
	if err := svc.EnsureModel(ctx, info.Model); err != nil {
		return err
	}
	fmt.Printf("\n%s is available on %s (%s)\n", info.Model, info.Endpoint, info.Provider)
	return nil
}

func generate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	var (
		model       = fs.String("model", "", "model to use (default: the configured one)")
		system      = fs.String("system", "", "the system prompt — how the model should behave")
		prompt      = fs.String("prompt", "", "the prompt")
		promptFile  = fs.String("prompt-file", "", "read the prompt from a file (use - for stdin)")
		purpose     = fs.String("purpose", "cli", "what this inference is for, for the logs")
		correlation = fs.String("correlation", "", "correlation ID, to tie this to a GitHub delivery")
		temperature = fs.Float64("temperature", 0, "0 = the configured default")
		maxTokens   = fs.Int("max-tokens", 0, "completion budget (0 = the configured default)")
		seed        = fs.Int("seed", 0, "make the generation reproducible")
		noStream    = fs.Bool("no-stream", false, "wait for the whole answer instead of streaming it")
		verbose     = fs.Bool("v", false, "log at debug level")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: llm generate [flags]\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	svc, info, err := service(ctx, level)
	if err != nil {
		return err
	}

	text, err := readPrompt(*prompt, *promptFile)
	if err != nil {
		return err
	}

	req := llm.Request{
		Model:         *model,
		System:        *system,
		Prompt:        text,
		Purpose:       llm.Purpose(*purpose),
		CorrelationID: *correlation,
		Options: llm.Options{
			Temperature: *temperature,
			MaxTokens:   *maxTokens,
			Seed:        *seed,
		},
	}
	if req.CorrelationID == "" {
		req.CorrelationID = fmt.Sprintf("cli-%d", time.Now().UnixNano())
	}

	stream := info.Stream && !*noStream

	fmt.Fprintf(os.Stderr, "\n--- %s (%s) ---\n", orDefault(*model, info.Model), map[bool]string{true: "streaming", false: "buffered"}[stream])

	var res llm.Response
	if stream {
		// Print tokens as they arrive. This is the whole reason to prefer streaming: a
		// ninety-second generation you can watch is a ninety-second generation; one you
		// cannot is indistinguishable from a hang.
		out := bufio.NewWriter(os.Stdout)
		res, err = svc.Stream(ctx, req, func(c llm.Chunk) error {
			if c.Content != "" {
				_, _ = out.WriteString(c.Content)
				_ = out.Flush() // unbuffered, deliberately: a buffered stream is not a stream
			}
			return nil
		})
		_ = out.Flush()
		fmt.Println()
	} else {
		res, err = svc.Generate(ctx, req)
		if err == nil {
			fmt.Println(res.Content)
		}
	}
	if err != nil {
		return err
	}

	// The numbers that matter, on stderr so that stdout is just the completion and can
	// be piped into something else.
	fmt.Fprintf(os.Stderr, "\n--- %d tokens in %s · %.1f tok/s · load %s · finish: %s ---\n",
		res.Usage.CompletionTokens, res.Duration.Round(time.Millisecond),
		res.Usage.TokensPerSecond, res.Usage.LoadDuration.Round(time.Millisecond), res.FinishReason)

	if res.Usage.TokensPerSecond > 0 && res.Usage.TokensPerSecond < 10 {
		// The single most useful diagnostic in the whole integration.
		fmt.Fprintf(os.Stderr, "note: %.1f tok/s is CPU-speed. If this box has a GPU, the model is not using it.\n",
			res.Usage.TokensPerSecond)
	}
	if res.FinishReason == "length" {
		fmt.Fprintln(os.Stderr, "warning: the answer was CUT OFF by the token budget — raise --max-tokens.")
	}
	return nil
}

func readPrompt(prompt, file string) (string, error) {
	switch {
	case prompt != "" && file != "":
		return "", fmt.Errorf("%w: use --prompt or --prompt-file, not both", errUsage)
	case file == "-":
		raw, err := os.ReadFile("/dev/stdin")
		if err != nil {
			return "", fmt.Errorf("%w: reading stdin: %v", llm.ErrInvalidRequest, err)
		}
		return string(raw), nil
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("%w: reading --prompt-file: %v", llm.ErrInvalidRequest, err)
		}
		return string(raw), nil
	case prompt != "":
		return prompt, nil
	default:
		return "", fmt.Errorf("%w: no prompt (use --prompt or --prompt-file)", errUsage)
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func usage() {
	fmt.Fprint(os.Stderr, `llm — run inference against the platform's provider

  llm models                     what models the provider has
  llm check                      is the configured model actually there?
  llm generate --prompt "…"      stream a completion
  llm converse --prompt "…"      let the model USE THE PLATFORM'S TOOLS  (M9)
  llm triage --diff-file f.diff  a STRUCTURED answer, not prose          (M9)
  llm compose --template … --format mermaid|yaml|json|markdown|table     (M9)
                                 generate an artefact, and REFUSE to return
                                 it unless it actually parses
  llm prompts                    the prompt catalogue, by capability      (M9)
  llm <command> -h               the flags

Streaming is the default. A generation you cannot watch is indistinguishable from
a hang — and a stall is only detectable in a stream.

Configuration comes from the environment:

  OLLAMA_BASE_URL       where Ollama is       (required)
  OLLAMA_MODEL          the default model     (required)
  OLLAMA_CONTEXT_TOKENS the context window    (default 8192 — get this RIGHT:
                        an oversized prompt is silently truncated, not refused)
  OLLAMA_IDLE_TIMEOUT   stall detection       (default 60s)
  OLLAMA_MAX_TOKENS     completion budget     (default 2048)

converse and triage need a model that can use tools — that means Claude, through
Bedrock:

  LLM_PROVIDER=bedrock
  BEDROCK_MODEL_ID=us.anthropic.claude-sonnet-4-20250514-v1:0

converse also needs n8n (N8N_BASE_URL) and OpenClaw (OPENCLAW_BASE_URL), because the
platform's tools ARE its integrations: the model can list and RUN workflows, and hand
work to an agent. Two of those tools change things, and the model is told so.

See INFERENCE.md.
`)
}

// converse lets the model use the platform's own tools.
//
// This is the milestone in one command. The model can list the workflows, run one, list
// the agent's task types, and submit one — and two of those four CHANGE THINGS, which is
// why the loop, not the model, decides what happens when something fails afterwards.
func converse(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("converse", flag.ContinueOnError)
	ask := fs.String("prompt", "", "what to ask (required)")
	repo := fs.String("repo", "teddynted/designing-an-ai-agent-platform-on-aws", "the repository this is about")
	branch := fs.String("branch", "main", "the branch")
	correlation := fs.String("correlation", "", "correlation ID (default: derived)")
	maxTurns := fs.Int("max-turns", llm.DefaultMaxIterations, "how many times the model may go round")
	maxCost := fs.Float64("max-cost", 0.50, "stop if the conversation costs more than this (USD)")
	reasoning := fs.Int("reasoning", 0, "extended thinking budget in tokens (0 = off)")
	verbose := fs.Bool("v", false, "show the tool calls as they happen")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: llm converse --prompt \"…\"\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if strings.TrimSpace(*ask) == "" {
		fs.Usage()
		return fmt.Errorf("%w: --prompt is required", errUsage)
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	provider, info, err := providers.New(ctx, log)
	if err != nil {
		return err
	}
	svc := llm.NewService(provider, log)

	// Refuse early and clearly. A model that cannot use tools does not say so — it ignores
	// them and answers from memory, which is the whole reason Capabilities exists.
	if !provider.Capabilities().Tools {
		return fmt.Errorf("%w: %s cannot use tools. Set LLM_PROVIDER=bedrock with a Claude "+
			"model (BEDROCK_MODEL_ID=us.anthropic.claude-sonnet-4-20250514-v1:0)",
			llm.ErrUnsupported, info.Provider)
	}

	// The platform's tools ARE its integrations.
	wf, err := workflowService(log)
	if err != nil {
		return fmt.Errorf("converse needs n8n, because run_workflow is one of the tools: %w", err)
	}
	ag, err := agentService(log)
	if err != nil {
		return fmt.Errorf("converse needs OpenClaw, because submit_agent_task is one of the tools: %w", err)
	}

	corr := *correlation
	if corr == "" {
		corr = "cli:" + time.Now().UTC().Format("20060102T150405")
	}

	registry := tools.New(tools.Origin{
		CorrelationID: corr,
		Repository: agent.Repository{
			Name:   *repo,
			URL:    "https://github.com/" + *repo,
			Branch: *branch,
		},
	})
	registry.
		Register(tools.ListWorkflows(wf)).
		Register(tools.RunWorkflow(wf)).
		Register(tools.ListAgentTasks(ag)).
		Register(tools.SubmitAgentTask(ag, platformInstructions()))

	// The system prompt is versioned and lives in internal/prompt, not in this file.
	sys := prompt.MustLoad("workflow/tool-use-system")
	system, err := sys.Render(map[string]any{
		"Repository": *repo, "Branch": *branch, "CommitSHA": "",
	})
	if err != nil {
		return err
	}

	req := llm.Request{
		System:         system,
		Prompt:         *ask,
		Purpose:        "converse",
		CorrelationID:  corr,
		PromptName:     sys.Name,
		PromptCategory: sys.Category,
		PromptVersion:  sys.Version,
		Options:        llm.Options{MaxTokens: 4096, Temperature: 0.2},
	}
	if *reasoning > 0 {
		req.Reasoning = &llm.ReasoningConfig{BudgetTokens: *reasoning}
	}

	fmt.Fprintf(os.Stderr, "--- %s · %s · %d tools · prompt %s ---\n",
		info.Provider, info.Model, len(registry.Specs()), sys.Version)

	convo, err := svc.Converse(ctx, req, registry, llm.LoopPolicy{
		MaxIterations: *maxTurns,
		MaxCostUSD:    *maxCost,
	})

	// Print what happened BEFORE returning any error — because if a Write tool ran, the
	// operator needs to know that far more urgently than they need the error text.
	for _, turn := range convo.Turns {
		for i, call := range turn.Response.ToolCalls {
			marker := "·"
			if i < len(turn.Results) && turn.Results[i].IsError {
				marker = "✗"
			}
			fmt.Fprintf(os.Stderr, "  %s turn %d: %s\n", marker, turn.Index+1, call.Name)
		}
	}

	if err != nil {
		if convo.EffectsCommitted {
			// The loudest thing this CLI can say.
			fmt.Fprintf(os.Stderr, "\n!!! A TOOL ALREADY CHANGED SOMETHING before this failed.\n"+
				"    Do NOT simply re-run this command: a workflow has been triggered or an\n"+
				"    agent task submitted, and running it again would do it twice.\n")
		}
		return err
	}

	fmt.Println(convo.Content)
	fmt.Fprintf(os.Stderr, "\n--- %d turns · %d in / %d out · ~$%.4f · effects: %v ---\n",
		len(convo.Turns), convo.Usage.PromptTokens, convo.Usage.CompletionTokens,
		convo.EstimatedCostUSD, convo.EffectsCommitted)
	return nil
}

// Triage is what the platform wants back from a model: a decision it can branch on, not
// prose it has to parse.
type Triage struct {
	Severity string   `json:"severity"`
	Summary  string   `json:"summary"`
	Files    []string `json:"files"`
}

// Validate is the check a JSON Schema cannot make. See llm.Validator.
func (t Triage) Validate() error {
	if t.Severity == "critical" && len(t.Files) == 0 {
		return fmt.Errorf("a critical finding must cite at least one file")
	}
	return nil
}

func triage(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	file := fs.String("diff-file", "", "a file containing the diff (required)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: llm triage --diff-file CHANGES.diff\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if *file == "" {
		fs.Usage()
		return fmt.Errorf("%w: --diff-file is required", errUsage)
	}

	diff, err := os.ReadFile(*file)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	provider, _, err := providers.New(ctx, log)
	if err != nil {
		return err
	}
	svc := llm.NewService(provider, log)

	p := prompt.MustLoad("structured/change-triage")
	text, err := p.Render(map[string]any{"Diff": string(diff)})
	if err != nil {
		return err
	}

	result, res, err := llm.Structured[Triage](ctx, svc, llm.Request{
		Prompt:         text,
		Purpose:        "change-triage",
		PromptName:     p.Name,
		PromptCategory: p.Category,
		PromptVersion:  p.Version,
		CorrelationID:  "cli:triage",
		Options:        llm.Options{MaxTokens: 1024, Temperature: 0},
	}, llm.Schema{
		Name:        "change_triage",
		Description: "Triage a change to the platform.",
		Definition: llm.Object(map[string]any{
			"severity": llm.String("How bad is it?", "low", "medium", "high", "critical"),
			"summary":  llm.String("One sentence, for an engineer who has not seen the branch."),
			"files": map[string]any{
				"type": "array", "description": "The files that drove the rating.",
				"items": map[string]any{"type": "string"},
			},
		}, "severity", "summary"),
	})
	if err != nil {
		return err
	}

	// A typed value. Not prose that something downstream has to parse with a regex and a
	// prayer — a struct the platform can branch on.
	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(out))
	fmt.Fprintf(os.Stderr, "\n--- %d in / %d out · prompt %s ---\n",
		res.Usage.PromptTokens, res.Usage.CompletionTokens, p.Version)
	return nil
}

// workflowService and agentService build the platform's cores from the environment — the
// same code path cmd/workflow and cmd/agent use.
func workflowService(log *slog.Logger) (*workflow.Service, error) {
	cfg, err := n8n.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	engine, err := n8n.New(cfg, log)
	if err != nil {
		return nil, err
	}
	return workflow.NewService(engine, log), nil
}

func agentService(log *slog.Logger) (*agent.Service, error) {
	cfg, err := openclaw.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	runtime, err := openclaw.New(cfg, log)
	if err != nil {
		return nil, err
	}
	return agent.NewService(runtime, log), nil
}

// platformInstructions is what the agent is actually TOLD, keyed by task type.
//
// The model chooses a key. It never writes a value. That is the entire defence against a
// hostile diff talking the model into authoring an instruction — see internal/tools.
func platformInstructions() tools.Instructions {
	return tools.Instructions{
		agent.TaskPRSummary: "Summarise this pull request for a reviewer. Read the diff and the " +
			"commit messages. Do not modify any file. Do not run any command that changes state.",
		agent.TaskBlogDraft: "Draft a technical blog post from the repository's recent changes. " +
			"Write to docs/blog/ only. Do not modify source code.",
		agent.TaskReleaseNotes: "Write release notes from the commits since the last tag. " +
			"Write to CHANGELOG.md only.",
	}
}

// compose generates an artefact in a given format — and does not hand it back unless it is
// valid.
//
// This is the command that dogfoods the milestone. `--format mermaid` asks Claude for a
// diagram, and the platform refuses to return one that would render as a red error box on
// a page somebody is reading. The validator knows about the two Mermaid bugs that actually
// shipped in this repository, because they shipped in this repository.
func compose(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("compose", flag.ContinueOnError)
	template := fs.String("template", "", "a prompt from the catalogue, e.g. architecture/mermaid-diagram (required)")
	kind := fs.String("format", "markdown", "json | yaml | markdown | mermaid | table | text")
	out := fs.String("out", "", "write to this file instead of stdout")
	varFlags := multiFlag{}
	fs.Var(&varFlags, "var", "a template variable: --var Subject=\"the tool loop\" (repeatable)")
	contextFile := fs.String("context-file", "", "a file whose contents become {{.Context}}")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: llm compose --template architecture/mermaid-diagram "+
			"--format mermaid --var Subject=\"the tool loop\"\n\nflags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nprompts:\n")
		for _, name := range prompt.Names() {
			fmt.Fprintf(os.Stderr, "  %s\n", name)
		}
	}
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if *template == "" {
		fs.Usage()
		return fmt.Errorf("%w: --template is required", errUsage)
	}

	k, err := format.Parse(*kind)
	if err != nil {
		return err
	}
	f := format.For(k)

	p, err := prompt.Load(*template)
	if err != nil {
		return err
	}

	data := map[string]any{}
	for k, v := range varFlags {
		data[k] = v
	}
	if *contextFile != "" {
		body, err := os.ReadFile(*contextFile)
		if err != nil {
			return err
		}
		data["Context"] = string(body)
	}
	if _, ok := data["Context"]; !ok {
		data["Context"] = ""
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	provider, info, err := providers.New(ctx, log)
	if err != nil {
		return err
	}
	svc := llm.NewService(provider, log)

	req := llm.Request{
		Purpose: llm.Purpose(p.Category),
		Options: llm.Options{MaxTokens: 4096, Temperature: 0.2},
	}
	// Apply stamps the prompt's name, category and version onto the request, so the log line
	// can say which prompt produced this and a bill can be grouped by CAPABILITY.
	if err := p.Apply(&req, data); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "--- %s · %s · %s (%s) · format: %s ---\n",
		info.Provider, info.Model, p.Name, p.Version, f.Name())

	content, res, err := svc.Compose(ctx, req, f)
	if err != nil {
		return err
	}

	if *out != "" {
		if err := os.WriteFile(*out, []byte(content+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	} else {
		fmt.Println(content)
	}

	fmt.Fprintf(os.Stderr, "--- %d in / %d out · %s VALIDATED ---\n",
		res.Usage.PromptTokens, res.Usage.CompletionTokens, strings.ToUpper(f.Name()))
	return nil
}

// listPrompts shows the catalogue, by capability.
func listPrompts() error {
	categories := []string{
		prompt.Summarisation, prompt.Structured, prompt.Architecture,
		prompt.Writing, prompt.Workflow,
	}
	for _, c := range categories {
		prompts := prompt.InCategory(c)
		if len(prompts) == 0 {
			continue
		}
		fmt.Printf("%s\n", strings.ToUpper(c))
		for _, p := range prompts {
			fmt.Printf("  %-38s %s\n", p.Name, p.Version)
		}
		fmt.Println()
	}
	return nil
}

// multiFlag collects repeated --var k=v flags.
type multiFlag map[string]string

func (m multiFlag) String() string { return "" }

func (m multiFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	m[k] = val
	return nil
}
