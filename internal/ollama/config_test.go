package ollama

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		EnvBaseURL: "http://10.20.1.5:11434",
		EnvModel:   "llama3.2",
	}
	for k, v := range overrides {
		base[k] = v
	}
	for _, key := range []string{
		EnvBaseURL, EnvModel, EnvToken, EnvTimeout, EnvIdleTimeout, EnvRetryAttempts,
		EnvRetryDelay, EnvContextTokens, EnvMaxTokens, EnvTemperature, EnvStream,
		EnvKeepAlive, EnvMaxResponseBytes, EnvCACert,
	} {
		t.Setenv(key, base[key])
	}
}

func TestConfigFromEnv(t *testing.T) {
	setEnv(t, map[string]string{
		EnvIdleTimeout:   "30s",
		EnvContextTokens: "32768",
		EnvMaxTokens:     "4096",
		EnvTemperature:   "0.7",
		EnvStream:        "false",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.IdleTimeout != 30*time.Second || cfg.ContextTokens != 32768 || cfg.MaxTokens != 4096 {
		t.Errorf("config = %+v, want the environment's values", cfg)
	}
	if cfg.Temperature != 0.7 || cfg.Stream {
		t.Errorf("config = %+v", cfg)
	}
}

func TestDefaults(t *testing.T) {
	setEnv(t, nil)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	// Streaming is ON by default: a generation you cannot see is indistinguishable from
	// a hang, and a stall is only detectable in a stream.
	if !cfg.Stream {
		t.Error("streaming should be the default")
	}
	// A completion budget by default: unbounded generation on hardware billed by the
	// hour is a model free to ramble at your expense.
	if cfg.MaxTokens <= 0 {
		t.Error("there must be a default completion budget")
	}
	if cfg.IdleTimeout <= 0 || cfg.ContextTokens <= 0 {
		t.Errorf("config = %+v, want the documented defaults", cfg)
	}
	// Low by default: most of what this platform does is summarising, not inventing.
	if cfg.Temperature > 0.5 {
		t.Errorf("Temperature = %v, want a low default for summarisation work", cfg.Temperature)
	}
}

func TestConfigRejectsBadInput(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		because   string
	}{
		{"no base URL", map[string]string{EnvBaseURL: ""}, "there is nowhere to send anything"},
		{"no model", map[string]string{EnvModel: ""}, "there is no sensible default model"},
		{"base URL is not a URL", map[string]string{EnvBaseURL: "10.20.1.5:11434"}, "no scheme"},
		{
			"a token over plain http to a public host",
			map[string]string{EnvBaseURL: "http://ollama.example.com", EnvToken: "secret"},
			"the token would cross the internet in clear text",
		},
		{"context window absurdly small", map[string]string{EnvContextTokens: "10"}, "too small to be real"},
		{"zero max tokens", map[string]string{EnvMaxTokens: "0"}, "a model that may emit nothing"},
		{"temperature out of range", map[string]string{EnvTemperature: "9"}, ""},
		{"retry attempts below 1", map[string]string{EnvRetryAttempts: "0"}, "it counts attempts, not retries"},
		{"idle timeout is not a duration", map[string]string{EnvIdleTimeout: "30"}, ""},
		{"stream is not a bool", map[string]string{EnvStream: "yes please"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.overrides)

			_, err := ConfigFromEnv()
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("ConfigFromEnv() = %v, want ErrConfig (%s)", err, tt.because)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("the token leaked into a configuration error: %v", err)
			}
		})
	}
}

// Plain http to a PRIVATE host is normal and correct here: Ollama has no authentication
// of its own and is meant to live on a private network, behind a security group that
// lets nothing in. The rule is about tokens crossing the public internet, not about the
// scheme.
func TestPlainHTTPToAPrivateHostIsFine(t *testing.T) {
	for _, host := range []string{
		"http://localhost:11434",
		"http://10.20.1.5:11434",
		"http://192.168.1.10:11434",
		"http://ollama.internal:11434",
	} {
		setEnv(t, map[string]string{EnvBaseURL: host, EnvToken: "a-token"})
		if _, err := ConfigFromEnv(); err != nil {
			t.Errorf("ConfigFromEnv(%s) = %v, want it allowed — this is a private network", host, err)
		}
	}
}

// And with no token, plain http anywhere is at worst your own problem: there is no
// credential to leak.
func TestPlainHTTPWithNoTokenIsAllowed(t *testing.T) {
	setEnv(t, map[string]string{EnvBaseURL: "http://ollama.example.com", EnvToken: ""})
	if _, err := ConfigFromEnv(); err != nil {
		t.Errorf("ConfigFromEnv = %v, want it allowed when there is no credential to leak", err)
	}
}

func TestRedactedHidesTheToken(t *testing.T) {
	setEnv(t, map[string]string{EnvToken: "super-secret-proxy-token"})
	cfg, _ := ConfigFromEnv()

	rendered, _ := json.Marshal(cfg.Redacted())
	if strings.Contains(string(rendered), "super-secret-proxy-token") {
		t.Errorf("Redacted() leaked the token: %s", rendered)
	}
	if !strings.Contains(string(rendered), "set,") {
		t.Errorf("Redacted() must still confirm a token IS set: %s", rendered)
	}
}
