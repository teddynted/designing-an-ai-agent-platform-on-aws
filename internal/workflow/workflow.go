// Package workflow is the platform's boundary with whatever engine runs its
// long-lived automation. Today that engine is self-hosted n8n; nothing in this
// package knows that.
//
// # Why a boundary at all
//
// The platform reacts to GitHub events by doing slow, multi-step, failure-prone
// work: read a repository, ask a model to draft a post, open a pull request,
// publish, announce. That work is an orchestration problem, and orchestration
// engines are good at it — retries, waits, human approval steps, fan-out, and a
// UI that shows you where a run got stuck at 3am.
//
// The temptation is to call n8n's webhook URL from wherever the event arrives.
// That works, and it welds the platform to n8n: every caller learns the URL
// scheme, the auth header, the retry policy, and the response shape. Replacing
// the engine — or running two during a migration — then means touching every
// caller. So the platform calls a [Service], the Service calls an [Engine], and
// n8n is one implementation of that interface.
//
// # What lives where
//
//   - This package: what a workflow *is* — its inputs, its outcome, its errors,
//     and the logging and validation every execution gets regardless of engine.
//   - Package n8n: how to actually talk to n8n over HTTP, with its auth, its
//     retries, and its quirks.
//
// The split is the point. A future engine (Step Functions, Temporal, a queue)
// implements [Engine] and nothing above it changes.
//
// # What this package deliberately does NOT do
//
// It does not deploy n8n, and it never will. n8n's infrastructure lives in the
// self-hosted-n8n-on-aws repository, which owns its deployment, its version, and
// its rollback. This repository owns the *contract* between the platform and the
// engine — the payload it sends, the auth it uses, the errors it understands.
// That boundary is written down in the repository scope, and this package is what
// it looks like in code.
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors the platform can act on, independently of which engine produced them.
//
// They exist so a caller can decide what to *do* — retry later, alert, drop the
// event, fail the webhook — without parsing an HTTP status code or a vendor's
// error string. An engine translates its own failures into these.
var (
	// ErrUnknownWorkflow means no workflow is registered under that name. It is a
	// configuration error, not a runtime one: the caller asked for something the
	// platform has never heard of.
	ErrUnknownWorkflow = errors.New("unknown workflow")

	// ErrInvalidRequest means the request could not be sent as it stands — a
	// missing workflow name, an unparseable payload. The caller is at fault, and
	// retrying it unchanged will fail identically.
	ErrInvalidRequest = errors.New("invalid workflow request")

	// ErrUnavailable means the engine could not be reached at all: connection
	// refused, DNS failure, TLS failure. The work was almost certainly not
	// started, so this is the safest failure there is.
	ErrUnavailable = errors.New("workflow engine unavailable")

	// ErrTimeout means the engine did not answer in time. Note carefully: this
	// does NOT mean the workflow did not run. The request may have arrived and be
	// executing right now, with the answer lost on the way back. See the note on
	// idempotency in the package n8n documentation.
	ErrTimeout = errors.New("workflow engine timed out")

	// ErrUnauthorized means the engine rejected our credentials. Retrying cannot
	// help, and the only fix is a human rotating a token.
	ErrUnauthorized = errors.New("workflow engine rejected our credentials")

	// ErrInvalidResponse means the engine answered with something we cannot
	// trust: not JSON, the wrong content type, or larger than we are willing to
	// read. A successful HTTP status with a garbage body is still a failure.
	ErrInvalidResponse = errors.New("invalid response from workflow engine")

	// ErrWorkflowFailed means the engine accepted the trigger and the workflow
	// itself then failed. This is the only error that says anything about the
	// *work*; every other error is about the plumbing.
	ErrWorkflowFailed = errors.New("workflow execution failed")

	// ErrRetriesExhausted means we retried a retryable failure until we ran out of
	// attempts. It always wraps the last underlying error, so errors.Is still
	// finds the cause.
	ErrRetriesExhausted = errors.New("retries exhausted")
)

// Status is the outcome of an execution, as far as the platform can tell.
type Status string

const (
	// StatusAccepted means the engine took the trigger and will run the workflow
	// asynchronously. It is the normal outcome for anything slow, and it says
	// nothing at all about whether the work will succeed — only that it started.
	StatusAccepted Status = "accepted"

	// StatusSucceeded means the engine ran the workflow synchronously and it
	// finished successfully. Only workflows that answer within the request timeout
	// can ever report this.
	StatusSucceeded Status = "succeeded"

	// StatusFailed means the engine ran the workflow and it failed.
	StatusFailed Status = "failed"
)

// Event is what happened in the outside world — the thing worth running a
// workflow about. Today that is always a GitHub event; the fields are the ones
// every workflow needs and none of them are GitHub-specific in name, so a
// GitLab or a Gitea event would fit here unchanged.
//
// The explicit fields matter. A workflow that has to reach into a raw webhook
// payload to find the commit SHA is a workflow coupled to GitHub's payload
// schema, and GitHub changes that schema whenever it likes.
type Event struct {
	// ID is the source's own identifier for this event — GitHub's delivery ID.
	// It is the basis of the correlation ID and of idempotency, so it is the most
	// important field here.
	ID string `json:"id"`

	// Type is the event type: push, pull_request, release, ...
	Type string `json:"type"`

	// Action is the sub-type, where the source has one: opened, closed, synchronize.
	Action string `json:"action,omitempty"`

	// Repository is the full name, e.g. "teddynted/designing-an-ai-agent-platform-on-aws".
	Repository string `json:"repository"`

	// RepositoryURL is where a workflow can clone or read the repository from.
	RepositoryURL string `json:"repositoryUrl"`

	// Branch is the ref the event concerns, without the refs/heads/ prefix.
	Branch string `json:"branch,omitempty"`

	// CommitSHA is the commit the event concerns.
	CommitSHA string `json:"commitSha,omitempty"`

	// CommitMessage is that commit's message. A blog-generating workflow reads it;
	// so does a release-notes workflow.
	CommitMessage string `json:"commitMessage,omitempty"`

	// Actor is the login of whoever caused the event.
	Actor string `json:"actor,omitempty"`

	// Payload is the source's original payload, forwarded verbatim so a workflow
	// can reach a field the platform did not think to model.
	//
	// It is forwarded through a redactor and a size cap, because it is the one
	// field here whose contents this platform does not control. See the n8n
	// package.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Request asks for one workflow to be run for one event.
type Request struct {
	// Workflow is the logical name — "blog-generator", not a URL. Which engine
	// runs it, and at which endpoint, is configuration and none of the caller's
	// business.
	Workflow string

	// Event is what happened.
	Event Event

	// CorrelationID ties this execution to the event that caused it, across the
	// platform's logs, the engine's logs, and CloudWatch. Leave it empty and the
	// Service derives one from the event ID — which is what you want, because
	// then the same delivery always produces the same correlation ID.
	CorrelationID string

	// Metadata is free-form context for the workflow: which environment, which
	// platform version, a feature flag. It is sent as-is, so do not put secrets
	// in it.
	Metadata map[string]string
}

// Validate reports whether the request can be sent at all.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Workflow) == "" {
		return fmt.Errorf("%w: no workflow name", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.Event.ID) == "" {
		// Without an event ID there is no correlation and no idempotency key, and
		// a retried trigger becomes a duplicate execution nobody can detect.
		return fmt.Errorf("%w: event has no ID", ErrInvalidRequest)
	}
	if len(r.Event.Payload) > 0 && !json.Valid(r.Event.Payload) {
		return fmt.Errorf("%w: event payload is not valid JSON", ErrInvalidRequest)
	}
	return nil
}

// Result is what came back.
type Result struct {
	Workflow      string        `json:"workflow"`
	CorrelationID string        `json:"correlationId"`
	Status        Status        `json:"status"`
	Duration      time.Duration `json:"-"`
	// DurationMS is the same thing in a form a log aggregator can graph.
	DurationMS int64 `json:"durationMs"`
	// Attempts is how many times we had to ask. 1 is the happy path; more than 1
	// means the engine wobbled and we absorbed it.
	Attempts int `json:"attempts"`
	// ExecutionID is the engine's own identifier for the run, when it gives one.
	// It is what you paste into the n8n UI to see what happened.
	ExecutionID string `json:"executionId,omitempty"`
	// Response is whatever the engine returned, for a caller that wants it. It is
	// never logged wholesale: it is the engine's data, not ours, and it may be
	// large.
	Response json.RawMessage `json:"-"`
}

// Engine runs workflows. It is the seam: n8n implements it today, and anything
// else could implement it tomorrow without a single caller changing.
//
// An implementation is responsible for its own transport, authentication,
// retries, and for translating its failures into this package's sentinel errors.
// It is NOT responsible for logging, validation, or correlation — the [Service]
// does those once, so that every engine gets them identically and no engine can
// forget.
type Engine interface {
	// Name identifies the engine in logs: "n8n".
	Name() string

	// Workflows lists the workflow names this engine can run. The Service checks
	// against it, so an unknown workflow fails immediately and locally rather than
	// as a 404 from somewhere else, half a second later.
	Workflows() []string

	// Trigger runs the workflow named in the request. It must honour ctx, and it
	// must return one of this package's sentinel errors, wrapped.
	Trigger(ctx context.Context, req Request) (Result, error)
}
