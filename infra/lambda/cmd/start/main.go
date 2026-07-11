// Command start is the Lambda that powers the scheduled EC2 instance on.
// It is invoked by the "start" EventBridge schedule; all of its behaviour lives
// in the shared ec2sched package.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/infra/lambda/internal/ec2sched"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	lambda.Start(func(ctx context.Context, _ json.RawMessage) (ec2sched.Result, error) {
		return ec2sched.Handle(ctx, ec2sched.Start)
	})
}
