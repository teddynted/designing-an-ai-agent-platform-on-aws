// Command workflow triggers one of the platform's n8n workflows from the command
// line.
//
// It exists for three reasons, and none of them is "a demo":
//
//   - To prove the integration against a REAL n8n. Unit tests prove the client
//     handles the responses we imagined; this proves the token is right, the
//     webhook path exists, the workflow is active, and the network permits it.
//     Those are the four things that actually break, and none of them can be
//     mocked.
//   - To replay an event by hand when a workflow failed and the fix is to run it
//     again — with the same idempotency key, so a workflow that already did half
//     the job does not do that half twice.
//   - To be the reference caller. When the webhook Lambda arrives in a later
//     milestone, it does exactly what this does, and this stays as the thing you
//     can run when you suspect the Lambda.
//
// Configuration comes from the environment (see internal/n8n). Nothing is
// hard-coded and no secret is ever printed.
//
// Usage:
//
//	workflow list
//	workflow trigger blog-generator --repo teddynted/platform --sha abc123 --branch main \
//	    --message "feat: add a thing" --event push --id delivery-123
//	workflow trigger blog-generator --payload event.json --dry-run
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

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/n8n"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/workflow"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// The error is already structured and already logged; this is the exit code
		// a shell or a CI step reads.
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(exitCode(err))
	}
}

// Exit codes are distinct so a script can tell a misconfiguration from an outage
// without parsing text: a CI job should fail loudly on 2 (fix your config) and
// might reasonably retry on 4 (n8n is down).
const (
	exitOK        = 0
	exitUsage     = 1
	exitConfig    = 2
	exitAuth      = 3
	exitUnavail   = 4
	exitFailed    = 5
	exitOtherwise = 6
)

func exitCode(err error) int {
	switch {
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, n8n.ErrConfig), errors.Is(err, workflow.ErrUnknownWorkflow), errors.Is(err, workflow.ErrInvalidRequest):
		return exitConfig
	case errors.Is(err, workflow.ErrUnauthorized):
		return exitAuth
	case errors.Is(err, workflow.ErrUnavailable), errors.Is(err, workflow.ErrTimeout):
		return exitUnavail
	case errors.Is(err, workflow.ErrWorkflowFailed):
		return exitFailed
	default:
		return exitOtherwise
	}
}

var errUsage = errors.New("usage")

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errUsage
	}

	// Ctrl-C must cancel the in-flight request rather than orphan it: the context
	// runs all the way down into the HTTP call and the retry backoff.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "list":
		return list()
	case "trigger":
		return trigger(ctx, args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

// service builds the real thing from the environment: the same code path the
// platform will use, so a success here means the platform would succeed too.
func service(level slog.Level) (*workflow.Service, n8n.Config, error) {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := n8n.ConfigFromEnv()
	if err != nil {
		return nil, n8n.Config{}, err
	}
	engine, err := n8n.New(cfg, log)
	if err != nil {
		return nil, n8n.Config{}, err
	}
	return workflow.NewService(engine, log), cfg, nil
}

func list() error {
	_, cfg, err := service(slog.LevelWarn)
	if err != nil {
		return err
	}

	// Redacted() shows exactly what we are configured with, including that a token
	// is set and how long it is — and never the token itself. Being able to print
	// this safely is worth a great deal when something is misconfigured at 3am.
	pretty, _ := json.MarshalIndent(cfg.Redacted(), "", "  ")
	fmt.Printf("n8n integration\n%s\n\nworkflows:\n", pretty)
	for _, name := range cfg.Names() {
		fmt.Printf("  %-20s → %s\n", name, cfg.Workflows[name])
	}
	return nil
}

func trigger(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("trigger", flag.ContinueOnError)
	var (
		id       = fs.String("id", "", "event ID (a GitHub delivery ID). Defaults to a timestamp — but see the warning below")
		event    = fs.String("event", "push", "event type: push, pull_request, release")
		action   = fs.String("action", "", "event action: opened, closed, synchronize")
		repo     = fs.String("repo", "", "repository, owner/name")
		repoURL  = fs.String("repo-url", "", "repository URL (defaults to https://github.com/<repo>)")
		branch   = fs.String("branch", "main", "branch")
		sha      = fs.String("sha", "", "commit SHA")
		message  = fs.String("message", "", "commit message")
		actor    = fs.String("actor", "", "who caused the event")
		payload  = fs.String("payload", "", "path to a JSON file with the original webhook payload")
		metadata = fs.String("metadata", "", "extra context, key=value,key=value")
		dryRun   = fs.Bool("dry-run", false, "print the request that would be sent, and send nothing")
		verbose  = fs.Bool("v", false, "log at debug level")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: workflow trigger <name> [flags]\n\nflags:\n")
		fs.PrintDefaults()
	}

	// The workflow name is taken off the front BEFORE the flags are parsed.
	//
	// This is not fussiness. Go's flag package stops parsing at the first
	// non-flag argument, so `trigger blog-generator --sha abc` parses zero flags
	// and silently ignores every one of them — it does not error, it just quietly
	// does the wrong thing, which is the worst failure a CLI can have. Taking the
	// name first means `trigger <name> --flags` works the way everyone expects.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fs.Usage()
		return fmt.Errorf("%w: which workflow?", errUsage)
	}
	name := args[0]

	if err := fs.Parse(args[1:]); err != nil {
		return errUsage
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q (flags go after the workflow name)", errUsage, fs.Arg(0))
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	svc, _, err := service(level)
	if err != nil {
		return err
	}

	req := workflow.Request{
		Workflow: name,
		Event: workflow.Event{
			ID:            *id,
			Type:          *event,
			Action:        *action,
			Repository:    *repo,
			RepositoryURL: *repoURL,
			Branch:        *branch,
			CommitSHA:     *sha,
			CommitMessage: *message,
			Actor:         *actor,
		},
		Metadata: parseMetadata(*metadata),
	}

	if req.Event.ID == "" {
		// A generated ID means a generated idempotency key, which means n8n cannot
		// tell this from a new event. That is fine for a smoke test and wrong for a
		// replay, so say so rather than let someone discover it.
		req.Event.ID = fmt.Sprintf("cli-%d", time.Now().UnixNano())
		fmt.Fprintf(os.Stderr,
			"note: no --id given, so this trigger gets a fresh idempotency key (%s).\n"+
				"      To REPLAY an event safely, pass its original --id.\n\n", req.Event.ID)
	}
	if req.Event.RepositoryURL == "" && req.Event.Repository != "" {
		req.Event.RepositoryURL = "https://github.com/" + req.Event.Repository
	}

	if *payload != "" {
		raw, err := os.ReadFile(*payload)
		if err != nil {
			return fmt.Errorf("%w: reading --payload: %v", workflow.ErrInvalidRequest, err)
		}
		req.Event.Payload = raw
	}

	if *dryRun {
		// Note that this prints the request as the CLI built it — the client will
		// still sanitise the payload before it goes out. That is deliberate: the
		// dry run shows you what you asked for, and the client shows you what it
		// actually sent.
		pretty, _ := json.MarshalIndent(req, "", "  ")
		fmt.Printf("%s\n", pretty)
		return nil
	}

	result, err := svc.Run(ctx, req)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s %s in %dms (attempt %d)\n",
		result.Workflow, result.Status, result.DurationMS, result.Attempts)
	if result.ExecutionID != "" {
		fmt.Printf("execution: %s\n", result.ExecutionID)
	}
	fmt.Printf("correlation: %s\n", result.CorrelationID)
	return nil
}

func parseMetadata(raw string) map[string]string {
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

func usage() {
	fmt.Fprint(os.Stderr, `workflow — trigger the platform's n8n workflows

  workflow list                     show the configured workflows and settings
  workflow trigger <name> [flags]   trigger one
  workflow trigger <name> -h        the flags

Configuration comes from the environment:

  N8N_BASE_URL     where n8n is        (required)
  N8N_TOKEN        the shared secret   (required, never printed)
  N8N_WORKFLOWS    name=/path,...      (required)
  N8N_TIMEOUT      per attempt         (default 10s)
  N8N_RETRY_ATTEMPTS / N8N_RETRY_DELAY (defaults 3 / 500ms)

See WORKFLOWS.md.
`)
}
