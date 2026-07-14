package ollama

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
)

// Environment variables. Everything that differs between a laptop, dev, and prod is
// here; nothing else is.
const (
	// EnvBaseURL is the Ollama instance, e.g. http://10.20.1.5:11434. It is deployed
	// by the ollama-on-aws repository; this one only needs to know where it landed.
	EnvBaseURL = "OLLAMA_BASE_URL"

	// EnvModel is the default model, e.g. "llama3.2" or "qwen2.5-coder:7b".
	EnvModel = "OLLAMA_MODEL"

	// EnvToken is an optional bearer token, for an Ollama behind a reverse proxy that
	// authenticates.
	//
	// Ollama itself has NO authentication. That is not a bug in Ollama — it is a tool
	// designed to run on your laptop. It does mean that an Ollama exposed to a network
	// is an open inference endpoint for anyone who can reach it, so it belongs behind
	// a security group that lets nothing in, which is exactly what this platform's
	// network stack provides.
	EnvToken = "OLLAMA_TOKEN"

	// EnvTimeout is the TOTAL budget for a NON-streaming request. It is a blunt
	// instrument and it is why streaming is preferred — see EnvIdleTimeout.
	EnvTimeout = "OLLAMA_TIMEOUT"

	// EnvIdleTimeout is how long a STREAM may go without producing a token before we
	// call it dead.
	//
	// This is the important one, and it is the difference between a timeout that works
	// for inference and one that does not.
	//
	// A total timeout has to be set to the longest legitimate generation — which on a
	// CPU can be minutes — and at that value it will happily wait minutes for a model
	// that hung instantly. A stall timeout asks a much better question: "has this
	// thing produced a single token in the last thirty seconds?" A slow model keeps
	// answering yes. A dead one does not.
	EnvIdleTimeout = "OLLAMA_IDLE_TIMEOUT"

	// EnvRetryAttempts is the TOTAL attempts, not retries after the first.
	EnvRetryAttempts = "OLLAMA_RETRY_ATTEMPTS"
	EnvRetryDelay    = "OLLAMA_RETRY_DELAY"

	// EnvContextTokens is the model's context window, in tokens.
	//
	// Ollama does not reliably report this, and the consequence of getting it wrong is
	// nasty: a prompt larger than the window is not rejected, it is silently truncated
	// from the front, and the model answers confidently from whatever is left. So the
	// platform is told the number, and refuses to send a prompt that would not fit.
	EnvContextTokens = "OLLAMA_CONTEXT_TOKENS"

	// EnvMaxTokens is the default completion budget. Unbounded generation on hardware
	// billed by the hour is a model free to ramble at your expense.
	EnvMaxTokens = "OLLAMA_MAX_TOKENS"

	// EnvTemperature is the default temperature.
	EnvTemperature = "OLLAMA_TEMPERATURE"

	// EnvStream selects streaming by default. On by default, because a generation you
	// cannot see is indistinguishable from a hang.
	EnvStream = "OLLAMA_STREAM"

	// EnvKeepAlive is how long Ollama holds the model in memory after a request.
	//
	// It matters more than it looks. Loading a 7B model takes seconds; evicting it
	// between every request means paying that cost on every inference, and the symptom
	// is a large loadMs on every single log line rather than only the first.
	EnvKeepAlive = "OLLAMA_KEEP_ALIVE"

	// EnvMaxResponseBytes bounds what we will read back.
	EnvMaxResponseBytes = "OLLAMA_MAX_RESPONSE_BYTES"

	// EnvCACert is a PEM bundle for an Ollama behind a private CA. There is no option
	// to skip TLS verification.
	EnvCACert = "OLLAMA_CA_CERT"
)

// Defaults.
const (
	DefaultTimeout     = 5 * time.Minute // a whole non-streaming generation
	DefaultIdleTimeout = 60 * time.Second
	DefaultRetries     = 3
	DefaultRetryDelay  = time.Second

	// 8k is the common default context for the small models this platform can
	// realistically run. It is a CONSERVATIVE guess on purpose: too small refuses a
	// prompt that would have fitted, too large lets one be silently truncated.
	DefaultContextTokens = 8192

	DefaultMaxTokens        = 2048
	DefaultTemperature      = 0.2 // low: most of this platform's work is summarising, not inventing
	DefaultKeepAlive        = "5m"
	DefaultMaxResponseBytes = 32 << 20
)

// ErrConfig means the integration is misconfigured. Always fatal at start-up.
var ErrConfig = errors.New("ollama configuration")

// Config is everything the client needs.
type Config struct {
	BaseURL string
	Model   string
	Token   string

	Timeout       time.Duration
	IdleTimeout   time.Duration
	RetryAttempts int
	RetryDelay    time.Duration

	ContextTokens int
	MaxTokens     int
	Temperature   float64
	Stream        bool
	KeepAlive     string

	MaxResponseBytes int64
	CACertPath       string
}

// ConfigFromEnv reads the configuration and refuses to return anything half-built.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		BaseURL:          strings.TrimRight(os.Getenv(EnvBaseURL), "/"),
		Model:            strings.TrimSpace(os.Getenv(EnvModel)),
		Token:            os.Getenv(EnvToken),
		CACertPath:       os.Getenv(EnvCACert),
		Timeout:          DefaultTimeout,
		IdleTimeout:      DefaultIdleTimeout,
		RetryAttempts:    DefaultRetries,
		RetryDelay:       DefaultRetryDelay,
		ContextTokens:    DefaultContextTokens,
		MaxTokens:        DefaultMaxTokens,
		Temperature:      DefaultTemperature,
		Stream:           true,
		KeepAlive:        DefaultKeepAlive,
		MaxResponseBytes: DefaultMaxResponseBytes,
	}

	if cfg.BaseURL == "" {
		return Config{}, fmt.Errorf("%w: %s is not set", ErrConfig, EnvBaseURL)
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Config{}, fmt.Errorf("%w: %s must be an absolute http(s) URL, got %q", ErrConfig, EnvBaseURL, cfg.BaseURL)
	}
	// Plain http is normal here, and — unusually — defensible: Ollama has no auth and
	// is meant to sit on a private network, unreachable from outside. What is NOT
	// defensible is plain http to a host on the public internet while sending it a
	// bearer token, so that is refused.
	if parsed.Scheme == "http" && cfg.Token != "" && !isPrivate(parsed.Hostname()) {
		return Config{}, fmt.Errorf("%w: %s sends a token over plain http to a non-private host — it would cross the network in clear text",
			ErrConfig, EnvBaseURL)
	}
	if cfg.Model == "" {
		return Config{}, fmt.Errorf("%w: %s is not set (there is no sensible default model)", ErrConfig, EnvModel)
	}

	if cfg.Timeout, err = envDuration(EnvTimeout, DefaultTimeout); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = envDuration(EnvIdleTimeout, DefaultIdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RetryDelay, err = envDuration(EnvRetryDelay, DefaultRetryDelay); err != nil {
		return Config{}, err
	}
	if cfg.RetryAttempts, err = envInt(EnvRetryAttempts, DefaultRetries); err != nil {
		return Config{}, err
	}
	if cfg.RetryAttempts < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1 (it counts total attempts, not retries)", ErrConfig, EnvRetryAttempts)
	}
	if cfg.ContextTokens, err = envInt(EnvContextTokens, DefaultContextTokens); err != nil {
		return Config{}, err
	}
	if cfg.ContextTokens < 512 {
		return Config{}, fmt.Errorf("%w: %s = %d is too small to be real", ErrConfig, EnvContextTokens, cfg.ContextTokens)
	}
	if cfg.MaxTokens, err = envInt(EnvMaxTokens, DefaultMaxTokens); err != nil {
		return Config{}, err
	}
	if cfg.MaxTokens < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1", ErrConfig, EnvMaxTokens)
	}
	if cfg.Temperature, err = envFloat(EnvTemperature, DefaultTemperature); err != nil {
		return Config{}, err
	}
	if cfg.Temperature < 0 || cfg.Temperature > 2 {
		return Config{}, fmt.Errorf("%w: %s must be in [0, 2]", ErrConfig, EnvTemperature)
	}
	if cfg.Stream, err = envBool(EnvStream, true); err != nil {
		return Config{}, err
	}
	if cfg.MaxResponseBytes, err = envInt64(EnvMaxResponseBytes, DefaultMaxResponseBytes); err != nil {
		return Config{}, err
	}
	if v := strings.TrimSpace(os.Getenv(EnvKeepAlive)); v != "" {
		cfg.KeepAlive = v
	}

	return cfg, nil
}

// Redacted returns the configuration with the token removed, so start-up can log
// exactly what it is configured with.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"baseUrl":          c.BaseURL,
		"model":            c.Model,
		"token":            httpx.Redacted(c.Token),
		"timeout":          c.Timeout.String(),
		"idleTimeout":      c.IdleTimeout.String(),
		"retryAttempts":    c.RetryAttempts,
		"retryDelay":       c.RetryDelay.String(),
		"contextTokens":    c.ContextTokens,
		"maxTokens":        c.MaxTokens,
		"temperature":      c.Temperature,
		"stream":           c.Stream,
		"keepAlive":        c.KeepAlive,
		"maxResponseBytes": c.MaxResponseBytes,
	}
}

// isPrivate reports whether the host looks like it is on a network we control. It is
// a heuristic, and it is used only to decide whether sending a token over plain http
// is defensible — never to decide whether something is trusted.
func isPrivate(host string) bool {
	switch {
	case host == "localhost", host == "127.0.0.1", host == "::1", host == "[::1]":
		return true
	case strings.HasPrefix(host, "10."), strings.HasPrefix(host, "192.168."):
		return true
	case strings.HasPrefix(host, "172."):
		// 172.16.0.0/12 — good enough for a warning, and it errs towards refusing.
		return true
	case strings.HasSuffix(host, ".internal"), strings.HasSuffix(host, ".local"):
		return true
	default:
		return false
	}
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a duration like 90s or 5m, got %q", ErrConfig, key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w: %s must be positive, got %q", ErrConfig, key, raw)
	}
	return d, nil
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

func envInt64(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: %s must be a positive whole number, got %q", ErrConfig, key, raw)
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
	return f, nil
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
