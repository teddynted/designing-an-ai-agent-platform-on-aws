package n8n

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// setEnv sets a complete, valid environment, then applies the overrides. An empty
// override value unsets the variable — which is how each test expresses "this one
// is missing".
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		EnvBaseURL:   "https://n8n.example.com",
		EnvToken:     "a-token",
		EnvWorkflows: "blog-generator=/webhook/blog",
	}
	for k, v := range overrides {
		base[k] = v
	}
	for _, key := range []string{
		EnvBaseURL, EnvToken, EnvAuthHeader, EnvWorkflows, EnvTimeout,
		EnvRetryAttempts, EnvRetryDelay, EnvMaxPayloadBytes, EnvMaxResponseBytes, EnvCACert,
	} {
		t.Setenv(key, base[key]) // absent from the map == ""
	}
}

func TestConfigFromEnv(t *testing.T) {
	setEnv(t, map[string]string{
		EnvWorkflows:     "blog-generator=/webhook/blog, release-notes=/webhook/notes",
		EnvTimeout:       "3s",
		EnvRetryAttempts: "5",
		EnvRetryDelay:    "250ms",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Timeout != 3*time.Second || cfg.RetryAttempts != 5 || cfg.RetryDelay != 250*time.Millisecond {
		t.Errorf("config = %+v, want the environment's values", cfg)
	}
	if cfg.AuthHeader != DefaultAuthHeader {
		t.Errorf("AuthHeader = %q, want the default %q", cfg.AuthHeader, DefaultAuthHeader)
	}
	if got := cfg.Workflows["release-notes"]; got != "/webhook/notes" {
		t.Errorf("release-notes = %q, want /webhook/notes (whitespace should be trimmed)", got)
	}
	// Sorted, so logs and --help are stable.
	if names := cfg.Names(); names[0] != "blog-generator" || names[1] != "release-notes" {
		t.Errorf("Names() = %v, want them sorted", names)
	}
}

// A misconfigured integration must fail at start-up, not on the first webhook of
// the day. Each of these is a way to be silently broken in production.
func TestConfigRejectsBadInput(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		because   string
	}{
		{"no base URL", map[string]string{EnvBaseURL: ""}, "there is nowhere to send anything"},
		{"no token", map[string]string{EnvToken: ""}, "requests would go out unauthenticated"},
		{"no workflows", map[string]string{EnvWorkflows: ""}, "nothing could ever be triggered"},
		{"base URL is not a URL", map[string]string{EnvBaseURL: "not-a-url"}, ""},
		{"base URL has no scheme", map[string]string{EnvBaseURL: "n8n.example.com"}, ""},
		{
			"plain http to a remote host",
			map[string]string{EnvBaseURL: "http://n8n.example.com"},
			"the token would cross the network in clear text",
		},
		{"workflow entry has no path", map[string]string{EnvWorkflows: "blog-generator"}, ""},
		{"workflow path is not rooted", map[string]string{EnvWorkflows: "blog=webhook/blog"}, ""},
		{"workflow registered twice", map[string]string{EnvWorkflows: "blog=/a,blog=/b"}, "the second would silently win"},
		{"timeout is not a duration", map[string]string{EnvTimeout: "10"}, ""},
		{"timeout is negative", map[string]string{EnvTimeout: "-1s"}, ""},
		{"retry attempts is zero", map[string]string{EnvRetryAttempts: "0"}, "it counts attempts, not retries"},
		{"retry attempts is not a number", map[string]string{EnvRetryAttempts: "many"}, ""},
		{"payload cap is not a number", map[string]string{EnvMaxPayloadBytes: "big"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.overrides)
			_, err := ConfigFromEnv()
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("ConfigFromEnv() = %v, want ErrConfig (%s)", err, tt.because)
			}
			if strings.Contains(err.Error(), "a-token") {
				t.Errorf("the token leaked into a configuration error: %v", err)
			}
		})
	}
}

// http://localhost is how you develop against n8n on your laptop, and it must
// keep working — the clear-text rule is about the network, not about the scheme.
func TestPlainHTTPIsAllowedForLocalhost(t *testing.T) {
	for _, host := range []string{"http://localhost:5678", "http://127.0.0.1:5678"} {
		setEnv(t, map[string]string{EnvBaseURL: host})
		if _, err := ConfigFromEnv(); err != nil {
			t.Errorf("ConfigFromEnv(%s) = %v, want it allowed for local development", host, err)
		}
	}
}

func TestTrailingSlashIsTrimmed(t *testing.T) {
	// Otherwise every URL becomes https://host//webhook/blog, which some proxies
	// treat as a different path — and it 404s in a way nobody enjoys debugging.
	setEnv(t, map[string]string{EnvBaseURL: "https://n8n.example.com/"})
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if strings.HasSuffix(cfg.BaseURL, "/") {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", cfg.BaseURL)
	}
}

func TestDefaultsApply(t *testing.T) {
	setEnv(t, nil)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Timeout != DefaultTimeout || cfg.RetryAttempts != DefaultRetryAttempts ||
		cfg.RetryDelay != DefaultRetryDelay || cfg.MaxPayloadBytes != DefaultMaxPayloadBytes {
		t.Errorf("config = %+v, want the documented defaults", cfg)
	}
}
