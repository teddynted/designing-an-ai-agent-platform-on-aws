// Command observe exercises the platform's observability layer against the real
// world, the same way cmd/workflow and cmd/agent exercise their integrations.
//
// It exists for three reasons, and none of them is "a demo":
//
//   - To SEE what lands in CloudWatch. `observe emit` prints exactly the structured
//     log line and the EMF metric line the platform would ship, so "what does a
//     workflow-completed metric look like?" is a command with an answer, not a
//     guess about a format.
//   - To PROBE a dependency by hand. `observe health` runs the same readiness
//     checks the platform's health endpoint runs — is OpenClaw reachable, is n8n
//     up — and prints the JSON body, which is what you want at 3am when the
//     dashboard says "unhealthy" and you need to know which dependency and how
//     slow.
//   - To BE the health endpoint. `observe serve` runs the liveness/readiness HTTP
//     server, so an EC2 workload or a container has a /healthz and /readyz to point
//     a load balancer at without embedding the server in every binary.
//
// Configuration comes from the environment (see internal/observability and the
// flags below). Nothing is hard-coded and no secret is ever printed — the logger's
// redaction sees to that even here.
//
// Usage:
//
//	observe emit   --metric WorkflowDurationMs=1500 --dim Workflow=blog-generator
//	observe health --target openclaw=http://localhost:8088 --target n8n=https://n8n.internal
//	observe serve  --addr :8080 --target openclaw=http://localhost:8088
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/observability"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("which command?")
	}

	// Ctrl-C cancels an in-flight probe and shuts the server down cleanly, rather
	// than orphaning either.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "emit":
		return emit(ctx, args[1:])
	case "health":
		return health(ctx, args[1:])
	case "serve":
		return serve(ctx, args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// emit prints a structured log line and, if a namespace is configured, the EMF
// metric line that would carry the same event into CloudWatch — so you can read
// both with your own eyes before trusting a dashboard to.
func emit(ctx context.Context, args []string) error {
	var (
		message string
		metrics []kv
		dims    = observability.Dimensions{}
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--metric", "-m":
			i++
			if i >= len(args) {
				return fmt.Errorf("--metric needs Name=Value")
			}
			k, v, ok := strings.Cut(args[i], "=")
			if !ok {
				return fmt.Errorf("--metric wants Name=Value, got %q", args[i])
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("--metric %q value is not a number: %v", k, err)
			}
			metrics = append(metrics, kv{k, f})
		case "--dim", "-d":
			i++
			if i >= len(args) {
				return fmt.Errorf("--dim needs Name=Value")
			}
			k, v, ok := strings.Cut(args[i], "=")
			if !ok {
				return fmt.Errorf("--dim wants Name=Value, got %q", args[i])
			}
			dims[k] = v
		case "--message":
			i++
			if i < len(args) {
				message = args[i]
			}
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	if message == "" {
		message = "observe emit sample"
	}

	cfg := observability.ConfigFromEnv()
	if cfg.Service == "" {
		cfg.Service = "observe"
	}
	// Default a namespace so the sample actually shows an EMF line even on a laptop
	// with nothing configured — the whole point of `emit` is to SEE one.
	if cfg.MetricsNamespace == "" {
		cfg.MetricsNamespace = "aiap/app"
	}

	log := observability.New(cfg)
	// A representative correlation context, so the sample line shows the standard
	// fields the way a real one would.
	ctx = observability.WithFields(ctx, observability.Fields{
		Component:     "cli",
		CorrelationID: "cli:" + strconv.FormatInt(time.Now().Unix(), 10),
	})

	log.InfoContext(ctx, message, "sample", true)

	if len(metrics) == 0 {
		// A sane default set that mirrors a real "workflow completed" line.
		metrics = []kv{{"WorkflowDurationMs", 1500}, {"WorkflowSuccess", 1}}
		if _, ok := dims["Workflow"]; !ok {
			dims["Workflow"] = "blog-generator"
		}
	}

	m := observability.NewEmitter(cfg).New(dims)
	for _, kv := range metrics {
		// Names ending in "Ms" are durations; everything else a count. A small
		// convenience so the sample reads naturally.
		if strings.HasSuffix(kv.k, "Ms") {
			m.Put(kv.k, kv.v, observability.UnitMilliseconds)
		} else {
			m.Count(kv.k, kv.v)
		}
	}
	m.Emit(ctx, message)
	return nil
}

// health runs the readiness probes against the configured targets and prints the
// report. It exits non-zero when anything is down, so a shell or a CI step can gate
// on it.
func health(ctx context.Context, args []string) error {
	targets, addr, err := parseServeFlags(args)
	if err != nil {
		return err
	}
	_ = addr

	h := observability.NewHealth()
	for _, t := range targets {
		h.AddReadiness(observability.HTTPCheck{CheckName: t.name, URL: t.url, Path: t.path})
	}

	rep := h.Ready(ctx)
	printReport(rep)
	if rep.Status != observability.StatusUp {
		return fmt.Errorf("readiness is %s", rep.Status)
	}
	return nil
}

// serve runs the health HTTP server until interrupted.
func serve(ctx context.Context, args []string) error {
	targets, addr, err := parseServeFlags(args)
	if err != nil {
		return err
	}
	if addr == "" {
		addr = ":8080"
	}

	h := observability.NewHealth()
	for _, t := range targets {
		h.AddReadiness(observability.HTTPCheck{CheckName: t.name, URL: t.url, Path: t.path})
	}

	cfg := observability.ConfigFromEnv()
	if cfg.Service == "" {
		cfg.Service = "observe"
	}
	log := observability.New(cfg)

	srv := &http.Server{Addr: addr, Handler: h.Handler()}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.InfoContext(ctx, "health server listening", "addr", addr, "targets", len(targets))
	fmt.Fprintf(os.Stderr, "health server on %s — GET /healthz (liveness), /readyz (readiness)\n", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

type target struct{ name, url, path string }
type kv struct {
	k string
	v float64
}

// parseServeFlags reads the shared --target and --addr flags. A target is
// name=url[,path], e.g. openclaw=http://localhost:8088 or n8n=https://n8n.io,/healthz.
func parseServeFlags(args []string) (targets []target, addr string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--target", "-t":
			i++
			if i >= len(args) {
				return nil, "", fmt.Errorf("--target needs name=url")
			}
			name, rest, ok := strings.Cut(args[i], "=")
			if !ok {
				return nil, "", fmt.Errorf("--target wants name=url, got %q", args[i])
			}
			url, path, _ := strings.Cut(rest, ",")
			targets = append(targets, target{name: name, url: url, path: path})
		case "--addr", "-a":
			i++
			if i < len(args) {
				addr = args[i]
			}
		default:
			return nil, "", fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	return targets, addr, nil
}

func printReport(rep observability.Report) {
	fmt.Printf("status: %s (uptime %ds)\n", rep.Status, rep.UptimeSec)
	for _, c := range rep.Checks {
		line := fmt.Sprintf("  %-12s %-4s %4dms", c.Name, c.Status, c.LatencyMS)
		if c.Error != "" {
			line += "  " + c.Error
		}
		fmt.Println(line)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `observe — exercise the platform's observability layer

  observe emit    [--metric Name=Value ...] [--dim Name=Value ...] [--message m]
                  print the structured log line and EMF metric line the platform ships

  observe health  --target name=url[,path] [--target ...]
                  run the readiness probes and print the report (exit != 0 if down)

  observe serve   [--addr :8080] --target name=url[,path] [--target ...]
                  run the /healthz + /readyz HTTP server

Configuration (see internal/observability):

  OBS_SERVICE            process name, stamped on every line
  OBS_LOG_LEVEL          debug | info | warn | error   (default info)
  OBS_LOG_FORMAT         json | text                   (default json)
  OBS_METRICS_NAMESPACE  CloudWatch namespace for EMF   (e.g. aiap/app)

See OBSERVABILITY.md.
`)
}
