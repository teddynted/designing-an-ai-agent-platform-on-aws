package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// SecretsAPI is the slice of the Secrets Manager client this package uses. It exists
// so the secret fetch is testable, and so a test never has to reach a real secret.
type SecretsAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// New builds a Handler wired to real AWS clients, fetching the shared secret from
// Secrets Manager.
//
// # Why the secret is fetched here, once, at cold start
//
// It is fetched in New — which the command's main calls once, before lambda.Start — so
// it is read once per cold start and then reused across every invocation that container
// serves. Fetching it per request would add a Secrets Manager round trip (and its
// latency, and its throttling limit) to every webhook, to re-read a value that does not
// change. And it is fetched from Secrets Manager rather than read from an environment
// variable because a secret in a Lambda's env is readable by anyone with
// lambda:GetFunctionConfiguration — the secret manager exists precisely so the secret
// lives behind its own, narrower permission.
//
// If the secret cannot be fetched, New fails and the cold start fails. That is correct:
// a webhook endpoint that cannot verify signatures must not come up, because an endpoint
// that fails open — accepting unverified requests — is worse than one that is down.
func New(ctx context.Context, log *slog.Logger) (*Handler, error) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		return nil, err
	}

	secretARN := os.Getenv(EnvSecretARN)
	if secretARN == "" {
		return nil, fmt.Errorf("%s is not set", EnvSecretARN)
	}

	// Adaptive retry so a throttled EventBridge under a burst of pushes backs off rather
	// than failing the delivery outright — a throttle is exactly when a redelivery-worthy
	// 500 would otherwise fire.
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRetryMode(aws.RetryModeAdaptive),
		config.WithRetryMaxAttempts(5),
	)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	secret, err := fetchSecret(ctx, secretsmanager.NewFromConfig(awsCfg), secretARN)
	if err != nil {
		return nil, err
	}
	cfg.Secret = secret

	log.Info("webhook handler ready", "config", cfg.Redacted())

	return &Handler{
		Cfg:    cfg,
		Events: eventbridge.NewFromConfig(awsCfg),
		Log:    log,
	}, nil
}

// fetchSecret reads the shared secret value. A secret can be stored as a plain string
// or as JSON (Secrets Manager's own generator produces JSON); this reads the plain
// string form, which is what the webhook stack creates. The value is returned, never
// logged.
func fetchSecret(ctx context.Context, api SecretsAPI, arn string) (string, error) {
	out, err := api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(arn),
	})
	if err != nil {
		return "", fmt.Errorf("fetching webhook secret %s: %w", arn, err)
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", fmt.Errorf("webhook secret %s is empty", arn)
	}
	return *out.SecretString, nil
}
