// Package observability is the platform's single, shared answer to one question:
// when something goes wrong at 3am, what does the operator have to work with?
//
// # Why this package exists
//
// Every integration in this repository already logs. n8n, OpenClaw, the inference
// plane, the loop controller — each emits structured lines, each times its work,
// each classifies its errors. That was deliberate, and it was not enough. Six
// packages logging "the same shape, roughly" is five near-misses away from a
// dashboard that cannot span them: one calls it `correlationId`, another
// `correlation_id`, a third forgets the field on the error path, and the query
// that was supposed to follow a GitHub delivery from the webhook to the pull
// request quietly returns three unrelated things.
//
// So this package is not a new logger. It is the **agreement** — the field names,
// the metric format, the redaction rule, the health contract — written once, in a
// leaf that anything may depend on and that depends on nothing of ours. The value
// is not the code; it is that there is exactly one of it.
//
// # What it is, in four parts
//
//   - [Logger] — structured JSON logging over log/slog, with the standard fields
//     ([FieldService], [FieldComponent], [FieldRequestID], [FieldExecutionID],
//     [FieldWorkflowID], and the trace ID) stamped consistently, and a redaction
//     rule that keeps secrets and repository content out of the logs by
//     construction rather than by remembering.
//   - [Emitter] — custom metrics in the CloudWatch Embedded Metric Format (EMF).
//     A metric is a specially-shaped log line; CloudWatch extracts it into a real
//     metric with no PutMetricData call and no extra IAM. That is the whole trick,
//     and it is why the application's metrics cost exactly what its logs already
//     cost. See [emf.go].
//   - [Health] — a liveness/readiness contract and an [http.Handler] that serves
//     it, so "is OpenClaw reachable?" is a probe with an answer rather than a
//     guess.
//   - Trace propagation — the AWS X-Ray trace ID, read from where the Lambda
//     runtime and the load balancer put it and stamped into every log line, so a
//     log and a trace can be joined. Deep segment instrumentation is the X-Ray
//     SDK's job and its limits are documented; correlation is this package's job.
//
// # Why it is a leaf
//
// A validator that imported the thing it validates could not be used by anything
// else; the same is true of an observability layer. If internal/observability
// imported internal/workflow "just for the correlation type", then internal/llm
// could not use it without dragging the workflow engine in behind it, and the one
// package that is supposed to be usable *everywhere* would be usable almost
// nowhere. So it imports only the standard library, and
// internal/architecture_test.go fails the build if that ever stops being true —
// the same guard that protects internal/format.
package observability

import "context"

// The standard log field names. Every package that emits a line about a unit of
// work stamps the ones it knows, so a single CloudWatch Logs Insights query can
// follow that work across the whole platform. slog already owns "time", "level"
// and "msg"; these are the platform's additions.
//
// They are constants, not string literals scattered across the codebase, for the
// same reason the error *kinds* are sentinels and not messages: an alert or a
// dashboard is built on these names, and a name that can be retyped is a name that
// will be, one package at a time, until the query that spanned them no longer
// does.
const (
	// FieldService names the process emitting the line — "webhook", "workflow",
	// "loop". It is what lets one log group hold several services and still be
	// filterable down to one.
	FieldService = "service"

	// FieldComponent names the part within a service — "engine", "controller",
	// "health". Finer than service, coarser than the message.
	FieldComponent = "component"

	// FieldRequestID is the ID of the inbound request or invocation. For a Lambda
	// it is the AWS request ID; for an HTTP handler, the request's own ID.
	FieldRequestID = "requestId"

	// FieldExecutionID is the ID of a long-running unit of work — an OpenClaw
	// execution, an n8n execution. It is how a line emitted minutes apart is
	// recognised as being about the same run.
	FieldExecutionID = "executionId"

	// FieldWorkflowID identifies the workflow or its logical name. Paired with the
	// correlation ID, it answers "which pipeline, for which event?".
	FieldWorkflowID = "workflowId"

	// FieldCorrelationID is the platform's cross-service thread, derived stably
	// from the originating event (see internal/workflow). It is the single most
	// useful field in an incident: given a GitHub delivery ID, it finds every line
	// the platform emitted because of it.
	FieldCorrelationID = "correlationId"

	// FieldTraceID is the AWS X-Ray trace ID, when one is present. It is what joins
	// a log line to a trace, so a slow request in a dashboard and its explanation
	// in the logs are one click apart rather than one investigation apart.
	FieldTraceID = "traceId"

	// FieldError and FieldErrorKind carry a failure and its *class*. The kind is a
	// stable token ("timeout", "unauthorized"), which an alert can match without
	// pattern-matching an error message someone will reword next week.
	FieldError     = "error"
	FieldErrorKind = "errorKind"
)

// Fields is the correlation context for a unit of work: the handful of IDs that,
// stamped on every line, let a query reassemble a story from a log group.
//
// It is carried on a [context.Context] rather than passed to every call, because
// the whole point is that a function deep in the stack — one that has a context
// but never heard of a GitHub delivery — still logs the correlation ID without
// being asked to thread it through. Empty fields are simply omitted; there is no
// "unknown" noise in the output.
type Fields struct {
	Service       string
	Component     string
	RequestID     string
	ExecutionID   string
	WorkflowID    string
	CorrelationID string
	TraceID       string
}

// merge returns a copy of f with any non-empty field from o overriding it. It is
// how a nested scope adds its component without discarding the correlation IDs
// the outer scope established.
func (f Fields) merge(o Fields) Fields {
	if o.Service != "" {
		f.Service = o.Service
	}
	if o.Component != "" {
		f.Component = o.Component
	}
	if o.RequestID != "" {
		f.RequestID = o.RequestID
	}
	if o.ExecutionID != "" {
		f.ExecutionID = o.ExecutionID
	}
	if o.WorkflowID != "" {
		f.WorkflowID = o.WorkflowID
	}
	if o.CorrelationID != "" {
		f.CorrelationID = o.CorrelationID
	}
	if o.TraceID != "" {
		f.TraceID = o.TraceID
	}
	return f
}

// attrs renders the non-empty fields as the alternating key/value slice slog
// wants. The order is fixed so two lines about the same work are byte-comparable
// on their correlation columns.
func (f Fields) attrs() []any {
	var a []any
	add := func(k, v string) {
		if v != "" {
			a = append(a, k, v)
		}
	}
	add(FieldService, f.Service)
	add(FieldComponent, f.Component)
	add(FieldCorrelationID, f.CorrelationID)
	add(FieldWorkflowID, f.WorkflowID)
	add(FieldExecutionID, f.ExecutionID)
	add(FieldRequestID, f.RequestID)
	add(FieldTraceID, f.TraceID)
	return a
}

// fieldsKey is unexported so nothing outside this package can put a value under it
// and collide with, or spoof, the correlation context.
type fieldsKey struct{}

// WithFields attaches (or extends) the correlation [Fields] on a context. It
// merges rather than replaces, so a handler can set the correlation ID once and a
// callee can add its component without erasing what the handler established.
func WithFields(ctx context.Context, f Fields) context.Context {
	existing, _ := ctx.Value(fieldsKey{}).(Fields)
	return context.WithValue(ctx, fieldsKey{}, existing.merge(f))
}

// FieldsFrom returns the correlation [Fields] carried on a context, if any. The
// trace ID is filled in from the ambient X-Ray context when the context does not
// already carry one, so a log line is joined to its trace even if nobody stamped
// the ID explicitly.
func FieldsFrom(ctx context.Context) Fields {
	f, _ := ctx.Value(fieldsKey{}).(Fields)
	if f.TraceID == "" {
		f.TraceID = TraceIDFrom(ctx)
	}
	return f
}
