package router

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// Environment variables.
//
// Note what is NOT here: an endpoint, a region, a model ID, a timeout, a retry count. Those
// are the *providers'* configuration and they stay there (OLLAMA_*, BEDROCK_*), because a
// router that had to be told Bedrock's region would be a router that knew Bedrock exists.
// Everything below is about CHOOSING, and nothing below is about calling.
const (
	// EnvProviders is the enabled providers, in preference order:
	//
	//	LLM_ROUTER_PROVIDERS=ollama,bedrock
	//
	// This is the strongest privacy control the platform has, and it is worth
	// understanding why. [llm.Request.RequireLocal] refuses a hosted provider per request,
	// and a routing strategy can prefer a local one — but *this* list decides which
	// providers are built at all. A platform started with `LLM_ROUTER_PROVIDERS=ollama`
	// cannot send a prompt to Bedrock by mistake, by misconfiguration, or by a bug in a
	// strategy, because there is no Bedrock client in the process to send it with.
	//
	// A guarantee enforced by absence beats a guarantee enforced by an `if`.
	EnvProviders = "LLM_ROUTER_PROVIDERS"

	// EnvStrategy is how a provider is chosen: "fixed" or "purpose". See [Strategy].
	EnvStrategy = "LLM_ROUTER_STRATEGY"

	// EnvDefault is where requests land when the strategy has no better idea. It must name
	// one of EnvProviders.
	EnvDefault = "LLM_ROUTER_DEFAULT"

	// EnvRules maps a purpose to a provider, for the "purpose" strategy:
	//
	//	LLM_ROUTER_RULES=release-notes=bedrock,diff-summary=ollama
	//
	// The shape deliberately matches OPENCLAW_AGENTS (Milestone 6), which maps a task type
	// to an agent. It is the same idea — route this KIND of work to that THING — and using
	// the same syntax for it means one fewer format for an operator to remember.
	EnvRules = "LLM_ROUTER_RULES"

	// EnvFallback enables failing over to another provider when the chosen one is down.
	//
	// It is on by default, and that default is a real choice: the entire argument for
	// running a local model *and* a hosted one is that they do not fail at the same time
	// or for the same reasons. A GPU instance is reclaimed (it is Spot — see
	// infra/SPOT.md); Bedrock throttles you at exactly the moment you are busiest. Neither
	// event is correlated with the other, and refusing to use that fact is leaving the
	// availability on the table.
	//
	// Turn it OFF when you would rather have an error than a surprise — see
	// [Config.Fallback].
	EnvFallback = "LLM_ROUTER_FALLBACK"

	// EnvHealthThreshold is how many consecutive provider-level failures take a provider
	// out of rotation. Zero would mean "one bad request condemns it", which on a busy
	// platform is a single blip taking out the primary.
	EnvHealthThreshold = "LLM_ROUTER_HEALTH_THRESHOLD"

	// EnvHealthCooldown is how long a provider stays out before it is tried again.
	//
	// See [Health] for why this exists at all, which is not obvious: without it, fallback
	// *works* — and costs the primary's full timeout on every single request while it is
	// down. Bedrock's default timeout is two minutes.
	EnvHealthCooldown = "LLM_ROUTER_HEALTH_COOLDOWN"
)

// Defaults.
const (
	// DefaultStrategy is "fixed" — one provider, named by EnvDefault, for everything.
	//
	// The boring one is the default on purpose. A platform whose routing rules are
	// interesting on day one is a platform where nobody can say why a given request went
	// where it went, and the first thing you want from a router is that it be predictable.
	DefaultStrategy = StrategyFixed

	DefaultFallback        = true
	DefaultHealthThreshold = 2
	DefaultHealthCooldown  = 30 * time.Second
)

// ErrConfig means the router is misconfigured. Always fatal at start-up: a routing table
// that names a provider which does not exist is not something to discover on the first
// webhook of the day.
var ErrConfig = errors.New("router configuration")

// Config is the routing table. It says nothing about how to CALL a provider.
type Config struct {
	// Providers is the enabled providers, in preference order. Never empty.
	Providers []string

	// Strategy is how one of them is chosen per request.
	Strategy string

	// Default is where a request lands when the strategy has no opinion.
	Default string

	// Rules is purpose → provider, for StrategyPurpose.
	Rules map[llm.Purpose]string

	// Fallback allows failing over to a second provider when the first is unavailable.
	//
	// It does NOT allow failing over out of a pinned request ([llm.Request.Provider]), out
	// of a constraint ([llm.Request.RequireLocal]), out of a stream that has already
	// emitted a token, or out of a conversation in which a tool has already run. Those are
	// not exceptions to this setting; they are cases where a second attempt would be a
	// different kind of wrong, and no configuration may turn them on.
	Fallback bool

	HealthThreshold int
	HealthCooldown  time.Duration
}

// ConfigFromEnv reads the routing table and refuses to return anything half-built.
func ConfigFromEnv(known []string) (Config, error) {
	cfg := Config{
		Providers:       known,
		Strategy:        DefaultStrategy,
		Fallback:        DefaultFallback,
		HealthThreshold: DefaultHealthThreshold,
		HealthCooldown:  DefaultHealthCooldown,
		Rules:           map[llm.Purpose]string{},
	}

	if raw := strings.TrimSpace(os.Getenv(EnvProviders)); raw != "" {
		cfg.Providers = split(raw)
	}
	if len(cfg.Providers) == 0 {
		return Config{}, fmt.Errorf("%w: %s is empty — a router with no providers cannot "+
			"do anything at all", ErrConfig, EnvProviders)
	}
	for _, name := range cfg.Providers {
		if !contains(known, name) {
			return Config{}, fmt.Errorf("%w: %s names %q, which is not a provider this platform "+
				"has (known: %s)", ErrConfig, EnvProviders, name, strings.Join(sorted(known), ", "))
		}
	}
	if dupe := duplicate(cfg.Providers); dupe != "" {
		// Not pedantry. The fallback chain is built from this list, and a provider listed
		// twice is a provider that gets tried twice — which is the beginning of the infinite
		// fallback loop the whole design exists to make impossible.
		return Config{}, fmt.Errorf("%w: %s lists %q twice — a provider may appear once, or the "+
			"fallback chain would try it twice", ErrConfig, EnvProviders, dupe)
	}

	// The default provider defaults to the first one listed, so that the common case —
	// "prefer the local model, use Bedrock when you must" — is expressed by the order of
	// one variable rather than by two.
	cfg.Default = strings.ToLower(strings.TrimSpace(os.Getenv(EnvDefault)))
	if cfg.Default == "" {
		cfg.Default = cfg.Providers[0]
	}
	if !contains(cfg.Providers, cfg.Default) {
		return Config{}, fmt.Errorf("%w: %s is %q, which is not enabled (%s = %s)",
			ErrConfig, EnvDefault, cfg.Default, EnvProviders, strings.Join(cfg.Providers, ", "))
	}

	if raw := strings.TrimSpace(os.Getenv(EnvStrategy)); raw != "" {
		cfg.Strategy = strings.ToLower(raw)
	}
	if cfg.Strategy != StrategyFixed && cfg.Strategy != StrategyPurpose {
		return Config{}, fmt.Errorf("%w: %s is %q (known: %s, %s)",
			ErrConfig, EnvStrategy, cfg.Strategy, StrategyFixed, StrategyPurpose)
	}

	rules, err := parseRules(os.Getenv(EnvRules), cfg.Providers)
	if err != nil {
		return Config{}, err
	}
	cfg.Rules = rules

	if cfg.Strategy == StrategyPurpose && len(cfg.Rules) == 0 {
		// A purpose strategy with no rules is a fixed strategy wearing a costume, and it
		// would send every request to the default while an operator sat looking at
		// LLM_ROUTER_STRATEGY=purpose wondering why nothing was being routed.
		return Config{}, fmt.Errorf("%w: %s is %q but %s is empty, so every request would go to "+
			"%q anyway — set some rules, or use %s=%s",
			ErrConfig, EnvStrategy, StrategyPurpose, EnvRules, cfg.Default, EnvStrategy, StrategyFixed)
	}

	if cfg.Fallback, err = envBool(EnvFallback, DefaultFallback); err != nil {
		return Config{}, err
	}
	if cfg.HealthThreshold, err = envInt(EnvHealthThreshold, DefaultHealthThreshold); err != nil {
		return Config{}, err
	}
	if cfg.HealthThreshold < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1 — a threshold of 0 would condemn a "+
			"provider before it had failed at all", ErrConfig, EnvHealthThreshold)
	}
	if cfg.HealthCooldown, err = envDuration(EnvHealthCooldown, DefaultHealthCooldown); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// parseRules reads "release-notes=bedrock,diff-summary=ollama".
//
// Every provider named must be ENABLED. A rule that points at a provider which is not
// running is a rule that does nothing, silently, and the request it was meant to route goes
// to the default instead — producing the single most confusing bug this package can have:
// routing that is configured, plausible, and simply not in effect.
func parseRules(raw string, enabled []string) (map[llm.Purpose]string, error) {
	rules := map[llm.Purpose]string{}

	for _, pair := range split(raw) {
		key, provider, ok := strings.Cut(pair, "=")
		key, provider = strings.TrimSpace(key), strings.ToLower(strings.TrimSpace(provider))

		if !ok || key == "" || provider == "" {
			return nil, fmt.Errorf("%w: %s entry %q is not purpose=provider", ErrConfig, EnvRules, pair)
		}
		if !contains(enabled, provider) {
			return nil, fmt.Errorf("%w: %s routes %q to %q, which is not enabled (%s = %s). "+
				"The rule would never fire and the request would quietly go somewhere else",
				ErrConfig, EnvRules, key, provider, EnvProviders, strings.Join(enabled, ", "))
		}
		if existing, dupe := rules[llm.Purpose(key)]; dupe {
			return nil, fmt.Errorf("%w: %s routes %q to both %q and %q", ErrConfig, EnvRules,
				key, existing, provider)
		}
		rules[llm.Purpose(key)] = provider
	}
	return rules, nil
}

// Redacted returns the routing table for logging. There is nothing secret in it — a router
// holds no credentials, because it never calls anything: the providers do that, with their
// own configuration and their own IAM. That is worth showing rather than merely claiming.
func (c Config) Redacted() map[string]any {
	rules := map[string]string{}
	for purpose, provider := range c.Rules {
		rules[string(purpose)] = provider
	}
	return map[string]any{
		"providers":       c.Providers,
		"strategy":        c.Strategy,
		"default":         c.Default,
		"rules":           orNoRules(rules),
		"fallback":        c.Fallback,
		"healthThreshold": c.HealthThreshold,
		"healthCooldown":  c.HealthCooldown.String(),
		"credentials":     "(none — a router calls nothing; each provider holds its own)",
	}
}

func orNoRules(rules map[string]string) any {
	if len(rules) == 0 {
		return "(none — every request goes to the default)"
	}
	return rules
}

func split(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func duplicate(names []string) string {
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			return name
		}
		seen[name] = true
	}
	return ""
}

func sorted(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%w: %s must be true or false, got %q", ErrConfig, key, raw)
	}
	return b, nil
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a whole number, got %q", ErrConfig, key, raw)
	}
	return n, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a duration like 30s or 2m, got %q", ErrConfig, key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w: %s must be positive, got %q", ErrConfig, key, raw)
	}
	return d, nil
}
