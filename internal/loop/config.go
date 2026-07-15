package loop

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables. As everywhere on this platform, everything that differs between a
// laptop, dev and prod is here, and there is not a single loop bound compiled into the code.
//
// Note what is NOT here: which model reasons, or which runtime executes. Those are the
// inference plane's configuration (LLM_PROVIDER, and the router's LLM_ROUTER_*) and the agent
// runtime's (OPENCLAW_*). The loop is provider- and runtime-agnostic, so its configuration
// says nothing about either — it configures the SHAPE of the loop, and the adapters bring the
// providers.
const (
	// EnvMaxIterations is the hard cap on execution attempts across the whole loop. It is the
	// backstop that guarantees termination, and it is the one bound that should never be
	// disabled in production.
	EnvMaxIterations = "LOOP_MAX_ITERATIONS"

	// EnvMaxRetries is how many additional attempts a single failed task gets.
	EnvMaxRetries = "LOOP_MAX_RETRIES"

	// EnvMaxReplans caps how many times the plan may be rebuilt — the outer loop's budget.
	EnvMaxReplans = "LOOP_MAX_REPLANS"

	// EnvRetryDelay is the base backoff before a retry.
	EnvRetryDelay = "LOOP_RETRY_DELAY"

	// EnvMaxRetryDelay caps the exponential backoff.
	EnvMaxRetryDelay = "LOOP_MAX_RETRY_DELAY"

	// EnvBackoffMultiplier is the exponential factor. 1.0 is a fixed delay.
	EnvBackoffMultiplier = "LOOP_BACKOFF_MULTIPLIER"

	// EnvTimeout bounds the whole loop's wall clock. It is NOT a task's timeout — the runtime
	// enforces that on its own execution — this one bounds the loop end to end.
	EnvTimeout = "LOOP_TIMEOUT"

	// EnvMaxCostUSD is the cost cap. Zero disables it, which is a choice a platform spending
	// real money on agent runs should make deliberately and probably not at all.
	EnvMaxCostUSD = "LOOP_MAX_COST_USD"

	// EnvReflection turns reflection on or off. On by default: a loop that cannot learn from a
	// failure is a loop that retries the same failing approach until its budget is gone.
	EnvReflection = "LOOP_REFLECTION"

	// EnvMinConfidence is the evaluation threshold. An evaluator "success" below this
	// confidence is treated as a failure worth another look, rather than waved through —
	// because a low-confidence pass is where a wrong-but-plausible result reaches a pull
	// request. Zero disables the gate (any success is accepted).
	EnvMinConfidence = "LOOP_MIN_CONFIDENCE"
)

// Defaults. Deliberately conservative: a loop is a machine for spending money autonomously,
// and the safe default for every bound is "small", with an operator raising it once they
// trust a given objective.
const (
	DefaultMaxIterations     = 12
	DefaultMaxRetries        = 2
	DefaultMaxReplans        = 2
	DefaultRetryDelay        = 2 * time.Second
	DefaultMaxRetryDelay     = 30 * time.Second
	DefaultBackoffMultiplier = 2.0
	DefaultTimeout           = 30 * time.Minute
	DefaultMaxCostUSD        = 5.0
	DefaultReflection        = true
	DefaultMinConfidence     = 0.0
)

// ErrConfig means the loop is misconfigured. Always fatal at start-up: a loop whose bounds do
// not parse is not something to discover on the first goal of the day.
var ErrConfig = errors.New("loop configuration")

// Config is the shape of the loop: its bounds, its retry policy, and whether it reflects. It
// holds no endpoint, no credential, and no provider — a loop calls nothing directly.
type Config struct {
	MaxIterations int
	MaxReplans    int
	Timeout       time.Duration
	MaxCostUSD    float64

	Retry RetryPolicy

	// Reflection enables the reflect stage. When false, a retryable failure goes straight back
	// to execution with the same instructions; when true, the reflector gets a say first.
	Reflection bool

	// MinConfidence is the evaluation threshold applied to a claimed success.
	MinConfidence float64
}

// ConfigFromEnv reads the configuration and refuses to return anything half-built.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		MaxIterations: DefaultMaxIterations,
		MaxReplans:    DefaultMaxReplans,
		Timeout:       DefaultTimeout,
		MaxCostUSD:    DefaultMaxCostUSD,
		Reflection:    DefaultReflection,
		MinConfidence: DefaultMinConfidence,
		Retry: RetryPolicy{
			MaxRetries: DefaultMaxRetries,
			BaseDelay:  DefaultRetryDelay,
			MaxDelay:   DefaultMaxRetryDelay,
			Multiplier: DefaultBackoffMultiplier,
		},
	}

	var err error
	if cfg.MaxIterations, err = envInt(EnvMaxIterations, DefaultMaxIterations); err != nil {
		return Config{}, err
	}
	if cfg.MaxIterations < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1 — a loop that cannot execute cannot "+
			"achieve anything", ErrConfig, EnvMaxIterations)
	}
	if cfg.Retry.MaxRetries, err = envInt(EnvMaxRetries, DefaultMaxRetries); err != nil {
		return Config{}, err
	}
	if cfg.Retry.MaxRetries < 0 {
		return Config{}, fmt.Errorf("%w: %s cannot be negative", ErrConfig, EnvMaxRetries)
	}
	if cfg.MaxReplans, err = envInt(EnvMaxReplans, DefaultMaxReplans); err != nil {
		return Config{}, err
	}
	if cfg.MaxReplans < 0 {
		return Config{}, fmt.Errorf("%w: %s cannot be negative", ErrConfig, EnvMaxReplans)
	}
	if cfg.Retry.BaseDelay, err = envDuration(EnvRetryDelay, DefaultRetryDelay); err != nil {
		return Config{}, err
	}
	if cfg.Retry.MaxDelay, err = envDuration(EnvMaxRetryDelay, DefaultMaxRetryDelay); err != nil {
		return Config{}, err
	}
	if cfg.Retry.Multiplier, err = envFloat(EnvBackoffMultiplier, DefaultBackoffMultiplier); err != nil {
		return Config{}, err
	}
	if cfg.Retry.Multiplier < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1.0 (1.0 is a fixed delay; less would "+
			"shrink the wait each retry, which is backwards)", ErrConfig, EnvBackoffMultiplier)
	}
	if cfg.Timeout, err = envDuration(EnvTimeout, DefaultTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxCostUSD, err = envFloat(EnvMaxCostUSD, DefaultMaxCostUSD); err != nil {
		return Config{}, err
	}
	if cfg.Reflection, err = envBool(EnvReflection, DefaultReflection); err != nil {
		return Config{}, err
	}
	if cfg.MinConfidence, err = envFloat(EnvMinConfidence, DefaultMinConfidence); err != nil {
		return Config{}, err
	}
	if cfg.MinConfidence < 0 || cfg.MinConfidence > 1 {
		return Config{}, fmt.Errorf("%w: %s must be in [0, 1]", ErrConfig, EnvMinConfidence)
	}

	return cfg, nil
}

// Redacted returns the configuration for logging. There is nothing secret in it — a loop
// holds no credentials — which is worth showing rather than merely being true.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"maxIterations":     c.MaxIterations,
		"maxRetries":        c.Retry.MaxRetries,
		"maxReplans":        c.MaxReplans,
		"retryDelay":        c.Retry.BaseDelay.String(),
		"maxRetryDelay":     c.Retry.MaxDelay.String(),
		"backoffMultiplier": c.Retry.Multiplier,
		"timeout":           c.Timeout.String(),
		"maxCostUsd":        c.MaxCostUSD,
		"reflection":        c.Reflection,
		"minConfidence":     c.MinConfidence,
		"credentials":       "(none — a loop calls nothing; the adapters hold the providers)",
	}
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

func envFloat(key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a number, got %q", ErrConfig, key, raw)
	}
	if f < 0 {
		return 0, fmt.Errorf("%w: %s must not be negative, got %q", ErrConfig, key, raw)
	}
	return f, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a duration like 30s or 5m, got %q", ErrConfig, key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w: %s must be positive, got %q", ErrConfig, key, raw)
	}
	return d, nil
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
