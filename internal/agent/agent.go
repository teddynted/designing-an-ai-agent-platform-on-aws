// Package agent is the platform's boundary with whatever runtime executes its
// autonomous tasks. Today that runtime is OpenClaw; nothing in this package knows
// that.
//
// # Orchestration is not execution
//
// Milestone 5 gave the platform an orchestrator (n8n): the thing that decides
// *what happens, in what order, and what to do when a step fails*. This milestone
// gives it an *executor*: the thing that actually does an open-ended piece of work
// — read this repository, understand it, write a draft.
//
// Those are different jobs and they fail differently. An orchestrator's steps are
// short, deterministic, and safe to retry. An agent's run is long, non-deterministic,
// expensive, and emphatically NOT safe to retry: it has a shell, it makes commits,
// it costs money per token, and running it twice can open two pull requests.
//
// Keeping them separate is the whole design. n8n knows the shape of the pipeline;
// it does not know how to be an agent. OpenClaw knows how to be an agent; it does
// not know what the pipeline is. Neither is in the other's code.
//
// # The shape that follows from "slow"
//
// An n8n webhook returns in milliseconds. An agent run takes minutes to hours.
// That single fact reshapes the contract:
//
//	Submit(…)  → an execution ID, immediately        (fast, retryable)
//	Status(id) → where it is now                     (cheap, pollable)
//	Result(id) → what it produced, once terminal     (once)
//	Cancel(id) → stop burning money                  (because it costs money)
//
// [Service.Execute] does submit-then-wait for callers that can afford to block,
// and its documentation says loudly where that is a mistake: **never hold a Lambda
// or an HTTP handler open for an agent run.** Waiting is n8n's job — it has wait
// nodes, and it is already durable. That is precisely why the platform has an
// orchestrator at all.
//
// # The agent is a deputy
//
// Milestone 1 wrote down that "OpenClaw holds a shell — its credentials, network
// egress, and filesystem are the security boundary, not the prompt." This package
// is where that stops being a slogan.
//
// The agent reads a repository. On a public repository, or one with an outside
// contributor, that content is **attacker-influenced**. It can contain text that
// reads like an instruction. So:
//
//   - Instructions come from the *platform*, never from the repository.
//   - The agent's OUTPUT is untrusted input to everything downstream. It is
//     validated, bounded, and scanned for credentials before this package will
//     hand it to anyone, because the next stop is a pull request or a blog post.
//
// See package openclaw for where that is enforced.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
)

// ErrRetriesExhausted is re-exported from the shared transport so that callers of
// this package never have to import httpx to classify a failure. The boundary
// should not leak the mechanism used to cross it.
var ErrRetriesExhausted = httpx.ErrRetriesExhausted

// Errors the platform can act on, whatever the runtime. They exist so a caller can
// decide what to *do* — fail the workflow, alert, wait longer, ask a human —
// without parsing an HTTP status or a vendor's error string.
var (
	// ErrUnknownTask means no agent is registered for that task type. A
	// configuration error, and a local one: it fails before anything is submitted.
	ErrUnknownTask = errors.New("no agent registered for this task")

	// ErrInvalidRequest means the request could not be submitted as it stands.
	// Retrying it unchanged will fail identically.
	ErrInvalidRequest = errors.New("invalid agent request")

	// ErrUnavailable means the runtime could not be reached. The work almost
	// certainly did not start — the safest failure there is.
	ErrUnavailable = errors.New("agent runtime unavailable")

	// ErrTimeout means the runtime did not answer in time. On a Submit this is
	// dangerous: the execution may have been created anyway, with the answer lost
	// on the way back. That is what the idempotency key is for.
	ErrTimeout = errors.New("agent runtime timed out")

	// ErrUnauthorized means our credentials were rejected. A human must fix it.
	ErrUnauthorized = errors.New("agent runtime rejected our credentials")

	// ErrInvalidResponse means the runtime answered with something we cannot trust.
	ErrInvalidResponse = errors.New("invalid response from agent runtime")

	// ErrNotFound means the execution does not exist — a bad ID, or an execution
	// old enough to have been reaped.
	ErrNotFound = errors.New("execution not found")

	// ErrAgentFailed means the agent RAN and failed. This is the only error that
	// says anything about the *work*; every other one is about the plumbing. It is
	// never retried: retrying re-runs an agent that has already spent your money.
	ErrAgentFailed = errors.New("agent execution failed")

	// ErrExecutionTimeout means the agent exceeded the limits we gave it and was
	// stopped. It is a *result*, not a transport failure: the agent had its chance.
	ErrExecutionTimeout = errors.New("agent execution exceeded its limits")

	// ErrCancelled means someone stopped it.
	ErrCancelled = errors.New("agent execution cancelled")

	// ErrOutputRejected means the agent produced something we will not pass on:
	// too large, not valid UTF-8, or carrying what looks like a credential. The
	// agent's output is untrusted input to whatever publishes it.
	ErrOutputRejected = errors.New("agent output rejected")

	// ErrStillRunning is returned by Result for an execution that has not finished.
	ErrStillRunning = errors.New("execution has not finished")
)

// TaskType is what we want done — "repo-analysis", "blog-draft". It is NOT an
// agent name: which agent performs it is configuration, and the caller has no
// business knowing.
//
// Adding a task type is a constant here and one entry in configuration. It is
// never a new client, a new interface, or a new retry policy — which is the test of
// whether this integration is actually reusable.
type TaskType string

const (
	// TaskRepoAnalysis reads a repository and describes what it is and how it works.
	TaskRepoAnalysis TaskType = "repo-analysis"
	// TaskArchitectureSummary summarises the architecture.
	TaskArchitectureSummary TaskType = "architecture-summary"
	// TaskBlogDraft drafts a technical post from a repository and its recent changes.
	TaskBlogDraft TaskType = "blog-draft"
	// TaskDocsGeneration writes or updates documentation.
	TaskDocsGeneration TaskType = "docs-generation"
	// TaskReleaseNotes turns a diff between two tags into release notes.
	TaskReleaseNotes TaskType = "release-notes"
	// TaskCodeAnalysis reviews code for a specific question.
	TaskCodeAnalysis TaskType = "code-analysis"
	// TaskPRSummary summarises a pull request.
	TaskPRSummary TaskType = "pr-summary"
	// TaskContentGeneration is the generic one, for content that is not a blog post.
	TaskContentGeneration TaskType = "content-generation"
)

// Repository is what the agent is being pointed at.
type Repository struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	Branch        string `json:"branch,omitempty"`
	CommitSHA     string `json:"commitSha,omitempty"`
	CommitMessage string `json:"commitMessage,omitempty"`
}

// Limits bound what an agent may spend before it is stopped.
//
// They are not a nicety. An autonomous agent in a loop is a machine for converting
// money into tokens, and the failure mode of "it kept trying" is a bill. Every
// execution carries limits; there is no way to submit one without them, because
// the default of "unlimited" is a default nobody should be able to choose by
// forgetting.
type Limits struct {
	// MaxSteps bounds the agent's reasoning loop.
	MaxSteps int `json:"maxSteps"`
	// MaxDuration is wall-clock. The runtime stops the agent when it expires.
	MaxDuration time.Duration `json:"-"`
	// MaxDurationSeconds is the wire form.
	MaxDurationSeconds int `json:"maxDurationSeconds"`
	// MaxOutputBytes bounds what it may hand back.
	MaxOutputBytes int64 `json:"maxOutputBytes"`
}

// Task is the work itself.
type Task struct {
	Type TaskType `json:"type"`

	// Instructions come from the PLATFORM — from a workflow, a template, an
	// operator. They never come from the repository the agent is reading.
	//
	// That is the security boundary. Repository content is attacker-influenced on
	// any public repo, and it can contain text shaped like an instruction. The
	// agent may *read* it; it must never be *told what to do* by it.
	Instructions string `json:"instructions"`

	Repository Repository `json:"repository"`

	// Parameters are task-specific knobs: a tone, a target file, a model hint.
	Parameters map[string]string `json:"parameters,omitempty"`

	Limits Limits `json:"limits"`
}

// Request submits one task.
type Request struct {
	Task Task

	// CorrelationID follows the GitHub delivery all the way down: webhook → n8n →
	// agent. It is what makes "why did this pull request appear?" answerable.
	CorrelationID string

	// WorkflowExecutionID is n8n's own ID for the run that asked for this. It is
	// the other half of the chain: with it, an agent execution can be traced back
	// to the workflow step that caused it, and n8n's UI can be opened at the run.
	WorkflowExecutionID string

	// Metadata is free-form context. It is sent as-is, so no secrets.
	Metadata map[string]string
}

// Validate reports whether the request can be submitted at all.
func (r Request) Validate() error {
	if strings.TrimSpace(string(r.Task.Type)) == "" {
		return fmt.Errorf("%w: no task type", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.Task.Instructions) == "" {
		// An agent with no instructions is an agent with a shell and its own ideas.
		return fmt.Errorf("%w: task has no instructions", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.Task.Repository.URL) == "" {
		return fmt.Errorf("%w: task has no repository URL", ErrInvalidRequest)
	}
	if strings.TrimSpace(r.CorrelationID) == "" {
		// Without one there is no idempotency key, and a retried submit becomes a
		// second agent run that nobody can detect.
		return fmt.Errorf("%w: no correlation ID", ErrInvalidRequest)
	}
	if r.Task.Limits.MaxSteps <= 0 || r.Task.Limits.MaxDuration <= 0 {
		return fmt.Errorf("%w: an execution must have limits (steps and duration)", ErrInvalidRequest)
	}
	return nil
}

// IdempotencyKey is stable for a given correlation and task, by construction: the
// same workflow step, retried, produces the same key. Anything random here would
// look sophisticated and defeat the entire purpose — the point is precisely that a
// retry is recognisable as a retry.
func (r Request) IdempotencyKey() string {
	return string(r.Task.Type) + ":" + r.CorrelationID
}

// Status is where an execution is.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	// StatusTimedOut means the agent hit the limits we gave it and was stopped.
	StatusTimedOut Status = "timed-out"
)

// Terminal reports whether the execution is finished, one way or another. Polling
// stops here; anything else is still costing money.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusTimedOut:
		return true
	default:
		return false
	}
}

// Execution is a handle on a run, as the runtime last described it.
type Execution struct {
	// ID is the runtime's own identifier. It is what you paste into OpenClaw to see
	// what the agent actually did, so it goes in every log line about this run.
	ID string `json:"id"`

	// Agent is which agent the runtime chose for the task type.
	Agent string `json:"agent"`

	TaskType            TaskType `json:"taskType"`
	Status              Status   `json:"status"`
	CorrelationID       string   `json:"correlationId"`
	WorkflowExecutionID string   `json:"workflowExecutionId,omitempty"`

	// Steps and Cost are what the run has spent. They are reported even on failure,
	// because "it failed after 40 steps and $1.80" is a different problem from "it
	// failed immediately", and only one of them is a bug in the prompt.
	Steps int     `json:"steps,omitempty"`
	Cost  float64 `json:"costUsd,omitempty"`

	SubmittedAt time.Time `json:"submittedAt,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	FinishedAt  time.Time `json:"finishedAt,omitempty"`

	// Attempts is how many times we had to ask to get the submission in.
	Attempts int `json:"attempts,omitempty"`

	// Error is the runtime's explanation, when it failed.
	Error string `json:"error,omitempty"`
}

// Duration is how long the agent ran, once it has finished.
func (e Execution) Duration() time.Duration {
	if e.StartedAt.IsZero() || e.FinishedAt.IsZero() {
		return 0
	}
	return e.FinishedAt.Sub(e.StartedAt)
}

// Artifact is a file the agent produced — a draft, a diagram, a patch.
type Artifact struct {
	Path        string `json:"path"`
	ContentType string `json:"contentType,omitempty"`
	Bytes       int64  `json:"bytes,omitempty"`
	// URI is where it was stored (S3, usually). The content itself is NOT inlined
	// here: an agent's output can be megabytes, and a result is not a place to put
	// megabytes.
	URI string `json:"uri,omitempty"`
}

// Output is what the agent produced.
//
// It is **untrusted**. The agent read a repository that may not be ours, and its
// output is about to become a pull request or a blog post. Everything here has
// been size-bounded, checked for valid UTF-8, and scanned for credential-shaped
// strings before it reached you — see package openclaw.
type Output struct {
	// Content is the primary text result: the draft, the summary, the notes.
	Content string `json:"content,omitempty"`
	// Artifacts are files it wrote.
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// Raw is the runtime's untouched response, for a caller that wants more. It is
	// never logged wholesale.
	Raw json.RawMessage `json:"-"`
}

// Result is a finished execution and what it produced.
type Result struct {
	Execution
	Output Output `json:"output"`
}

// Runtime executes agent tasks. It is the seam: OpenClaw implements it today, and
// something else could tomorrow without a single caller changing.
//
// An implementation owns its transport, its authentication, its retries, and the
// translation of its failures into this package's errors. It does NOT own logging,
// validation, or correlation — the [Service] does those once, so every runtime gets
// them identically and none can forget.
type Runtime interface {
	// Name identifies the runtime in logs: "openclaw".
	Name() string

	// Tasks lists the task types this runtime has an agent for. The Service checks
	// against it, so an unregistered task fails immediately and locally rather than
	// as a 404 from somewhere else.
	Tasks() []TaskType

	// Submit starts an execution and returns as soon as the runtime has accepted
	// it. It must NOT wait for the agent to finish.
	Submit(ctx context.Context, req Request) (Execution, error)

	// Status reports where an execution is. It is cheap and safe to call often.
	Status(ctx context.Context, executionID string) (Execution, error)

	// Result returns what a terminal execution produced. It returns
	// ErrStillRunning for one that has not finished.
	Result(ctx context.Context, executionID string) (Result, error)

	// Cancel stops an execution. It exists because an agent that has gone wrong is
	// still spending money, and "wait for it to hit its limits" is not an
	// acceptable answer to that.
	Cancel(ctx context.Context, executionID string) error
}
