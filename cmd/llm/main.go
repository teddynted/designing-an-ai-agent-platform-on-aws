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

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/ollama"
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
	case errors.Is(err, ollama.ErrConfig), errors.Is(err, llm.ErrInvalidRequest):
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

	// Ctrl-C cancels the generation. Unlike an agent run (Milestone 6), that is exactly
	// what you want: inference has no side effects, so stopping it costs nothing but the
	// tokens already produced.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "models":
		return models(ctx)
	case "check":
		return check(ctx)
	case "generate":
		return generate(ctx, args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

func service(level slog.Level) (*llm.Service, ollama.Config, error) {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := ollama.ConfigFromEnv()
	if err != nil {
		return nil, ollama.Config{}, err
	}
	provider, err := ollama.New(cfg, log)
	if err != nil {
		return nil, ollama.Config{}, err
	}
	return llm.NewService(provider, log), cfg, nil
}

func models(ctx context.Context) error {
	svc, cfg, err := service(slog.LevelWarn)
	if err != nil {
		return err
	}

	pretty, _ := json.MarshalIndent(cfg.Redacted(), "", "  ")
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
	svc, cfg, err := service(slog.LevelInfo)
	if err != nil {
		return err
	}
	if err := svc.EnsureModel(ctx, cfg.Model); err != nil {
		return err
	}
	fmt.Printf("\n%s is available on %s\n", cfg.Model, cfg.BaseURL)
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
	svc, cfg, err := service(level)
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

	stream := cfg.Stream && !*noStream

	fmt.Fprintf(os.Stderr, "\n--- %s (%s) ---\n", orDefault(*model, cfg.Model), map[bool]string{true: "streaming", false: "buffered"}[stream])

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
  llm generate -h                the flags

Streaming is the default. A generation you cannot watch is indistinguishable from
a hang — and a stall is only detectable in a stream.

Configuration comes from the environment:

  OLLAMA_BASE_URL       where Ollama is       (required)
  OLLAMA_MODEL          the default model     (required)
  OLLAMA_CONTEXT_TOKENS the context window    (default 8192 — get this RIGHT:
                        an oversized prompt is silently truncated, not refused)
  OLLAMA_IDLE_TIMEOUT   stall detection       (default 60s)
  OLLAMA_MAX_TOKENS     completion budget     (default 2048)

See INFERENCE.md.
`)
}
