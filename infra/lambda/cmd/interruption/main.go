// Command interruption is the Lambda that reacts to a Spot instance being
// reclaimed: the two-minute interruption warning, and the earlier rebalance
// recommendation that says an interruption is becoming likely.
//
// It records the event, re-publishes it on the platform's event bus, and logs
// it. It does not — and cannot — drain the instance: that happens on the
// instance itself, in the drain agent the compute stack installs. See the
// package documentation for why.
//
// All of its behaviour lives in the shared spot package.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/infra/lambda/internal/spot"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// Built once per cold start, not per invocation: an interruption is a bad
	// moment to be negotiating credentials.
	handler, err := spot.New(context.Background(), log, spot.KindInterruption, spot.KindRebalance)
	if err != nil {
		// Nothing can work without configuration, and a Lambda that starts anyway
		// would fail one event at a time instead of failing to deploy.
		log.Error("could not start the interruption handler", "error", err)
		os.Exit(1)
	}

	lambda.Start(func(ctx context.Context, event json.RawMessage) (spot.Result, error) {
		return handler.Handle(ctx, event)
	})
}
