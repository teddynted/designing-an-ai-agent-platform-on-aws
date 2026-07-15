// Command webhook is the Lambda GitHub calls when something happens in a repository.
//
// It sits behind a Lambda Function URL — an HTTPS endpoint with no API Gateway in
// front of it, because none is needed: authentication is the GitHub HMAC signature the
// handler verifies, not IAM, and a Function URL is the smallest thing that terminates
// TLS and hands a Go function the raw request. The URL's auth type is NONE for exactly
// that reason; "NONE" means "no AWS auth", and the real auth is in the body.
//
// All behaviour lives in the webhook package. This file adapts the Function URL's
// request and response types to the package's transport-independent [webhook.Request]
// and [webhook.Response], so the package never imports the Lambda events types and can
// be tested with plain values.
package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/textproto"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/infra/lambda/internal/webhook"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	handler, err := webhook.New(context.Background(), log)
	if err != nil {
		// A cold start that cannot build the handler — no secret, missing config — must
		// die, not serve. An endpoint that came up unable to verify signatures would
		// accept forgeries.
		log.Error("could not start the webhook handler", "error", err)
		os.Exit(1)
	}

	lambda.Start(func(ctx context.Context, req events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
		resp, _ := handler.Handle(ctx, translate(req))
		return events.LambdaFunctionURLResponse{
			StatusCode: resp.StatusCode,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       resp.Body,
		}, nil
	})
}

// translate turns a Function URL request into the handler's transport-independent form.
// The two things it must get exactly right are the raw body and case-insensitive
// headers — both of which the signature check depends on.
func translate(req events.LambdaFunctionURLRequest) webhook.Request {
	body := []byte(req.Body)
	// A Function URL base64-encodes the body only for content types it treats as binary.
	// GitHub sends application/json, so this is usually a no-op — but the signature is
	// over the RAW bytes GitHub sent, so decoding when the flag is set is not optional:
	// verifying over the base64 text would reject every such delivery.
	if req.IsBase64Encoded {
		if decoded, err := base64.StdEncoding.DecodeString(req.Body); err == nil {
			body = decoded
		}
	}
	return webhook.Request{
		Headers: headerMap(req.Headers),
		Body:    body,
	}
}

// headerMap wraps the Function URL's header map in a case-insensitive lookup. A Function
// URL lower-cases header names, so GitHub's X-GitHub-Event arrives as x-github-event;
// canonicalising on read means the package can ask for either spelling and get the same
// answer, and a test can pass canonical names without the production path diverging.
type headerMap map[string]string

func (h headerMap) Get(name string) string {
	// Try the exact key, then the lower-cased key (what a Function URL delivers), then
	// the canonical MIME form (what a test or an API Gateway might use).
	if v, ok := h[name]; ok {
		return v
	}
	if v, ok := h[strings.ToLower(name)]; ok {
		return v
	}
	if v, ok := h[textproto.CanonicalMIMEHeaderKey(name)]; ok {
		return v
	}
	return ""
}
