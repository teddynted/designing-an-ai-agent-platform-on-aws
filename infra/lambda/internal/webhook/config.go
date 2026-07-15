package webhook

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Environment variables, all set by the CloudFormation stack. As with the spot
// handler, the wiring ones (event bus, source) have no defaults — a webhook that
// guessed its bus would publish where nobody is listening and look like it worked.
// The FILTER ones do have defaults, and the default is "accept", so a freshly
// deployed webhook works before anyone has tuned it and is narrowed deliberately.
const (
	// EnvProject and EnvEnvironment identify the deployment, for the event source and
	// the logs.
	EnvProject     = "PROJECT_NAME"
	EnvEnvironment = "ENVIRONMENT"

	// EnvEventBus is the platform's own bus. EnvEventSource must match what the bus's
	// rule filters on, or the event lands where nothing routes it.
	EnvEventBus    = "EVENT_BUS_NAME"
	EnvEventSource = "EVENT_SOURCE"

	// EnvSecretARN is the Secrets Manager secret holding the shared webhook secret.
	// The secret VALUE is never an environment variable — a plaintext secret in a
	// Lambda's config is readable by anyone with lambda:GetFunctionConfiguration, which
	// is a wider audience than "can read this one secret". The Lambda fetches it at
	// cold start; see New.
	EnvSecretARN = "WEBHOOK_SECRET_ARN"

	// EnvSupportedEvents narrows the accepted events below the built-in set:
	// "push,release". Empty means "all supported events". It can only narrow — an event
	// the parser does not understand is never accepted however it is listed.
	EnvSupportedEvents = "SUPPORTED_EVENTS"

	// EnvRepoAllowList restricts which repositories are processed:
	// "owner/repo,owner/other". Empty means "any repository" — appropriate for a
	// single-tenant platform, and the first thing to set when the endpoint might be
	// pointed at by more than one.
	EnvRepoAllowList = "REPO_ALLOW_LIST"

	// EnvBranchAllowList restricts push/create/delete to certain branches:
	// "main,release/*". Empty means "any branch". A trailing "/*" is a prefix match, so
	// "release/*" matches "release/1.2".
	EnvBranchAllowList = "BRANCH_ALLOW_LIST"

	// EnvIgnoreForks drops events from forked repositories. On by default: a fork is
	// usually not the repository the platform is meant to act on, and an agent pointed
	// at an attacker's fork is an agent reading attacker-controlled content.
	EnvIgnoreForks = "IGNORE_FORKS"

	// EnvIgnoreArchived drops events from archived repositories. On by default: an
	// archived repository is read-only and inert, and automation acting on it is almost
	// always a mistake.
	EnvIgnoreArchived = "IGNORE_ARCHIVED"

	// EnvIgnoreBranchDeletes drops branch/tag deletions. On by default: "a branch was
	// deleted" is rarely something to start an AI workflow over, and the deletion has
	// no commit to check out, so most agents could not act on it anyway.
	EnvIgnoreBranchDeletes = "IGNORE_BRANCH_DELETES"
)

// Config is the handler's deployment-specific wiring and its filtering policy.
type Config struct {
	Project     string
	Environment string
	EventBus    string
	EventSource string

	// Secret is the shared webhook secret, fetched from Secrets Manager at cold start.
	// It is never logged and never leaves this struct.
	Secret string

	// SupportedEvents is the accepted set. Empty means "every event the parser
	// understands".
	SupportedEvents []string

	// RepoAllowList, empty means any.
	RepoAllowList []string

	// BranchAllowList, empty means any. Entries ending in "/*" are prefix matches.
	BranchAllowList []string

	IgnoreForks         bool
	IgnoreArchived      bool
	IgnoreBranchDeletes bool
}

// ConfigFromEnv reads the configuration, failing if the wiring is missing. Filter
// settings are optional and default to the safe, narrow-able "accept" — except the
// three ignore flags, whose safe default is "ignore".
//
// Note the SECRET is not read here: it comes from Secrets Manager, not the
// environment, and it is fetched in New. ConfigFromEnv reads everything that is safe
// to be an environment variable, and nothing that is not.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Project:         os.Getenv(EnvProject),
		Environment:     os.Getenv(EnvEnvironment),
		EventBus:        os.Getenv(EnvEventBus),
		EventSource:     os.Getenv(EnvEventSource),
		SupportedEvents: splitList(os.Getenv(EnvSupportedEvents)),
		RepoAllowList:   splitList(os.Getenv(EnvRepoAllowList)),
		BranchAllowList: splitList(os.Getenv(EnvBranchAllowList)),
	}

	missing := []string{}
	for name, value := range map[string]string{
		EnvProject:     cfg.Project,
		EnvEnvironment: cfg.Environment,
		EnvEventBus:    cfg.EventBus,
		EnvEventSource: cfg.EventSource,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing environment variables: %v", missing)
	}

	// Any listed supported event must be one the parser understands. A configuration
	// that named an event the code cannot read would accept it and then fail on it —
	// so refuse to boot instead, at deploy time, where a human is watching.
	for _, e := range cfg.SupportedEvents {
		if !IsSupportedEvent(e) {
			return Config{}, fmt.Errorf("%s lists %q, which this webhook does not understand (known: %s)",
				EnvSupportedEvents, e, strings.Join(knownEvents(), ", "))
		}
	}

	var err error
	if cfg.IgnoreForks, err = envBool(EnvIgnoreForks, true); err != nil {
		return Config{}, err
	}
	if cfg.IgnoreArchived, err = envBool(EnvIgnoreArchived, true); err != nil {
		return Config{}, err
	}
	if cfg.IgnoreBranchDeletes, err = envBool(EnvIgnoreBranchDeletes, true); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Redacted returns the configuration for logging. The secret is not in it — not
// redacted to a placeholder, ABSENT — because the safest way to never log a secret is
// to never put it somewhere that gets logged.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"project":             c.Project,
		"environment":         c.Environment,
		"eventBus":            c.EventBus,
		"eventSource":         c.EventSource,
		"supportedEvents":     orAny(c.SupportedEvents),
		"repoAllowList":       orAny(c.RepoAllowList),
		"branchAllowList":     orAny(c.BranchAllowList),
		"ignoreForks":         c.IgnoreForks,
		"ignoreArchived":      c.IgnoreArchived,
		"ignoreBranchDeletes": c.IgnoreBranchDeletes,
		"secret":              "(from Secrets Manager; never logged)",
	}
}

func knownEvents() []string {
	return []string{EventPush, EventRelease, EventCreate, EventDelete, EventWorkflowRun, EventRepository, EventPing}
}

func orAny(list []string) any {
	if len(list) == 0 {
		return "(any)"
	}
	return list
}

// splitList parses a comma-separated env var, trimming and dropping blanks.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", key, raw)
	}
	return b, nil
}
