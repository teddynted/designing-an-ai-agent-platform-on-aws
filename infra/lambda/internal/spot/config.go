package spot

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
)

// Environment variables, all set by the CloudFormation stack. There are no
// defaults: a handler that guessed its event bus or its metric namespace would
// publish somewhere nobody is reading, and look like it worked.
const (
	EnvProject     = "PROJECT_NAME"
	EnvEnvironment = "ENVIRONMENT"
	EnvEventBus    = "EVENT_BUS_NAME"
	EnvEventSource = "EVENT_SOURCE"
	EnvNamespace   = "METRIC_NAMESPACE"
)

// ConfigFromEnv reads the handler's configuration from the environment, failing
// if any of it is missing.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Project:     os.Getenv(EnvProject),
		Environment: os.Getenv(EnvEnvironment),
		EventBus:    os.Getenv(EnvEventBus),
		EventSource: os.Getenv(EnvEventSource),
		Namespace:   os.Getenv(EnvNamespace),
	}

	missing := []string{}
	for name, value := range map[string]string{
		EnvProject:     cfg.Project,
		EnvEnvironment: cfg.Environment,
		EnvEventBus:    cfg.EventBus,
		EnvEventSource: cfg.EventSource,
		EnvNamespace:   cfg.Namespace,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing environment variables: %v", missing)
	}
	return cfg, nil
}

// New builds a Handler wired to real AWS clients, accepting the given kinds.
//
// It is called once per cold start, from the command's main, so the clients (and
// their credential caches) are reused across invocations.
func New(ctx context.Context, log *slog.Logger, accepts ...Kind) (*Handler, error) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		return nil, err
	}

	// Adaptive retry backs off under throttling and retries transient errors.
	// EC2's Describe APIs and CloudWatch's PutMetricData are both throttled per
	// account, and an interruption is exactly when a fleet-wide event storm makes
	// that likely.
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRetryMode(aws.RetryModeAdaptive),
		config.WithRetryMaxAttempts(5),
	)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return &Handler{
		Cfg:     cfg,
		EC2:     ec2.NewFromConfig(awsCfg),
		Events:  eventbridge.NewFromConfig(awsCfg),
		Metrics: cloudwatch.NewFromConfig(awsCfg),
		Log:     log,
		Accepts: accepts,
	}, nil
}
