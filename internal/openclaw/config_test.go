package openclaw

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/agent"
)

// setEnv sets a complete, valid environment then applies the overrides. An empty
// value unsets the variable — which is how a test says "this one is missing".
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		EnvBaseURL: "https://openclaw.example.com",
		EnvToken:   "a-token",
		EnvAgents:  "blog-draft=writer,repo-analysis=analyst",
	}
	for k, v := range overrides {
		base[k] = v
	}
	for _, key := range []string{
		EnvBaseURL, EnvToken, EnvAuthHeader, EnvAgents, EnvDefaultAgent, EnvTimeout,
		EnvRetryAttempts, EnvRetryDelay, EnvMaxSteps, EnvMaxDuration, EnvMaxOutputBytes,
		EnvMaxResponseBytes, EnvPollInterval, EnvCACert,
	} {
		t.Setenv(key, base[key])
	}
}

func TestConfigFromEnv(t *testing.T) {
	setEnv(t, map[string]string{
		EnvTimeout:     "5s",
		EnvMaxSteps:    "25",
		EnvMaxDuration: "10m",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", cfg.Timeout)
	}
	if cfg.Limits.MaxSteps != 25 || cfg.Limits.MaxDuration != 10*time.Minute {
		t.Errorf("limits = %+v, want the environment's budget", cfg.Limits)
	}
	if cfg.Agents[agent.TaskBlogDraft] != "writer" {
		t.Errorf("blog-draft maps to %q, want writer", cfg.Agents[agent.TaskBlogDraft])
	}
	// Adding an agent type must be configuration, never code. This is that promise,
	// asserted.
	if len(cfg.Tasks()) != 2 {
		t.Errorf("Tasks() = %v, want both registered task types", cfg.Tasks())
	}
}

func TestConfigRejectsBadInput(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		because   string
	}{
		{"no base URL", map[string]string{EnvBaseURL: ""}, "there is nowhere to send anything"},
		{"no token", map[string]string{EnvToken: ""}, "requests would go out unauthenticated"},
		{
			"no agents and no default",
			map[string]string{EnvAgents: "", EnvDefaultAgent: ""},
			"nothing could ever be executed",
		},
		{
			"plain http to a remote host",
			map[string]string{EnvBaseURL: "http://openclaw.example.com"},
			"the token would cross the network in clear text",
		},
		{"agent entry is not task=agent", map[string]string{EnvAgents: "writer"}, ""},
		{"a task mapped twice", map[string]string{EnvAgents: "blog-draft=a,blog-draft=b"}, "the second would silently win"},
		{"zero steps", map[string]string{EnvMaxSteps: "0"}, "an agent with no steps cannot do anything"},
		{"negative duration", map[string]string{EnvMaxDuration: "-5m"}, ""},
		{"retry attempts below 1", map[string]string{EnvRetryAttempts: "0"}, "it counts attempts, not retries"},
		{"timeout is not a duration", map[string]string{EnvTimeout: "15"}, ""},
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

// A default agent alone is a valid configuration: one generalist that handles
// everything is a perfectly reasonable place to start.
func TestADefaultAgentAloneIsEnough(t *testing.T) {
	setEnv(t, map[string]string{EnvAgents: "", EnvDefaultAgent: "generalist"})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if name, ok := cfg.AgentFor(agent.TaskReleaseNotes); !ok || name != "generalist" {
		t.Errorf("AgentFor = %q/%v, want the default to catch an unmapped task", name, ok)
	}
}

func TestLocalhostMayUsePlainHTTP(t *testing.T) {
	// This is how you develop against OpenClaw on your laptop, and it must keep
	// working: the clear-text rule is about the network, not about the scheme.
	for _, host := range []string{"http://localhost:8088", "http://127.0.0.1:8088"} {
		setEnv(t, map[string]string{EnvBaseURL: host})
		if _, err := ConfigFromEnv(); err != nil {
			t.Errorf("ConfigFromEnv(%s) = %v, want it allowed for local development", host, err)
		}
	}
}

func TestDefaultsAreASurvivableBudget(t *testing.T) {
	setEnv(t, nil)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	// There is deliberately no way to configure "unlimited". An autonomous agent in
	// a loop is a machine for turning money into tokens.
	if cfg.Limits.MaxSteps <= 0 || cfg.Limits.MaxDuration <= 0 || cfg.Limits.MaxOutputBytes <= 0 {
		t.Errorf("limits = %+v, want a budget by default", cfg.Limits)
	}
	if cfg.Limits.MaxSteps != DefaultMaxSteps || cfg.Limits.MaxDuration != DefaultMaxDuration {
		t.Errorf("limits = %+v, want the documented defaults", cfg.Limits)
	}
}

func TestRedactedHidesTheTokenButProvesItIsSet(t *testing.T) {
	setEnv(t, nil)
	cfg, _ := ConfigFromEnv()

	rendered, _ := json.Marshal(cfg.Redacted())
	if strings.Contains(string(rendered), "a-token") {
		t.Errorf("Redacted() leaked the token: %s", rendered)
	}
	if !strings.Contains(string(rendered), "set,") {
		t.Errorf("Redacted() must still confirm a token IS set: %s", rendered)
	}
}
