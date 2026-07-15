package webhook

import (
	"strings"
	"testing"
)

// setEnv sets the required wiring vars and returns a cleanup. The filter vars are left
// to each test.
func setWiring(t *testing.T) {
	t.Helper()
	t.Setenv(EnvProject, "aiap")
	t.Setenv(EnvEnvironment, "dev")
	t.Setenv(EnvEventBus, "aiap-dev-bus")
	t.Setenv(EnvEventSource, "aiap.dev.github")
}

func TestConfigFromEnvDefaults(t *testing.T) {
	setWiring(t)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	// The safe defaults: accept every event/repo/branch, but ignore forks, archived,
	// and deletions.
	if len(cfg.SupportedEvents) != 0 || len(cfg.RepoAllowList) != 0 {
		t.Errorf("filters should default to empty (any); got %+v", cfg)
	}
	if !cfg.IgnoreForks || !cfg.IgnoreArchived || !cfg.IgnoreBranchDeletes {
		t.Error("the ignore flags should default ON — the safe default for each is to ignore")
	}
}

func TestConfigFromEnvParsesFilters(t *testing.T) {
	setWiring(t)
	t.Setenv(EnvSupportedEvents, "push, release")
	t.Setenv(EnvRepoAllowList, "acme/platform,acme/docs")
	t.Setenv(EnvBranchAllowList, "main,release/*")
	t.Setenv(EnvIgnoreForks, "false")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if len(cfg.SupportedEvents) != 2 || cfg.SupportedEvents[0] != "push" {
		t.Errorf("supportedEvents = %v", cfg.SupportedEvents)
	}
	if len(cfg.RepoAllowList) != 2 || len(cfg.BranchAllowList) != 2 {
		t.Errorf("allow-lists = %v / %v", cfg.RepoAllowList, cfg.BranchAllowList)
	}
	if cfg.IgnoreForks {
		t.Error("IgnoreForks should be false when set so")
	}
}

func TestConfigFromEnvRequiresWiring(t *testing.T) {
	// No wiring set at all.
	t.Setenv(EnvProject, "")
	t.Setenv(EnvEventBus, "")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("missing wiring must fail — a webhook that guessed its bus would publish nowhere")
	}
	if !strings.Contains(err.Error(), EnvEventBus) {
		t.Errorf("err = %v, want it to name the missing var", err)
	}
}

// A supported-events list naming an event the parser cannot read is refused at
// config time — better a deploy-time error than an accepted event that then fails.
func TestConfigRejectsAnUnknownSupportedEvent(t *testing.T) {
	setWiring(t)
	t.Setenv(EnvSupportedEvents, "push,telepathy")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("an unknown supported event must fail config")
	}
	if !strings.Contains(err.Error(), "telepathy") {
		t.Errorf("err = %v, want it to name the offending event", err)
	}
}

// The redacted config must not carry the secret — it is absent, not masked.
func TestRedactedHasNoSecret(t *testing.T) {
	cfg := Config{Secret: "super-secret-value", EventBus: "b"}
	red := cfg.Redacted()
	if s, _ := red["secret"].(string); strings.Contains(s, "super-secret-value") {
		t.Error("the secret value leaked into the redacted config")
	}
}
