package bedrock

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
		EnvModelID: "anthropic.claude-3-5-haiku-20241022-v1:0",
	}
	for k, v := range overrides {
		base[k] = v
	}
	for _, key := range []string{
		EnvRegion, EnvModelID, EnvEndpoint, EnvContextTokens, EnvMaxTokens, EnvTemperature,
		EnvTimeout, EnvIdleTimeout, EnvRetryAttempts, EnvRetryDelay, EnvStream,
		EnvInputCostPer1M, EnvOutputCostPer1M,
	} {
		t.Setenv(key, base[key])
	}
}

func TestConfigFromEnv(t *testing.T) {
	setEnv(t, map[string]string{
		EnvRegion:          "eu-west-1",
		EnvMaxTokens:       "4096",
		EnvTemperature:     "0.5",
		EnvInputCostPer1M:  "0.80",
		EnvOutputCostPer1M: "4.00",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Region != "eu-west-1" || cfg.MaxTokens != 4096 {
		t.Errorf("config = %+v, want the environment's values", cfg)
	}
	// The price is configuration, not a constant in source. A price table baked into a
	// repository is a price table that is quietly wrong within a year — and Milestone 10
	// will make routing decisions on it.
	if cfg.InputCostPer1M != 0.80 || cfg.OutputCostPer1M != 4.00 {
		t.Errorf("cost = %v/%v, want it configurable", cfg.InputCostPer1M, cfg.OutputCostPer1M)
	}
}

func TestDefaults(t *testing.T) {
	setEnv(t, nil)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if !cfg.Stream {
		t.Error("streaming should be the default, as it is for every provider")
	}
	// A completion budget by default. On a per-token provider, an unbounded generation is
	// a model free to spend your money.
	if cfg.MaxTokens <= 0 {
		t.Error("there must be a default completion budget")
	}
	if cfg.Region != DefaultRegion || cfg.Timeout != DefaultTimeout {
		t.Errorf("config = %+v, want the documented defaults", cfg)
	}
}

func TestConfigRejectsBadInput(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		because   string
	}{
		{"no model ID", map[string]string{EnvModelID: ""}, "there is no sensible default foundation model"},
		{
			"temperature above 1",
			map[string]string{EnvTemperature: "1.5"},
			"Bedrock's Converse takes [0,1] — narrower than Ollama's [0,2]",
		},
		{"negative temperature", map[string]string{EnvTemperature: "-1"}, ""},
		{"zero max tokens", map[string]string{EnvMaxTokens: "0"}, ""},
		{"absurd context window", map[string]string{EnvContextTokens: "10"}, "too small to be real"},
		{"retry attempts below 1", map[string]string{EnvRetryAttempts: "0"}, "it counts attempts, not retries"},
		{"timeout is not a duration", map[string]string{EnvTimeout: "120"}, ""},
		{"negative cost", map[string]string{EnvInputCostPer1M: "-1"}, ""},
		{"stream is not a bool", map[string]string{EnvStream: "sometimes"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.overrides)

			if _, err := ConfigFromEnv(); !errors.Is(err, ErrConfig) {
				t.Fatalf("ConfigFromEnv() = %v, want ErrConfig (%s)", err, tt.because)
			}
		})
	}
}

// The providers disagree about what a valid temperature is: Ollama takes [0, 2] and
// Bedrock's Converse takes [0, 1]. A value that is merely "creative" for one is a
// validation error for the other — and absorbing that difference, rather than passing it
// on to a caller, is precisely what a provider is for.
func TestTemperatureIsValidatedAgainstBedrocksRangeNotOllamas(t *testing.T) {
	setEnv(t, map[string]string{EnvTemperature: "1.5"}) // legal for Ollama, illegal here

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("1.5 is legal for Ollama and must be rejected for Bedrock")
	}
	// The error explains the difference rather than just refusing, because the next person
	// to hit this will have copied the value from the Ollama config.
	if !strings.Contains(err.Error(), "Ollama") {
		t.Errorf("the error should explain that the providers disagree; got %v", err)
	}
}

// The point of IAM: there is no secret to configure, so there is no secret to leak.
func TestThereIsNoCredentialInTheEnvironment(t *testing.T) {
	setEnv(t, nil)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}

	rendered, _ := json.Marshal(cfg.Redacted())
	lower := strings.ToLower(string(rendered))

	// No access key, no secret key, no session token — none of them exist as concepts here.
	for _, forbidden := range []string{"accesskey", "secretkey", "sessiontoken", "apikey"} {
		if strings.Contains(strings.ReplaceAll(lower, "_", ""), forbidden) {
			t.Errorf("the configuration mentions %q — Bedrock authenticates with IAM; a static "+
				"credential is a credential that cannot be rotated: %s", forbidden, rendered)
		}
	}
	if !strings.Contains(string(rendered), "IAM") {
		t.Error("the redacted config should say plainly that IAM resolves the credentials")
	}
}

func TestTimeoutsAreSeparate(t *testing.T) {
	setEnv(t, map[string]string{EnvTimeout: "3m", EnvIdleTimeout: "20s"})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	// The same two-timeout design as Ollama, and for the same reason: a total timeout
	// cannot tell a slow model from a hung one, and only a stream can answer "has it
	// produced a token recently?".
	if cfg.Timeout != 3*time.Minute || cfg.IdleTimeout != 20*time.Second {
		t.Errorf("config = %+v, want both timeouts honoured", cfg)
	}
}
