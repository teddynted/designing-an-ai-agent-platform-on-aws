package openclaw

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/agent"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
)

// Environment variables. Everything that differs between a laptop, dev, and prod is
// here; nothing else is. There is not a single OpenClaw URL, token, or agent name
// compiled into this repository.
const (
	// EnvBaseURL is the OpenClaw instance. It is deployed and owned by the
	// openclaw-on-aws repository; this one only needs to know where it ended up.
	EnvBaseURL = "OPENCLAW_BASE_URL"

	// EnvToken authenticates us to it. Never logged, never in an error.
	EnvToken = "OPENCLAW_TOKEN"

	// EnvAuthHeader is the header the token goes in.
	EnvAuthHeader = "OPENCLAW_AUTH_HEADER"

	// EnvAgents maps task types to the agent that performs them:
	//
	//   OPENCLAW_AGENTS=blog-draft=writer,repo-analysis=analyst,release-notes=writer
	//
	// This is the seam that makes a new agent type a configuration change rather
	// than a code change, and it is what "supports future multi-agent workflows"
	// actually means in practice: two task types can map to different agents, or to
	// the same one, and the platform never learns which.
	EnvAgents = "OPENCLAW_AGENTS"

	// EnvDefaultAgent handles a task type with no explicit mapping. Empty means
	// there is no default, and an unmapped task is an error — which is the safer
	// choice: silently sending "release-notes" to whichever agent happens to be the
	// default is how you get a blog post where you wanted release notes.
	EnvDefaultAgent = "OPENCLAW_DEFAULT_AGENT"

	// EnvTimeout bounds a single HTTP request — NOT an agent run. Submitting is
	// fast; the agent is slow. Confusing the two is how people end up holding a
	// connection open for twenty minutes.
	EnvTimeout = "OPENCLAW_TIMEOUT"

	// EnvRetryAttempts is the TOTAL number of attempts at an HTTP call.
	EnvRetryAttempts = "OPENCLAW_RETRY_ATTEMPTS"

	// EnvRetryDelay is the base for the exponential backoff.
	EnvRetryDelay = "OPENCLAW_RETRY_DELAY"

	// EnvMaxSteps, EnvMaxDuration and EnvMaxOutputBytes are the DEFAULT execution
	// limits — the ones a task gets when it does not ask for its own.
	//
	// They are not a nicety. An autonomous agent in a loop is a machine for turning
	// money into tokens, and "it kept trying" is a failure mode that arrives as a
	// bill. There is deliberately no way to configure "unlimited".
	EnvMaxSteps       = "OPENCLAW_MAX_STEPS"
	EnvMaxDuration    = "OPENCLAW_MAX_DURATION"
	EnvMaxOutputBytes = "OPENCLAW_MAX_OUTPUT_BYTES"

	// EnvMaxResponseBytes caps what we will read from any single HTTP response.
	EnvMaxResponseBytes = "OPENCLAW_MAX_RESPONSE_BYTES"

	// EnvPollInterval is how often to ask whether an execution has finished.
	EnvPollInterval = "OPENCLAW_POLL_INTERVAL"

	// EnvCACert is a PEM bundle, for an OpenClaw behind a private CA. There is
	// deliberately no "skip TLS verification": an environment variable that turns
	// off certificate checking eventually gets set in production and never unset.
	EnvCACert = "OPENCLAW_CA_CERT"
)

// Defaults.
const (
	DefaultAuthHeader = "Authorization"

	// A submit or a status check is a fast call. If OpenClaw needs longer than this
	// to acknowledge a request, it is unwell.
	DefaultTimeout       = 15 * time.Second
	DefaultRetryAttempts = 3
	DefaultRetryDelay    = 500 * time.Millisecond

	// The default budget for one agent run. Chosen to be *useful but survivable*:
	// enough to read a repository and write a draft, not enough to reason itself
	// into a hole overnight.
	DefaultMaxSteps       = 40
	DefaultMaxDuration    = 20 * time.Minute
	DefaultMaxOutputBytes = 1 << 20 // 1 MiB of text is a very long blog post

	DefaultMaxResponseBytes = 8 << 20 // a result can legitimately be larger than a trigger
	DefaultPollInterval     = 5 * time.Second
)

// ErrConfig means the integration is misconfigured. Always fatal at start-up: an
// agent runtime you cannot authenticate to is not something to find out about on
// the first webhook of the day.
var ErrConfig = errors.New("openclaw configuration")

// Config is everything the client needs.
type Config struct {
	BaseURL    string
	Token      string
	AuthHeader string

	// Agents maps a task type to the agent that performs it.
	Agents       map[agent.TaskType]string
	DefaultAgent string

	Timeout       time.Duration
	RetryAttempts int
	RetryDelay    time.Duration
	PollInterval  time.Duration

	// Limits are the defaults applied to a task that does not carry its own.
	Limits agent.Limits

	MaxResponseBytes int64
	CACertPath       string
}

// ConfigFromEnv reads the configuration, applies defaults, and refuses to return
// anything half-built.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		BaseURL:      strings.TrimRight(os.Getenv(EnvBaseURL), "/"),
		Token:        os.Getenv(EnvToken),
		AuthHeader:   envOr(EnvAuthHeader, DefaultAuthHeader),
		DefaultAgent: strings.TrimSpace(os.Getenv(EnvDefaultAgent)),
		CACertPath:   os.Getenv(EnvCACert),

		Timeout:          DefaultTimeout,
		RetryAttempts:    DefaultRetryAttempts,
		RetryDelay:       DefaultRetryDelay,
		PollInterval:     DefaultPollInterval,
		MaxResponseBytes: DefaultMaxResponseBytes,
		Limits: agent.Limits{
			MaxSteps:       DefaultMaxSteps,
			MaxDuration:    DefaultMaxDuration,
			MaxOutputBytes: DefaultMaxOutputBytes,
		},
	}

	if cfg.BaseURL == "" {
		return Config{}, fmt.Errorf("%w: %s is not set", ErrConfig, EnvBaseURL)
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Config{}, fmt.Errorf("%w: %s must be an absolute http(s) URL, got %q", ErrConfig, EnvBaseURL, cfg.BaseURL)
	}
	// Plain HTTP is how you develop against OpenClaw on your laptop, and is a token
	// crossing the network in clear text anywhere else.
	if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
		return Config{}, fmt.Errorf("%w: %s uses http:// against a non-local host — the token would cross the network in clear text", ErrConfig, EnvBaseURL)
	}
	if cfg.Token == "" {
		return Config{}, fmt.Errorf("%w: %s is not set (requests to OpenClaw must be authenticated)", ErrConfig, EnvToken)
	}

	if cfg.Agents, err = parseAgents(os.Getenv(EnvAgents)); err != nil {
		return Config{}, err
	}
	if len(cfg.Agents) == 0 && cfg.DefaultAgent == "" {
		return Config{}, fmt.Errorf("%w: no agents are registered (%s) and there is no default (%s), so nothing could ever be executed",
			ErrConfig, EnvAgents, EnvDefaultAgent)
	}

	if cfg.Timeout, err = envDuration(EnvTimeout, DefaultTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RetryDelay, err = envDuration(EnvRetryDelay, DefaultRetryDelay); err != nil {
		return Config{}, err
	}
	if cfg.PollInterval, err = envDuration(EnvPollInterval, DefaultPollInterval); err != nil {
		return Config{}, err
	}
	if cfg.Limits.MaxDuration, err = envDuration(EnvMaxDuration, DefaultMaxDuration); err != nil {
		return Config{}, err
	}
	if cfg.RetryAttempts, err = envInt(EnvRetryAttempts, DefaultRetryAttempts); err != nil {
		return Config{}, err
	}
	if cfg.RetryAttempts < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1 (it counts total attempts, not retries)", ErrConfig, EnvRetryAttempts)
	}
	if cfg.Limits.MaxSteps, err = envInt(EnvMaxSteps, DefaultMaxSteps); err != nil {
		return Config{}, err
	}
	if cfg.Limits.MaxSteps < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1 — an agent with no steps cannot do anything", ErrConfig, EnvMaxSteps)
	}
	if cfg.Limits.MaxOutputBytes, err = envInt64(EnvMaxOutputBytes, DefaultMaxOutputBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxResponseBytes, err = envInt64(EnvMaxResponseBytes, DefaultMaxResponseBytes); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// parseAgents reads "task-type=agent,task-type=agent".
func parseAgents(raw string) (map[agent.TaskType]string, error) {
	agents := map[agent.TaskType]string{}
	if strings.TrimSpace(raw) == "" {
		return agents, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		task, name, ok := strings.Cut(entry, "=")
		task, name = strings.TrimSpace(task), strings.TrimSpace(name)
		if !ok || task == "" || name == "" {
			return nil, fmt.Errorf("%w: %s entry %q is not task-type=agent", ErrConfig, EnvAgents, entry)
		}
		if _, exists := agents[agent.TaskType(task)]; exists {
			return nil, fmt.Errorf("%w: %s maps %q twice", ErrConfig, EnvAgents, task)
		}
		agents[agent.TaskType(task)] = name
	}
	return agents, nil
}

// AgentFor picks the agent for a task type, falling back to the default.
func (c Config) AgentFor(t agent.TaskType) (string, bool) {
	if name, ok := c.Agents[t]; ok {
		return name, true
	}
	if c.DefaultAgent != "" {
		return c.DefaultAgent, true
	}
	return "", false
}

// Tasks lists the task types that have an agent, sorted so logs and help are stable.
func (c Config) Tasks() []agent.TaskType {
	out := make([]agent.TaskType, 0, len(c.Agents))
	for t := range c.Agents {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Redacted returns the configuration with the token removed, so start-up can log
// exactly what it is configured with — which is worth a great deal at 3am, but only
// if doing so cannot leak the secret.
func (c Config) Redacted() map[string]any {
	agents := map[string]string{}
	for task, name := range c.Agents {
		agents[string(task)] = name
	}
	return map[string]any{
		"baseUrl":          c.BaseURL,
		"authHeader":       c.AuthHeader,
		"token":            httpx.Redacted(c.Token),
		"agents":           agents,
		"defaultAgent":     orNone(c.DefaultAgent),
		"timeout":          c.Timeout.String(),
		"retryAttempts":    c.RetryAttempts,
		"retryDelay":       c.RetryDelay.String(),
		"pollInterval":     c.PollInterval.String(),
		"maxSteps":         c.Limits.MaxSteps,
		"maxDuration":      c.Limits.MaxDuration.String(),
		"maxOutputBytes":   c.Limits.MaxOutputBytes,
		"maxResponseBytes": c.MaxResponseBytes,
		"caCert":           orNone(c.CACertPath),
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a duration like 20m or 500ms, got %q", ErrConfig, key, raw)
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
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a whole number, got %q", ErrConfig, key, raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%w: %s must be positive, got %q", ErrConfig, key, raw)
	}
	return n, nil
}
