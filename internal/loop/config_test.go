package loop

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.MaxIterations != DefaultMaxIterations {
		t.Errorf("maxIterations = %d, want the default %d", cfg.MaxIterations, DefaultMaxIterations)
	}
	if !cfg.Reflection {
		t.Error("reflection should default on — a loop that cannot learn retries the same failure")
	}
	if cfg.MaxCostUSD != DefaultMaxCostUSD {
		t.Errorf("maxCostUsd = %v, want the default %v", cfg.MaxCostUSD, DefaultMaxCostUSD)
	}
}

func TestConfigParsesKnobs(t *testing.T) {
	t.Setenv(EnvMaxIterations, "20")
	t.Setenv(EnvMaxRetries, "5")
	t.Setenv(EnvTimeout, "1h")
	t.Setenv(EnvMaxCostUSD, "12.50")
	t.Setenv(EnvReflection, "false")
	t.Setenv(EnvBackoffMultiplier, "3")
	t.Setenv(EnvMinConfidence, "0.7")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.MaxIterations != 20 || cfg.Retry.MaxRetries != 5 {
		t.Errorf("iterations/retries = %d/%d, want 20/5", cfg.MaxIterations, cfg.Retry.MaxRetries)
	}
	if cfg.Timeout != time.Hour {
		t.Errorf("timeout = %v, want 1h", cfg.Timeout)
	}
	if cfg.MaxCostUSD != 12.50 {
		t.Errorf("maxCostUsd = %v, want 12.50", cfg.MaxCostUSD)
	}
	if cfg.Reflection {
		t.Error("reflection should be off")
	}
	if cfg.Retry.Multiplier != 3 || cfg.MinConfidence != 0.7 {
		t.Errorf("multiplier/minConfidence = %v/%v, want 3/0.7", cfg.Retry.Multiplier, cfg.MinConfidence)
	}
}

func TestConfigRejects(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"zero iterations", map[string]string{EnvMaxIterations: "0"}, "at least 1"},
		{"negative retries", map[string]string{EnvMaxRetries: "-1"}, "negative"},
		{"a multiplier below 1", map[string]string{EnvBackoffMultiplier: "0.5"}, "at least 1.0"},
		{"a nonsense duration", map[string]string{EnvTimeout: "soon"}, "duration"},
		{"confidence out of range", map[string]string{EnvMinConfidence: "1.5"}, "[0, 1]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := ConfigFromEnv()
			if err == nil {
				t.Fatal("want a start-up error")
			}
			if !errors.Is(err, ErrConfig) {
				t.Errorf("err = %v, want it to wrap ErrConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The loop holds no secret, because it calls nothing. Worth showing in the log.
func TestRedactedSaysThereIsNothingToRedact(t *testing.T) {
	cfg, _ := ConfigFromEnv()
	creds, _ := cfg.Redacted()["credentials"].(string)
	if !strings.Contains(creds, "none") {
		t.Errorf("credentials = %q, want it to say there are none", creds)
	}
}
