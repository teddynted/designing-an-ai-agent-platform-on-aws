package n8n

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Environment variables. Everything that differs between a laptop, dev, and prod
// is here, and nothing else is: there is not a single n8n URL, token, or workflow
// path compiled into this repository.
const (
	// EnvBaseURL is the n8n instance, e.g. https://n8n.internal.example.com.
	// It is deployed and owned by the self-hosted-n8n-on-aws repository; this one
	// only needs to know where it ended up.
	EnvBaseURL = "N8N_BASE_URL"

	// EnvToken is the shared secret n8n checks on the inbound webhook. Never
	// logged, never included in an error, never echoed back.
	EnvToken = "N8N_TOKEN"

	// EnvAuthHeader is the header the token goes in. n8n's own "Header Auth"
	// credential lets you choose the header name, so this must be configurable
	// rather than assumed.
	EnvAuthHeader = "N8N_AUTH_HEADER"

	// EnvWorkflows maps logical workflow names to n8n webhook paths:
	//
	//   N8N_WORKFLOWS=blog-generator=/webhook/blog-generator,release-notes=/webhook/release-notes
	//
	// This is the seam that makes a new workflow a configuration change rather
	// than a code change — which is most of the point of using an orchestrator at
	// all. Adding "social-publisher" to the platform is one entry here and a
	// workflow drawn in the n8n UI; this repository does not get recompiled.
	EnvWorkflows = "N8N_WORKFLOWS"

	// EnvTimeout bounds a single attempt, e.g. 10s.
	EnvTimeout = "N8N_TIMEOUT"

	// EnvRetryAttempts is the TOTAL number of attempts, not the number of retries
	// after the first. 1 means "try once, never retry".
	EnvRetryAttempts = "N8N_RETRY_ATTEMPTS"

	// EnvRetryDelay is the base delay for the exponential backoff, e.g. 500ms.
	EnvRetryDelay = "N8N_RETRY_DELAY"

	// EnvMaxPayloadBytes caps the event payload we are willing to forward. A
	// GitHub payload is usually a few KB and occasionally enormous; an
	// orchestrator is not the right place to find that out.
	EnvMaxPayloadBytes = "N8N_MAX_PAYLOAD_BYTES"

	// EnvMaxResponseBytes caps what we will read back. Without it, a misbehaving
	// or compromised engine can exhaust our memory just by answering.
	EnvMaxResponseBytes = "N8N_MAX_RESPONSE_BYTES"

	// EnvCACert is the path to a PEM bundle, for an n8n behind a private CA.
	// There is deliberately no "skip TLS verification" option: an environment
	// variable that turns off certificate checking is a foot-gun that eventually
	// gets set in production and never gets unset.
	EnvCACert = "N8N_CA_CERT"
)

// Defaults. They are chosen for a webhook handler that must answer GitHub
// quickly: fail fast, retry a little, never hang.
const (
	DefaultAuthHeader       = "X-N8N-Api-Key"
	DefaultTimeout          = 10 * time.Second
	DefaultRetryAttempts    = 3
	DefaultRetryDelay       = 500 * time.Millisecond
	DefaultMaxPayloadBytes  = 1 << 20 // 1 MiB
	DefaultMaxResponseBytes = 1 << 20
)

// ErrConfig means the integration is misconfigured. It is always fatal at start
// up: a workflow engine you cannot authenticate to is not something to discover
// on the first webhook of the day.
var ErrConfig = errors.New("n8n configuration")

// Config is everything the client needs. Build it with [ConfigFromEnv]; a test
// builds it directly.
type Config struct {
	BaseURL    string
	Token      string
	AuthHeader string

	// Workflows maps a logical name to the n8n webhook path it lives at.
	Workflows map[string]string

	Timeout          time.Duration
	RetryAttempts    int
	RetryDelay       time.Duration
	MaxPayloadBytes  int64
	MaxResponseBytes int64
	CACertPath       string
}

// ConfigFromEnv reads the configuration, applies the defaults, and refuses to
// return anything half-built.
//
// It fails loudly on a missing token rather than quietly sending unauthenticated
// requests, because an integration that silently degrades to "no auth" is worse
// than one that will not start.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		BaseURL:          strings.TrimRight(os.Getenv(EnvBaseURL), "/"),
		Token:            os.Getenv(EnvToken),
		AuthHeader:       envOr(EnvAuthHeader, DefaultAuthHeader),
		Timeout:          DefaultTimeout,
		RetryAttempts:    DefaultRetryAttempts,
		RetryDelay:       DefaultRetryDelay,
		MaxPayloadBytes:  DefaultMaxPayloadBytes,
		MaxResponseBytes: DefaultMaxResponseBytes,
		CACertPath:       os.Getenv(EnvCACert),
	}

	if cfg.BaseURL == "" {
		return Config{}, fmt.Errorf("%w: %s is not set", ErrConfig, EnvBaseURL)
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Config{}, fmt.Errorf("%w: %s must be an absolute http(s) URL, got %q", ErrConfig, EnvBaseURL, cfg.BaseURL)
	}
	// Plain HTTP is fine against localhost while you develop, and is a token
	// leaked over the wire anywhere else.
	if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
		return Config{}, fmt.Errorf("%w: %s uses http:// against a non-local host — the token would cross the network in clear text", ErrConfig, EnvBaseURL)
	}
	if cfg.Token == "" {
		return Config{}, fmt.Errorf("%w: %s is not set (requests to n8n must be authenticated)", ErrConfig, EnvToken)
	}

	if cfg.Workflows, err = parseWorkflows(os.Getenv(EnvWorkflows)); err != nil {
		return Config{}, err
	}

	if cfg.Timeout, err = envDuration(EnvTimeout, DefaultTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RetryDelay, err = envDuration(EnvRetryDelay, DefaultRetryDelay); err != nil {
		return Config{}, err
	}
	if cfg.RetryAttempts, err = envInt(EnvRetryAttempts, DefaultRetryAttempts); err != nil {
		return Config{}, err
	}
	if cfg.RetryAttempts < 1 {
		return Config{}, fmt.Errorf("%w: %s must be at least 1 (it counts total attempts, not retries)", ErrConfig, EnvRetryAttempts)
	}
	if cfg.MaxPayloadBytes, err = envInt64(EnvMaxPayloadBytes, DefaultMaxPayloadBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxResponseBytes, err = envInt64(EnvMaxResponseBytes, DefaultMaxResponseBytes); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// parseWorkflows reads "name=/path,other=/other-path".
//
// A workflow with no path is not registered, and one with a path that is not
// rooted is a typo — both fail here, at start-up, rather than as a 404 from n8n
// on the day the workflow is first needed.
func parseWorkflows(raw string) (map[string]string, error) {
	workflows := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: %s is not set (no workflows are registered, so nothing could ever be triggered)", ErrConfig, EnvWorkflows)
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, path, ok := strings.Cut(entry, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("%w: %s entry %q is not name=/path", ErrConfig, EnvWorkflows, entry)
		}
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("%w: %s path for %q must start with / (got %q)", ErrConfig, EnvWorkflows, name, path)
		}
		if _, exists := workflows[name]; exists {
			return nil, fmt.Errorf("%w: %s registers %q twice", ErrConfig, EnvWorkflows, name)
		}
		workflows[name] = path
	}
	if len(workflows) == 0 {
		return nil, fmt.Errorf("%w: %s registers no workflows", ErrConfig, EnvWorkflows)
	}
	return workflows, nil
}

// Names lists the registered workflows, sorted, so logs and help text are stable.
func (c Config) Names() []string {
	names := make([]string, 0, len(c.Workflows))
	for name := range c.Workflows {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Redacted returns the configuration with the token removed, so it can be logged
// at start-up. Being able to log "here is exactly what I am configured with" is
// worth a great deal at 3am — but only if doing so cannot leak the secret.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"baseUrl":          c.BaseURL,
		"authHeader":       c.AuthHeader,
		"token":            redacted(c.Token),
		"workflows":        c.Names(),
		"timeout":          c.Timeout.String(),
		"retryAttempts":    c.RetryAttempts,
		"retryDelay":       c.RetryDelay.String(),
		"maxPayloadBytes":  c.MaxPayloadBytes,
		"maxResponseBytes": c.MaxResponseBytes,
		"caCert":           c.CACertPath,
	}
}

// redacted describes a secret without revealing it. It reports only that one is
// set and how long it is — never a prefix, because a prefix of a short token is
// most of the token.
func redacted(secret string) string {
	if secret == "" {
		return "(not set)"
	}
	return fmt.Sprintf("(set, %d chars)", len(secret))
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
		return 0, fmt.Errorf("%w: %s must be a duration like 10s or 500ms, got %q", ErrConfig, key, raw)
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
