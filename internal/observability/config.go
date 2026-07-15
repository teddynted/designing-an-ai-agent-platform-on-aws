package observability

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// Environment variables. As everywhere else in this repository, what differs
// between a laptop, dev and prod is configuration, not code: there is no service
// name, log level or metric namespace compiled in.
const (
	// EnvService names the process, and becomes [FieldService] on every line. It is
	// the one setting worth getting right — a log group full of lines with no
	// service is a log group you cannot slice.
	EnvService = "OBS_SERVICE"

	// EnvLogLevel is debug, info, warn or error. Info by default: debug is a
	// firehose in production and warn hides the "requested/completed" pair that
	// makes a workflow followable.
	EnvLogLevel = "OBS_LOG_LEVEL"

	// EnvLogFormat is "json" (default, for a machine) or "text" (for a terminal).
	EnvLogFormat = "OBS_LOG_FORMAT"

	// EnvLogSource adds a file:line to every record. Off by default — it is noise
	// and cost in production, and the correlation IDs locate a problem better.
	EnvLogSource = "OBS_LOG_SOURCE"

	// EnvMetricsNamespace is the CloudWatch namespace custom metrics land in, e.g.
	// "aiap/app". Empty disables metric emission entirely (see [Config.Metrics]),
	// which is the right default for a context where nothing consumes them.
	EnvMetricsNamespace = "OBS_METRICS_NAMESPACE"

	// EnvMetricsEnabled can force metrics off even when a namespace is set — useful
	// for a CLI run where the EMF lines would just be clutter on the terminal.
	EnvMetricsEnabled = "OBS_METRICS_ENABLED"
)

// Config is everything the observability layer needs, resolved once at start-up.
// It holds no secrets — an observability configuration that needed a credential
// would be one that reached somewhere it should not.
type Config struct {
	// Service is stamped on every log line and is the default EMF dimension.
	Service string

	// Level is the minimum level that is emitted.
	Level slog.Level

	// Format is "json" or "text".
	Format string

	// AddSource adds a file:line to each record.
	AddSource bool

	// MetricsNamespace is the CloudWatch namespace for EMF metrics. Empty means no
	// namespace, and [Config.Metrics] is then false.
	MetricsNamespace string

	// metricsDisabled records an explicit OBS_METRICS_ENABLED=false, which turns
	// metrics off even when a namespace is present.
	metricsDisabled bool
}

// ConfigFromEnv reads the configuration from the environment, applying the safe
// defaults above. An unrecognised log level is a configuration mistake worth
// being loud about, so it falls back to info and the caller is free to log the
// substitution — nothing here panics, because a process should still start and
// still log even when its logging is slightly misconfigured.
func ConfigFromEnv() Config {
	return Config{
		Service:          os.Getenv(EnvService),
		Level:            parseLevel(os.Getenv(EnvLogLevel)),
		Format:           os.Getenv(EnvLogFormat),
		AddSource:        parseBool(os.Getenv(EnvLogSource), false),
		MetricsNamespace: strings.TrimSpace(os.Getenv(EnvMetricsNamespace)),
		metricsDisabled:  !parseBool(os.Getenv(EnvMetricsEnabled), true),
	}
}

// Metrics reports whether EMF metric emission is on: a namespace is set and it has
// not been explicitly disabled. When it is off, [Emitter] becomes a no-op, so a
// caller never has to guard its metric calls with an if.
func (c Config) Metrics() bool {
	return c.MetricsNamespace != "" && !c.metricsDisabled
}

func (c Config) level() slog.Level { return c.Level }

func (c Config) textFormat() bool {
	return strings.EqualFold(strings.TrimSpace(c.Format), "text")
}

// parseLevel maps a name to an slog level, defaulting to info. It is lenient about
// case and whitespace, because "INFO " in a YAML file should not silence a
// process.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func parseBool(s string, def bool) bool {
	if strings.TrimSpace(s) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return b
}
