// Package providers builds the configured llm.Provider.
//
// It exists so that switching the platform between a local model and a managed one is a
// **configuration change, not a code change**:
//
//	LLM_PROVIDER=ollama    # a model on hardware you own; the prompt does not leave
//	LLM_PROVIDER=bedrock   # a managed foundation model; the prompt leaves, and is billed
//	LLM_PROVIDER=router    # BOTH — chosen per request (Milestone 10)
//
// # Why this is its own package
//
// It is the only package in the repository that imports two vendors, and that is
// deliberate. The architecture test in internal/architecture_test.go enforces that
// `llm` never imports `ollama` or `bedrock`, that neither vendor imports the other, and —
// since Milestone 10 — that the *router* does not import either. So the knowledge of
// *which providers exist* has to live somewhere, and it must be somewhere that nothing
// else depends on.
//
// That somewhere is here: a leaf. Callers depend on llm.Provider, an interface. This
// package is the one place that knows the list, and it is the one place that changes when
// a third provider is added.
//
// # Milestone 10 kept the promise this package made
//
// Milestone 7 wrote the following here, when there was exactly one provider and nothing to
// route:
//
//	It is not a router. It picks a provider from configuration, once, at start-up, and then
//	gets out of the way. Choosing a provider *per request* — by cost, by latency, by whether
//	this repository's source may leave the VPC — is Milestone 10, and it will implement
//	llm.Provider itself and sit exactly where a single provider sits today. Nothing above
//	will notice.
//
// That is exactly what happened, and it is worth being precise about how little it cost.
// [New] grew one `case`. The router is built from the same vendor constructors that were
// already here, handed to it as a `map[string]llm.Provider`, and returned as an
// `llm.Provider` — so every caller in the repository still receives one interface and still
// cannot tell how many models are behind it.
//
// **This package is still the only one that knows the catalogue.** internal/router does not:
// it routes between whatever map it is given, and it would route between five providers, or
// between two Bedrock models, without a line changing. Adding Amazon Nova, Mistral or OpenAI
// means writing an llm.Provider and adding a case to [New]. It does not mean touching the
// router, the service, the tool loop, or any caller.
package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/bedrock"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/ollama"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/router"
)

// ErrConfig means the configured provider could not be built from the environment.
//
// It exists so a caller can recognise a misconfiguration without importing a vendor to ask
// whether it was ollama.ErrConfig or bedrock.ErrConfig. Milestone 8 left exactly that leak
// in cmd/llm — it checked ollama.ErrConfig, and therefore silently gave the wrong exit
// code for every Bedrock misconfiguration. The vendors' own errors still wrap through, so
// nothing that already worked stops working.
var ErrConfig = errors.New("the configured llm provider could not be built")

// EnvProvider selects the provider.
const EnvProvider = "LLM_PROVIDER"

// Router is the value of LLM_PROVIDER that turns on hybrid routing.
//
// It is a value of the *same* variable rather than a separate LLM_ROUTING=true, and that is
// a deliberately small piece of API design: routing is not a mode the platform is additionally
// in, it is a choice of provider. The router IS an llm.Provider. Modelling it as a flag
// alongside the provider name would imply the two could disagree, and invite the question
// of what `LLM_PROVIDER=ollama LLM_ROUTING=true` is supposed to mean.
const Router = "router"

// DefaultProvider is Ollama.
//
// The default is the one where the prompt does not leave the network. A platform that
// defaults to sending somebody's source code to a hosted service, because nobody set an
// environment variable, has chosen badly on their behalf.
//
// Note that the default is NOT the router, even though the router is the interesting thing
// this milestone built. A platform that silently starts routing to a paid API because
// somebody left a variable unset is the same mistake in a better disguise: hybrid inference
// is opted into, not defaulted into.
const DefaultProvider = "ollama"

// Known providers, for error messages and for anything that wants to enumerate them.
//
// This is the catalogue, and it is the list the router is built from. It is the ONE place a
// new provider is registered.
var Known = []string{"bedrock", "ollama"}

// Info describes the provider that was built, in terms no caller has to translate.
//
// It exists so that a CLI (or a health endpoint) can say "bedrock, anthropic.claude-…"
// without importing a vendor package to find out what a bedrock.Config looks like. The
// moment a caller has to type `bedrock.Config` to display a model name, the abstraction
// has sprung a leak.
type Info struct {
	// Provider is "ollama", "bedrock", or "router".
	Provider string
	// Model is the configured default model, whatever that provider calls it. For a router
	// it is empty: the answer is "it depends on the request", which is the point of it.
	Model string
	// Endpoint is where the provider lives, in human terms — a URL for Ollama, a region
	// for Bedrock. It is for showing a person, never for building a request.
	Endpoint string
	// Stream reports whether this provider is configured to stream by default.
	Stream bool
	// Redacted is the configuration, safe to print. For Bedrock it contains no secret at
	// all — which is the whole difference between IAM and an API key, and worth showing.
	Redacted map[string]any

	// Routed reports that this is a router, and Members describes what is behind it — so a
	// CLI can print the whole fleet without learning that ollama.Config exists.
	Routed  bool
	Members []Info
}

// New builds the configured provider.
func New(ctx context.Context, log *slog.Logger) (llm.Provider, Info, error) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv(EnvProvider)))
	if name == "" {
		name = DefaultProvider
	}

	if name == Router {
		return newRouter(ctx, log)
	}

	provider, info, err := build(ctx, name, log)
	if err != nil {
		return nil, Info{}, err
	}
	return provider, info, nil
}

// build constructs ONE vendor. It is the catalogue, and adding a provider is adding a case.
func build(ctx context.Context, name string, log *slog.Logger) (llm.Provider, Info, error) {
	switch name {
	case "ollama":
		cfg, err := ollama.ConfigFromEnv()
		if err != nil {
			return nil, Info{}, fmt.Errorf("%w: %w", ErrConfig, err)
		}
		provider, err := ollama.New(cfg, log)
		if err != nil {
			return nil, Info{}, fmt.Errorf("%w: %w", ErrConfig, err)
		}
		return provider, Info{Provider: name, Model: cfg.Model, Endpoint: cfg.BaseURL, Stream: cfg.Stream, Redacted: cfg.Redacted()}, nil

	case "bedrock":
		cfg, err := bedrock.ConfigFromEnv()
		if err != nil {
			return nil, Info{}, fmt.Errorf("%w: %w", ErrConfig, err)
		}
		provider, err := bedrock.New(ctx, cfg, log)
		if err != nil {
			return nil, Info{}, fmt.Errorf("%w: %w", ErrConfig, err)
		}
		return provider, Info{Provider: name, Model: cfg.ModelID, Endpoint: "AWS " + cfg.Region, Stream: cfg.Stream, Redacted: cfg.Redacted()}, nil

	default:
		known := append([]string(nil), Known...)
		known = append(known, Router)
		sort.Strings(known)
		return nil, Info{}, fmt.Errorf("%w: unknown %s %q (known: %s)",
			ErrConfig, EnvProvider, name, strings.Join(known, ", "))
	}
}

// newRouter builds every enabled provider and puts a router in front of them.
//
// # Every enabled provider must build, or none of them do
//
// A router that quietly started with one of its two providers missing would be a router
// whose fallback does not exist, whose routing rules silently do not fire, and which reports
// itself as perfectly healthy — because from the inside, a fleet of one looks exactly like a
// fleet of one that was asked for.
//
// So a Bedrock misconfiguration is fatal to the whole router, even though Ollama built fine
// and the platform could technically run. That is the same principle every config loader in
// this repository follows and it is worth restating: finding out at start-up, while nobody is
// waiting, beats finding out on the first webhook of the day. "Degraded" is a state you enter
// because something broke, not a state you BOOT into because something was never configured.
func newRouter(ctx context.Context, log *slog.Logger) (llm.Provider, Info, error) {
	cfg, err := router.ConfigFromEnv(Known)
	if err != nil {
		return nil, Info{}, fmt.Errorf("%w: %w", ErrConfig, err)
	}

	built := make(map[string]llm.Provider, len(cfg.Providers))
	members := make([]Info, 0, len(cfg.Providers))

	for _, name := range cfg.Providers {
		provider, info, err := build(ctx, name, log)
		if err != nil {
			return nil, Info{}, fmt.Errorf("%w: the router enables %q, which did not build: %w",
				ErrConfig, name, err)
		}
		built[name] = provider
		members = append(members, info)
	}

	r, err := router.New(built, cfg, log)
	if err != nil {
		return nil, Info{}, fmt.Errorf("%w: %w", ErrConfig, err)
	}

	// Stream is true if ANY member streams — the same union rule the router applies to
	// capabilities, for the same reason.
	stream := false
	for _, m := range members {
		stream = stream || m.Stream
	}

	return r, Info{
		Provider: Router,
		Model:    "", // it depends on the request, which is the entire point
		Endpoint: strings.Join(cfg.Providers, " + "),
		Stream:   stream,
		Redacted: cfg.Redacted(),
		Routed:   true,
		Members:  members,
	}, nil
}
