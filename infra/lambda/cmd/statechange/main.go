// Command statechange is the Lambda that reacts to an EC2 instance changing
// state: launched (running), stopped, or terminated.
//
// It is the other half of the interruption story. The interruption handler sees
// the warning; this one sees the consequence — and, on a Spot instance that was
// reclaimed, the terminated event is the only record that the instance ever
// existed. Keeping the two in separate functions keeps their log groups, their
// failures, and their retries separate.
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

	handler, err := spot.New(context.Background(), log, spot.KindStateChange)
	if err != nil {
		log.Error("could not start the state-change handler", "error", err)
		os.Exit(1)
	}

	lambda.Start(func(ctx context.Context, event json.RawMessage) (spot.Result, error) {
		return handler.Handle(ctx, event)
	})
}
