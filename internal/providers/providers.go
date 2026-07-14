// Package providers builds the configured llm.Provider.
//
// It exists so that switching the platform between a local model and a managed one is a
// **configuration change, not a code change**:
//
//	LLM_PROVIDER=ollama    # a model on hardware you own; the prompt does not leave
//	LLM_PROVIDER=bedrock   # a managed foundation model; the prompt leaves, and is billed
//
// # Why this is its own package
//
// It is the only package in the repository that imports two vendors, and that is
// deliberate. The architecture test in internal/architecture_test.go enforces that
// `llm` never imports `ollama` or `bedrock`, and that neither vendor imports the other —
// so the knowledge of *which providers exist* has to live somewhere, and it must be
// somewhere that nothing else depends on.
//
// That somewhere is here: a leaf. Callers depend on llm.Provider, an interface. This
// package is the one place that knows the list, and it is the one place that changes when
// a third provider (Claude, M9) is added.
//
// # What it is NOT
//
// It is not a router. It picks a provider from configuration, once, at start-up, and then
// gets out of the way. Choosing a provider *per request* — by cost, by latency, by whether
// this repository's source may leave the VPC — is Milestone 10, and it will implement
// llm.Provider itself and sit exactly where a single provider sits today. Nothing above
// will notice.
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

// DefaultProvider is Ollama.
//
// The default is the one where the prompt does not leave the network. A platform that
// defaults to sending somebody's source code to a hosted service, because nobody set an
// environment variable, has chosen badly on their behalf.
const DefaultProvider = "ollama"

// Known providers, for error messages and for anything that wants to enumerate them.
var Known = []string{"bedrock", "ollama"}

// Info describes the provider that was built, in terms no caller has to translate.
//
// It exists so that a CLI (or a health endpoint) can say "bedrock, anthropic.claude-…"
// without importing a vendor package to find out what a bedrock.Config looks like. The
// moment a caller has to type `bedrock.Config` to display a model name, the abstraction
// has sprung a leak.
type Info struct {
	// Provider is "ollama" or "bedrock".
	Provider string
	// Model is the configured default model, whatever that provider calls it.
	Model string
	// Endpoint is where the provider lives, in human terms — a URL for Ollama, a region
	// for Bedrock. It is for showing a person, never for building a request.
	Endpoint string
	// Stream reports whether this provider is configured to stream by default.
	Stream bool
	// Redacted is the configuration, safe to print. For Bedrock it contains no secret at
	// all — which is the whole difference between IAM and an API key, and worth showing.
	Redacted map[string]any
}

// New builds the configured provider.
func New(ctx context.Context, log *slog.Logger) (llm.Provider, Info, error) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv(EnvProvider)))
	if name == "" {
		name = DefaultProvider
	}

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
		sort.Strings(known)
		return nil, Info{}, fmt.Errorf("%w: unknown %s %q (known: %s)",
			ErrConfig, EnvProvider, name, strings.Join(known, ", "))
	}
}
