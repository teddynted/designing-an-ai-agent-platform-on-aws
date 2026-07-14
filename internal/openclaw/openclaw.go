// Package openclaw talks to a self-hosted OpenClaw instance over HTTP. It is one
// implementation of agent.Runtime, and the only code in this repository that knows
// OpenClaw exists.
//
// # The boundary, and the contract
//
// OpenClaw's deployment — its compute, its sandbox, its credentials, its version —
// lives in the openclaw-on-aws repository, which owns all of it. This package owns
// the *contract*, and the contract is this repository's to define. From the
// repository scope:
//
//	"Component deployments live in their own repositories… This repository defines
//	 the contracts between them; it does not deploy them."
//
// So this package states what the platform requires of an OpenClaw deployment:
//
//	POST   /v1/executions            submit a task            → 202 + execution
//	GET    /v1/executions/{id}       where is it now          → execution
//	GET    /v1/executions/{id}/result  what did it produce    → result
//	POST   /v1/executions/{id}/cancel  stop spending          → 202
//
// That is a small, boring, honest HTTP surface, and it is deliberately shaped the
// way a *slow* thing must be shaped. It is written down here because the other
// repository has to implement it, and a contract nobody wrote down is a contract
// both sides think the other one owns.
//
// # Why submit is fast and everything else polls
//
// An agent run takes minutes to hours. Nothing in the platform may block on it: not
// a Lambda, not an HTTP handler, and above all not a webhook that GitHub is timing.
// So Submit returns an execution ID as soon as OpenClaw has accepted the work, and
// the waiting happens somewhere durable — which is n8n, which has wait nodes and
// survives restarts. See agent.Service.Wait.
//
// # The agent's output is untrusted
//
// The agent reads a repository. On any public repository that content is
// attacker-influenced, and the agent has a shell. Whatever it produces is about to
// become a pull request or a published post — so its output is treated here as what
// it is: *input from an untrusted source*. It is bounded, checked for valid UTF-8,
// and scanned for credential-shaped strings before this package will hand it over.
package openclaw

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/agent"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
)

// Headers on every request. They are part of the contract with openclaw-on-aws.
const (
	// HeaderIdempotencyKey lets OpenClaw recognise a resubmission of work it has
	// already accepted. Without it, a Submit that times out on the way back — and
	// is therefore retried — starts a SECOND agent, which costs money and can open a
	// second pull request. Same hazard as Milestone 5, higher stakes: an n8n retry
	// wastes a webhook, an agent retry wastes a model.
	HeaderIdempotencyKey = "X-Idempotency-Key"

	// HeaderCorrelationID follows the GitHub delivery all the way down.
	HeaderCorrelationID = "X-Correlation-Id"

	// HeaderWorkflowExecutionID ties the agent run to the n8n run that asked for it,
	// so a pull request can be traced back through the workflow to the commit.
	HeaderWorkflowExecutionID = "X-Workflow-Execution-Id"
)

const userAgent = "aiap-platform/agent (+https://github.com/teddynted/designing-an-ai-agent-platform-on-aws)"

// Client is an agent.Runtime backed by OpenClaw.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger

	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client (tests point it at an httptest server).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithSleep replaces the retry backoff sleep, so tests run instantly.
func WithSleep(f func(context.Context, time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// WithJitter replaces the backoff jitter, so tests are deterministic.
func WithJitter(f func(time.Duration) time.Duration) Option {
	return func(c *Client) { c.jitter = f }
}

// New builds a Client.
func New(cfg Config, log *slog.Logger, opts ...Option) (*Client, error) {
	transport, err := transportFor(cfg)
	if err != nil {
		return nil, err
	}
	c := &Client{
		cfg:    cfg,
		log:    log,
		http:   &http.Client{Transport: transport},
		sleep:  httpx.SleepCtx,
		jitter: httpx.FullJitter,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func transportFor(cfg Config) (http.RoundTripper, error) {
	if cfg.CACertPath == "" {
		return http.DefaultTransport, nil
	}
	pem, err := os.ReadFile(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrConfig, EnvCACert, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: %s contains no usable certificates", ErrConfig, cfg.CACertPath)
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return t, nil
}

// Name implements agent.Runtime.
func (c *Client) Name() string { return "openclaw" }

// Tasks implements agent.Runtime.
func (c *Client) Tasks() []agent.TaskType { return c.cfg.Tasks() }

// submitBody is the wire contract for a submission. Flat and explicit: an agent
// implementation reads named fields, not a webhook payload it has to spelunk.
type submitBody struct {
	IdempotencyKey      string            `json:"idempotencyKey"`
	CorrelationID       string            `json:"correlationId"`
	WorkflowExecutionID string            `json:"workflowExecutionId,omitempty"`
	Agent               string            `json:"agent"`
	TaskType            agent.TaskType    `json:"taskType"`
	Task                agent.Task        `json:"task"`
	Attempt             int               `json:"attempt"`
	SubmittedAt         string            `json:"submittedAt"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

// executionBody is OpenClaw's description of an execution.
//
// It echoes the correlation chain back to us, and that is part of the contract
// rather than a nicety. Submit knows the chain because it sent it — but Status and
// Result are called LATER, often by a different process (an n8n poll, an operator
// with a CLI), which has only an execution ID. If OpenClaw did not hand the chain
// back, the log line that says "the agent finished" would be the one line that
// could not be traced to the GitHub delivery that caused it.
type executionBody struct {
	ID                  string  `json:"id"`
	Agent               string  `json:"agent"`
	Status              string  `json:"status"`
	TaskType            string  `json:"taskType"`
	CorrelationID       string  `json:"correlationId"`
	WorkflowExecutionID string  `json:"workflowExecutionId"`
	Steps               int     `json:"steps"`
	CostUSD             float64 `json:"costUsd"`
	Error               string  `json:"error"`
	SubmittedAt         string  `json:"submittedAt"`
	StartedAt           string  `json:"startedAt"`
	FinishedAt          string  `json:"finishedAt"`
}

// resultBody is what a finished execution produced.
type resultBody struct {
	executionBody
	Output struct {
		Content   string `json:"content"`
		Artifacts []struct {
			Path        string `json:"path"`
			ContentType string `json:"contentType"`
			Bytes       int64  `json:"bytes"`
			URI         string `json:"uri"`
		} `json:"artifacts"`
	} `json:"output"`
}

// Submit implements agent.Runtime: hand the task to OpenClaw and return as soon as
// it has accepted it. It does not wait for the agent.
func (c *Client) Submit(ctx context.Context, req agent.Request) (agent.Execution, error) {
	name, ok := c.cfg.AgentFor(req.Task.Type)
	if !ok {
		return agent.Execution{}, fmt.Errorf("%w: %q", agent.ErrUnknownTask, req.Task.Type)
	}

	// Apply the configured defaults to anything the caller did not specify. A task
	// without limits is not allowed to exist — see agent.Request.Validate — but a
	// caller that only cares about one of them should not have to state all three.
	task := req.Task
	task.Limits = c.limits(task.Limits)

	key := req.IdempotencyKey()
	var exec agent.Execution

	attempts, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, attempt int) error {
		payload, err := json.Marshal(submitBody{
			IdempotencyKey:      key,
			CorrelationID:       req.CorrelationID,
			WorkflowExecutionID: req.WorkflowExecutionID,
			Agent:               name,
			TaskType:            task.Type,
			Task:                task,
			Attempt:             attempt,
			SubmittedAt:         time.Now().UTC().Format(time.RFC3339),
			Metadata:            req.Metadata,
		})
		if err != nil {
			return fmt.Errorf("%w: encoding request: %v", agent.ErrInvalidRequest, err)
		}

		raw, err := c.do(ctx, http.MethodPost, "/v1/executions", payload, req)
		if err != nil {
			return err
		}

		var body executionBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("%w: submit answered with something that is not an execution: %s",
				agent.ErrInvalidResponse, httpx.Snippet(raw))
		}
		if body.ID == "" {
			// An accepted submission with no execution ID is unusable: we could never
			// poll it, cancel it, or find out what it did. Better to fail now than to
			// return a handle to nothing.
			return fmt.Errorf("%w: OpenClaw accepted the task but returned no execution ID",
				agent.ErrInvalidResponse)
		}
		exec = toExecution(body)
		return nil
	})

	exec.Attempts = attempts
	if err != nil {
		return exec, err
	}
	return exec, nil
}

// Status implements agent.Runtime.
func (c *Client) Status(ctx context.Context, id string) (agent.Execution, error) {
	if strings.TrimSpace(id) == "" {
		return agent.Execution{}, fmt.Errorf("%w: no execution ID", agent.ErrInvalidRequest)
	}

	var exec agent.Execution
	attempts, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, _ int) error {
		raw, err := c.do(ctx, http.MethodGet, "/v1/executions/"+id, nil, agent.Request{})
		if err != nil {
			return err
		}
		var body executionBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("%w: status is not an execution: %s", agent.ErrInvalidResponse, httpx.Snippet(raw))
		}
		exec = toExecution(body)
		return nil
	})
	exec.Attempts = attempts
	return exec, err
}

// Result implements agent.Runtime: fetch what a finished execution produced, and
// refuse to pass on anything we should not.
func (c *Client) Result(ctx context.Context, id string) (agent.Result, error) {
	if strings.TrimSpace(id) == "" {
		return agent.Result{}, fmt.Errorf("%w: no execution ID", agent.ErrInvalidRequest)
	}

	var result agent.Result
	_, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, _ int) error {
		raw, err := c.do(ctx, http.MethodGet, "/v1/executions/"+id+"/result", nil, agent.Request{})
		if err != nil {
			return err
		}
		var body resultBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("%w: result is not a result: %s", agent.ErrInvalidResponse, httpx.Snippet(raw))
		}

		exec := toExecution(body.executionBody)
		if !exec.Status.Terminal() {
			// Asking for the result of a running execution is a caller bug, not a
			// transport failure, and retrying it just asks again.
			return fmt.Errorf("%w: %s is %s", agent.ErrStillRunning, id, exec.Status)
		}

		// THE IMPORTANT LINE. The agent read a repository we may not control and its
		// output is about to become a pull request. Validate it before anyone sees it.
		output, err := validateOutput(body, c.cfg.Limits.MaxOutputBytes)
		if err != nil {
			return err
		}

		result = agent.Result{Execution: exec, Output: output}
		return nil
	})
	return result, err
}

// Cancel implements agent.Runtime. It exists because an agent that has gone wrong
// is still spending money, and "wait for it to hit its limits" is not an acceptable
// answer to that.
func (c *Client) Cancel(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: no execution ID", agent.ErrInvalidRequest)
	}
	_, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, _ int) error {
		_, err := c.do(ctx, http.MethodPost, "/v1/executions/"+id+"/cancel", []byte(`{}`), agent.Request{})
		return err
	})
	return err
}

// limits fills in the configured defaults for anything the caller left unset.
func (c *Client) limits(l agent.Limits) agent.Limits {
	if l.MaxSteps <= 0 {
		l.MaxSteps = c.cfg.Limits.MaxSteps
	}
	if l.MaxDuration <= 0 {
		l.MaxDuration = c.cfg.Limits.MaxDuration
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = c.cfg.Limits.MaxOutputBytes
	}
	// The wire form. OpenClaw is told the budget explicitly; it is not asked to
	// infer it, and it is not trusted to have a sensible default of its own.
	l.MaxDurationSeconds = int(l.MaxDuration.Seconds())
	return l
}

// policy is the retry policy for a single HTTP call.
func (c *Client) policy() httpx.Policy {
	return httpx.Policy{
		Attempts:  c.cfg.RetryAttempts,
		Delay:     c.cfg.RetryDelay,
		Retryable: retryable,
		Sleep:     c.sleep,
		Jitter:    c.jitter,
	}
}

// retryable is deliberately a short list.
//
// Note what is NOT here: agent.ErrAgentFailed. An agent that ran and failed must
// never be retried by an HTTP client — it has already spent the money, and it may
// have already opened the pull request. Re-running it is a decision for a human or
// for n8n's error path.
func retryable(err error) bool {
	return errors.Is(err, agent.ErrUnavailable) || errors.Is(err, agent.ErrTimeout)
}

// do performs one HTTP call and classifies everything that can go wrong.
func (c *Client) do(ctx context.Context, method, path string, payload []byte, req agent.Request) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("%w: building request: %v", agent.ErrInvalidRequest, err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set(c.cfg.AuthHeader, c.authValue())
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.CorrelationID != "" {
		httpReq.Header.Set(HeaderCorrelationID, req.CorrelationID)
		httpReq.Header.Set(HeaderIdempotencyKey, req.IdempotencyKey())
	}
	if req.WorkflowExecutionID != "" {
		httpReq.Header.Set(HeaderWorkflowExecutionID, req.WorkflowExecutionID)
	}

	res, err := c.http.Do(httpReq)
	if err != nil {
		return nil, transportError(err)
	}
	defer httpx.Drain(res, c.cfg.MaxResponseBytes)

	raw, err := httpx.ReadBounded(res.Body, c.cfg.MaxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: reading response: %v", agent.ErrInvalidResponse, err)
	}

	return c.interpret(res, raw)
}

// authValue formats the credential for the configured header. A token in an
// Authorization header conventionally carries a scheme; in a custom header it does
// not, and sending "Bearer …" to something expecting a raw key fails in a way that
// looks exactly like a wrong token.
func (c *Client) authValue() string {
	if strings.EqualFold(c.cfg.AuthHeader, "Authorization") && !strings.Contains(c.cfg.Token, " ") {
		return "Bearer " + c.cfg.Token
	}
	return c.cfg.Token
}

// interpret turns an HTTP response into bytes we trust, or one of agent's errors.
func (c *Client) interpret(res *http.Response, raw []byte) ([]byte, error) {
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		// Deliberately without the body: an auth error page can echo back the
		// credential it rejected, and that would put the token in our logs.
		return nil, fmt.Errorf("%w: OpenClaw answered %d", agent.ErrUnauthorized, res.StatusCode)

	case res.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", agent.ErrNotFound, httpx.Snippet(raw))

	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		err := fmt.Errorf("%w: OpenClaw answered %d: %s",
			agent.ErrUnavailable, res.StatusCode, httpx.Snippet(raw))
		// If it told us when to come back, believe it — it knows more about its own
		// load than our exponent does.
		if delay := httpx.ParseRetryAfter(res.Header.Get("Retry-After")); delay > 0 {
			return nil, httpx.RetryAfterError{Err: err, Delay: delay}
		}
		return nil, err

	case res.StatusCode >= 400:
		return nil, fmt.Errorf("%w: OpenClaw answered %d: %s",
			agent.ErrInvalidRequest, res.StatusCode, httpx.Snippet(raw))
	}

	if len(bytes.TrimSpace(raw)) == 0 {
		// A 202 with no body is a legitimate answer to a cancel.
		return []byte(`{}`), nil
	}
	if ct := res.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		// A 200 with an HTML body is a proxy or a login page answering instead of
		// OpenClaw. Treating it as success means submitting agents into a void.
		return nil, fmt.Errorf("%w: expected JSON, got %q: %s",
			agent.ErrInvalidResponse, ct, httpx.Snippet(raw))
	}
	return raw, nil
}

// transportError classifies a failure that happened before we got a response.
//
// The distinction it draws is the one that matters: a refused connection means the
// agent certainly did not start; a timeout means it may be running right now, with
// the answer lost on the way back. Only one of those is safe to be relaxed about.
func transportError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: OpenClaw did not answer in time (the execution may still have been created)", agent.ErrTimeout)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %v", context.Canceled, err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", agent.ErrTimeout, err)
	}
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		// Not "unavailable": something answered. We simply did not believe it was
		// OpenClaw, and that is not a thing to retry into.
		return fmt.Errorf("%w: TLS verification failed: %v", agent.ErrInvalidResponse, err)
	}
	return fmt.Errorf("%w: %v", agent.ErrUnavailable, err)
}

// toExecution maps OpenClaw's view of an execution onto ours.
func toExecution(b executionBody) agent.Execution {
	return agent.Execution{
		ID:                  b.ID,
		Agent:               b.Agent,
		Status:              toStatus(b.Status),
		TaskType:            agent.TaskType(b.TaskType),
		CorrelationID:       b.CorrelationID,
		WorkflowExecutionID: b.WorkflowExecutionID,
		Steps:               b.Steps,
		Cost:                b.CostUSD,
		Error:               b.Error,
		SubmittedAt:         parseTime(b.SubmittedAt),
		StartedAt:           parseTime(b.StartedAt),
		FinishedAt:          parseTime(b.FinishedAt),
	}
}

// toStatus maps OpenClaw's status strings onto ours.
//
// An unrecognised status becomes "running", NOT "succeeded" or "failed". That is
// the safe direction: treating an unknown state as terminal would either discard a
// live execution or invent a result for one that has not produced any. Treating it
// as running means we keep polling, and the execution's own limits stop it.
func toStatus(s string) agent.Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "queued", "pending", "accepted":
		return agent.StatusQueued
	case "running", "in_progress", "in-progress":
		return agent.StatusRunning
	case "succeeded", "success", "completed", "complete":
		return agent.StatusSucceeded
	case "failed", "error":
		return agent.StatusFailed
	case "cancelled", "canceled":
		return agent.StatusCancelled
	case "timed-out", "timedout", "timeout", "expired":
		return agent.StatusTimedOut
	default:
		return agent.StatusRunning
	}
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
