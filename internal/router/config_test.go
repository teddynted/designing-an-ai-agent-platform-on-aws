package router

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// known is the catalogue the factory would pass in.
var known = []string{"bedrock", "ollama"}

func TestConfigDefaults(t *testing.T) {
	// A router configured with nothing but its providers is the boring, predictable one: a
	// fixed strategy, the first provider as default, fallback on.
	t.Setenv(EnvProviders, "ollama,bedrock")

	cfg, err := ConfigFromEnv(known)
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Strategy != StrategyFixed {
		t.Errorf("strategy = %q, want the predictable default %q", cfg.Strategy, StrategyFixed)
	}
	if cfg.Default != "ollama" {
		t.Errorf("default = %q, want the first listed provider so preference is one variable", cfg.Default)
	}
	if !cfg.Fallback {
		t.Error("fallback should default ON — the whole point of two providers is that they do " +
			"not fail together")
	}
}

// An unset provider list means "route between everything I have", not an error. It is a
// deliberate default: LLM_PROVIDER=router with no further configuration should do the
// obvious thing rather than refuse to boot.
func TestAnUnsetProviderListDefaultsToTheWholeCatalogue(t *testing.T) {
	t.Setenv(EnvProviders, "")

	cfg, err := ConfigFromEnv(known)
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if len(cfg.Providers) != len(known) {
		t.Errorf("providers = %v, want the whole catalogue %v", cfg.Providers, known)
	}
}

func TestConfigRejects(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string // a substring the error must contain, so the message stays actionable
	}{
		{
			name: "providers that parse to nothing",
			env:  map[string]string{EnvProviders: " , , "},
			want: "empty",
		},
		{
			name: "a provider that does not exist",
			env:  map[string]string{EnvProviders: "ollama,gpt-9"},
			want: "gpt-9",
		},
		{
			name: "a provider listed twice — the seed of an infinite fallback loop",
			env:  map[string]string{EnvProviders: "ollama,ollama"},
			want: "twice",
		},
		{
			name: "a default that is not enabled",
			env:  map[string]string{EnvProviders: "ollama", EnvDefault: "bedrock"},
			want: "not enabled",
		},
		{
			name: "an unknown strategy",
			env:  map[string]string{EnvProviders: "ollama", EnvStrategy: "round-robin"},
			want: "round-robin",
		},
		{
			name: "purpose strategy with no rules is a fixed strategy in a costume",
			env:  map[string]string{EnvProviders: "ollama,bedrock", EnvStrategy: "purpose"},
			want: EnvRules,
		},
		{
			name: "a rule pointing at a provider that is not enabled",
			env: map[string]string{
				EnvProviders: "ollama", EnvStrategy: "purpose", EnvRules: "release-notes=bedrock",
			},
			want: "not enabled",
		},
		{
			name: "a malformed rule",
			env: map[string]string{
				EnvProviders: "ollama", EnvStrategy: "purpose", EnvRules: "release-notes",
			},
			want: "purpose=provider",
		},
		{
			name: "a health threshold of zero would condemn a provider before it failed",
			env:  map[string]string{EnvProviders: "ollama", EnvHealthThreshold: "0"},
			want: "at least 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := ConfigFromEnv(known)
			if err == nil {
				t.Fatal("want a start-up error — a bad routing table must not boot")
			}
			if !errors.Is(err, ErrConfig) {
				t.Errorf("err = %v, want it to wrap ErrConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q so an operator can fix it", err, tc.want)
			}
		})
	}
}

func TestConfigParsesRules(t *testing.T) {
	t.Setenv(EnvProviders, "ollama,bedrock")
	t.Setenv(EnvStrategy, "purpose")
	t.Setenv(EnvRules, " release-notes = bedrock , diff-summary=ollama ")

	cfg, err := ConfigFromEnv(known)
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Rules[llm.Purpose("release-notes")] != "bedrock" {
		t.Errorf("release-notes → %q, want bedrock (and whitespace tolerated)", cfg.Rules["release-notes"])
	}
	if cfg.Rules[llm.Purpose("diff-summary")] != "ollama" {
		t.Errorf("diff-summary → %q, want ollama", cfg.Rules["diff-summary"])
	}
}

func TestConfigParsesKnobs(t *testing.T) {
	t.Setenv(EnvProviders, "ollama,bedrock")
	t.Setenv(EnvFallback, "false")
	t.Setenv(EnvHealthThreshold, "5")
	t.Setenv(EnvHealthCooldown, "45s")

	cfg, err := ConfigFromEnv(known)
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Fallback {
		t.Error("fallback should be off")
	}
	if cfg.HealthThreshold != 5 {
		t.Errorf("threshold = %d, want 5", cfg.HealthThreshold)
	}
	if cfg.HealthCooldown != 45*time.Second {
		t.Errorf("cooldown = %v, want 45s", cfg.HealthCooldown)
	}
}

// The routing table holds no secret, because a router calls nothing. That is worth showing
// in the log rather than merely being true.
func TestRedactedSaysThereIsNothingToRedact(t *testing.T) {
	t.Setenv(EnvProviders, "ollama,bedrock")
	cfg, err := ConfigFromEnv(known)
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	creds, _ := cfg.Redacted()["credentials"].(string)
	if !strings.Contains(creds, "none") {
		t.Errorf("credentials = %q, want it to say there are none — each provider holds its own", creds)
	}
}
