package observability

import (
	"context"
	"os"
	"strings"
)

// This file is the platform's whole relationship with AWS X-Ray, and it is
// deliberately narrow. It is worth being honest about the boundary.
//
// # What this does
//
// It reads the X-Ray *trace ID* from the two places AWS puts it — the Lambda
// runtime's _X_AMZN_TRACE_ID environment variable, and the X-Amzn-Trace-Id HTTP
// header a load balancer or API Gateway adds — and makes it available as
// [FieldTraceID] on every log line and metric. That single field is what joins a
// slow request in a trace to the log lines that explain it: without it, the trace
// and the logs are two separate investigations of the same incident.
//
// # What this deliberately does NOT do
//
// It does not create segments or subsegments, and it does not sample. That is the
// AWS X-Ray SDK's job (or the ADOT collector's), and reproducing a fraction of it
// here would be a liability — a half-instrumented trace is worse than none,
// because it looks complete and is not. Enabling *active tracing* on a Lambda
// (TracingConfig: Active in CloudFormation) gives you the automatic
// service-boundary segments for free; this package makes those traces
// *correlatable with the logs*, which is the part active tracing does not do.
//
// # Where tracing cannot be applied, and why
//
//   - The EC2 workloads — OpenClaw, Ollama, n8n — are deployed by their own
//     repositories. The platform can trace its *own* Lambdas and its own process
//     boundaries; it cannot instrument the inside of a service it does not deploy.
//     A trace that reaches the OpenClaw boundary and stops there is the honest
//     shape, and the correlation ID (which crosses that boundary in the request)
//     is what carries the story the rest of the way.
//   - A trace does not survive EventBridge. When the webhook Lambda publishes an
//     event and n8n later consumes it, that is an intentional decoupling (see
//     WEBHOOKS.md) — the two are different transactions, joined by the correlation
//     ID, not one trace. Pretending otherwise would draw a causal line the
//     architecture specifically does not have.

// traceKey is unexported so a trace ID can only be placed on a context through
// [WithTraceID], never spoofed from outside the package.
type traceKey struct{}

// WithTraceID stores an explicit trace ID on the context. A handler that has
// parsed the ID from a request header uses this; most callers do not need it,
// because [TraceIDFrom] falls back to the ambient Lambda environment.
func WithTraceID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, id)
}

// TraceIDFrom returns the X-Ray trace ID for this context: the one explicitly
// stored with [WithTraceID] if present, otherwise the one the Lambda runtime
// exports in _X_AMZN_TRACE_ID. It returns "" when there is no trace, which is the
// common case for a local CLI run and is not an error — the field is simply
// omitted from the line.
func TraceIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(traceKey{}).(string); ok && v != "" {
		return v
	}
	return parseTraceRoot(os.Getenv("_X_AMZN_TRACE_ID"))
}

// TraceIDFromHeader parses the Root trace ID out of an X-Amzn-Trace-Id header
// value. The header a load balancer sends looks like:
//
//	Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1
//
// and only the Root portion is the trace ID that ties everything together.
func TraceIDFromHeader(header string) string { return parseTraceRoot(header) }

// parseTraceRoot pulls "Root=..." out of an X-Ray header/env value. Both the HTTP
// header and the Lambda environment variable use the same grammar, so one parser
// serves both. An input without a Root (or an empty one) yields "", never a
// partial or a guess.
func parseTraceRoot(s string) string {
	if s == "" {
		return ""
	}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "Root="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
