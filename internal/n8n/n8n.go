// Package n8n talks to a self-hosted n8n instance over HTTP. It is one
// implementation of workflow.Engine, and the only place in this repository that
// knows n8n exists.
//
// # The boundary
//
// n8n's deployment — its EC2/ECS footprint, its database, its queue mode, its
// version, its backups — lives in the self-hosted-n8n-on-aws repository, which
// owns all of it. This package owns the *contract*: the URL we call, the header
// we authenticate with, the JSON we send, and the errors we understand. Nothing
// here provisions anything, and nothing here should ever start to.
//
// # The one hard problem: a retry is not free
//
// Triggering a workflow is not a read. If we send a trigger, the network eats the
// response, and we retry, n8n may run the workflow TWICE — and a blog-generating
// workflow that runs twice opens two pull requests.
//
// A timeout is the dangerous case, and it is worth being precise about why: a
// timeout tells you that no answer arrived, and nothing whatsoever about whether
// the request did. The work may be running right now.
//
// So every request carries an idempotency key derived from the event's own ID
// (X-Idempotency-Key, and the same value inside the body). It is stable across
// retries by construction: the same GitHub delivery always produces the same key.
// That makes the *transport* at-least-once and lets n8n make the *execution*
// effectively-once — but only if the workflow on the other side actually checks
// the key. This package cannot enforce that, and it is the single most important
// thing to get right on the n8n side. It is documented in WORKFLOWS.md, and it is
// the first thing to check when something has happened twice.
package n8n

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/workflow"
)

// Headers we set on every request. They are read by the n8n workflow, so they are
// part of the contract and must not be renamed casually.
const (
	// HeaderIdempotencyKey lets n8n recognise a retry of a trigger it has already
	// seen. See the package documentation.
	HeaderIdempotencyKey = "X-Idempotency-Key"

	// HeaderCorrelationID ties the execution to the event across both systems'
	// logs, so a GitHub delivery ID is enough to find everything.
	HeaderCorrelationID = "X-Correlation-Id"

	// HeaderEventType and HeaderRepository let an n8n workflow route or filter
	// without parsing the body.
	HeaderEventType  = "X-Event-Type"
	HeaderRepository = "X-Repository"
)

const userAgent = "aiap-platform/workflow (+https://github.com/teddynted/designing-an-ai-agent-platform-on-aws)"

// Client is a workflow.Engine backed by n8n.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger
	// sleep is injectable so a retry test does not actually wait.
	sleep func(context.Context, time.Duration) error
	// jitter is injectable so a backoff test is deterministic.
	jitter func(time.Duration) time.Duration
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client. Tests use it to point at an httptest
// server; a caller might use it to add a proxy or a tracer.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithSleep replaces the backoff sleep. Tests use it to run instantly.
func WithSleep(f func(context.Context, time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// WithJitter replaces the backoff jitter. Tests use it to make delays exact.
func WithJitter(f func(time.Duration) time.Duration) Option {
	return func(c *Client) { c.jitter = f }
}

// New builds a Client from a Config.
func New(cfg Config, log *slog.Logger, opts ...Option) (*Client, error) {
	transport, err := transportFor(cfg)
	if err != nil {
		return nil, err
	}

	c := &Client{
		cfg: cfg,
		log: log,
		http: &http.Client{
			// The per-attempt timeout lives on the request context, not here, so that
			// a caller's own deadline can shorten it but never lengthen it.
			Transport: transport,
		},
		sleep:  httpx.SleepCtx,
		jitter: httpx.FullJitter,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// transportFor builds the HTTP transport, trusting a private CA if one is
// configured. There is no option to skip verification — see EnvCACert.
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

// Name implements workflow.Engine.
func (c *Client) Name() string { return "n8n" }

// Workflows implements workflow.Engine.
func (c *Client) Workflows() []string { return c.cfg.Names() }

// body is the JSON we POST. It is a flat, explicit contract — an n8n workflow
// reads {{$json.event.commitSha}}, not a GitHub payload it has to spelunk.
type body struct {
	// IdempotencyKey is repeated in the body as well as the header because n8n
	// workflows find body fields much easier to work with than headers, and the
	// key is useless if the workflow cannot conveniently reach it.
	IdempotencyKey string            `json:"idempotencyKey"`
	CorrelationID  string            `json:"correlationId"`
	Workflow       string            `json:"workflow"`
	RequestedAt    string            `json:"requestedAt"`
	Attempt        int               `json:"attempt"`
	Event          workflow.Event    `json:"event"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// response is the part of n8n's answer we model. n8n's webhook node returns
// whatever the workflow's "Respond to Webhook" node produces, so these fields are
// all optional and we tolerate their absence — but we do read an error out of the
// shape n8n itself uses when a workflow throws.
type response struct {
	ExecutionID string `json:"executionId"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	Error       string `json:"error"`
	Code        int    `json:"code"`
}

// Trigger implements workflow.Engine: send the request, retry what is worth
// retrying, and translate everything that can go wrong into workflow's errors.
func (c *Client) Trigger(ctx context.Context, req workflow.Request) (workflow.Result, error) {
	path, ok := c.cfg.Workflows[req.Workflow]
	if !ok {
		// The Service checks this first, so reaching here means the engine and the
		// Service disagree — worth an explicit error rather than a nil-map read.
		return workflow.Result{}, fmt.Errorf("%w: %q", workflow.ErrUnknownWorkflow, req.Workflow)
	}

	// The event payload is the one thing here this platform did not author, so it
	// is the one thing we sanitise: strip anything that looks like a secret, and
	// refuse to forward something enormous.
	event := req.Event
	sanitised, err := sanitisePayload(event.Payload, c.cfg.MaxPayloadBytes)
	if err != nil {
		return workflow.Result{}, err
	}
	event.Payload = sanitised

	key := idempotencyKey(req)
	url := c.cfg.BaseURL + path

	var result workflow.Result

	// The retry mechanics are shared with the OpenClaw integration (internal/httpx);
	// the POLICY — what is worth retrying — stays here, because only this package
	// knows that a workflow which ran and failed must never be retried.
	attempts, err := httpx.Do(ctx, httpx.Policy{
		Attempts:  c.cfg.RetryAttempts,
		Delay:     c.cfg.RetryDelay,
		Retryable: retryable,
		Sleep:     c.sleep,
		Jitter:    c.jitter,
	}, func(ctx context.Context, attempt int) error {
		payload, err := json.Marshal(body{
			IdempotencyKey: key,
			CorrelationID:  req.CorrelationID,
			Workflow:       req.Workflow,
			RequestedAt:    time.Now().UTC().Format(time.RFC3339),
			Attempt:        attempt,
			Event:          event,
			Metadata:       req.Metadata,
		})
		if err != nil {
			return fmt.Errorf("%w: encoding request: %v", workflow.ErrInvalidRequest, err)
		}

		res, err := c.attempt(ctx, url, payload, req, key, attempt)
		if err != nil {
			return err
		}
		result.Status = res.Status
		result.ExecutionID = res.ExecutionID
		result.Response = res.Response
		return nil
	})

	result.Attempts = attempts
	return result, err
}

// attemptResult is one successful round trip.
type attemptResult struct {
	Status      workflow.Status
	ExecutionID string
	Response    json.RawMessage
}

// attempt makes exactly one request. Every failure it returns is already one of
// workflow's sentinel errors.
func (c *Client) attempt(ctx context.Context, url string, payload []byte, req workflow.Request, key string, attempt int) (attemptResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return attemptResult{}, fmt.Errorf("%w: building request: %v", workflow.ErrInvalidRequest, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set(c.cfg.AuthHeader, c.cfg.Token)
	httpReq.Header.Set(HeaderIdempotencyKey, key)
	httpReq.Header.Set(HeaderCorrelationID, req.CorrelationID)
	httpReq.Header.Set(HeaderEventType, req.Event.Type)
	httpReq.Header.Set(HeaderRepository, req.Event.Repository)

	c.log.Debug("n8n request",
		"correlationId", req.CorrelationID,
		"workflow", req.Workflow,
		"attempt", attempt,
		"url", url, // the path, not the token — the token is in a header we never log
		"bytes", len(payload),
	)

	res, err := c.http.Do(httpReq)
	if err != nil {
		return attemptResult{}, transportError(ctx, err)
	}
	// Drain so the connection can be reused, and read a BOUNDED amount: an engine
	// that answers with a gigabyte must not be able to take this process down.
	// Both live in httpx now, because the OpenClaw client needs them too.
	defer httpx.Drain(res, c.cfg.MaxResponseBytes)

	raw, err := httpx.ReadBounded(res.Body, c.cfg.MaxResponseBytes)
	if err != nil {
		return attemptResult{}, fmt.Errorf("%w: reading response: %v", workflow.ErrInvalidResponse, err)
	}

	return c.interpret(res, raw)
}

// interpret turns an HTTP response into an outcome or one of workflow's errors.
func (c *Client) interpret(res *http.Response, raw []byte) (attemptResult, error) {
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		// Deliberately does not include the body: an auth error page can echo back
		// the credential it rejected, and that would put the token in our logs.
		return attemptResult{}, fmt.Errorf("%w: n8n answered %d", workflow.ErrUnauthorized, res.StatusCode)

	case res.StatusCode == http.StatusNotFound:
		// The workflow is registered here but n8n has never heard of it: the webhook
		// is not registered, or the workflow is not active. This is the most common
		// real-world failure, and it is a configuration problem, not a transient one.
		return attemptResult{}, fmt.Errorf("%w: n8n has no webhook at that path (is the workflow active?)", workflow.ErrUnknownWorkflow)

	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		err := fmt.Errorf("%w: n8n answered %d: %s",
			workflow.ErrUnavailable, res.StatusCode, httpx.Snippet(raw))
		// If n8n told us when to come back, believe it: it knows more about its own
		// load than our exponent does, and ignoring Retry-After while it is asking
		// for room is how a struggling instance gets pushed over.
		if delay := httpx.ParseRetryAfter(res.Header.Get("Retry-After")); delay > 0 {
			return attemptResult{}, httpx.RetryAfterError{Err: err, Delay: delay}
		}
		return attemptResult{}, err

	case res.StatusCode >= 400:
		return attemptResult{}, fmt.Errorf("%w: n8n answered %d: %s",
			workflow.ErrInvalidRequest, res.StatusCode, httpx.Snippet(raw))
	}

	// 2xx. An empty body is legitimate — an n8n webhook set to "respond
	// immediately" returns nothing at all — and means the trigger was accepted.
	if len(bytes.TrimSpace(raw)) == 0 {
		return attemptResult{Status: workflow.StatusAccepted}, nil
	}

	if ct := res.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		// A 200 with an HTML body is the signature of a proxy or a login page
		// answering instead of n8n. Treating it as success is how a platform ends up
		// cheerfully reporting that it triggered workflows into a void.
		return attemptResult{}, fmt.Errorf("%w: expected JSON, got %q: %s",
			workflow.ErrInvalidResponse, ct, httpx.Snippet(raw))
	}

	var parsed response
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return attemptResult{}, fmt.Errorf("%w: body is not JSON: %s",
			workflow.ErrInvalidResponse, httpx.Snippet(raw))
	}

	// n8n returns 200 with an error in the body when a workflow throws and the
	// error is caught by a "Respond to Webhook" node. A 200 is not a success.
	if parsed.Error != "" || strings.EqualFold(parsed.Status, "error") || strings.EqualFold(parsed.Status, "failed") {
		return attemptResult{}, fmt.Errorf("%w: %s", workflow.ErrWorkflowFailed,
			firstNonEmpty(parsed.Error, parsed.Message, "n8n reported a failure with no message"))
	}

	status := workflow.StatusAccepted
	if strings.EqualFold(parsed.Status, "success") || strings.EqualFold(parsed.Status, "succeeded") {
		status = workflow.StatusSucceeded
	}

	return attemptResult{
		Status:      status,
		ExecutionID: parsed.ExecutionID,
		Response:    json.RawMessage(raw),
	}, nil
}

// transportError classifies a failure that happened before we got a response.
//
// The distinction it draws is the one that matters for correctness: a timeout may
// mean the work started, while a refused connection means it certainly did not.
func transportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: no answer within the timeout (the workflow may still be running)", workflow.ErrTimeout)
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %v", context.Canceled, err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", workflow.ErrTimeout, err)
	}

	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		// Not retryable, and not "unavailable": the engine answered, we simply did
		// not believe it was the engine.
		return fmt.Errorf("%w: TLS verification failed: %v", workflow.ErrInvalidResponse, err)
	}

	return fmt.Errorf("%w: %v", workflow.ErrUnavailable, err)
}

// retryable reports whether asking again could plausibly help.
//
// It is a short list on purpose. Retrying an unauthorized request will not make
// the token valid; retrying a malformed one will not make it well-formed; and
// retrying a workflow that *ran and failed* will simply run it again, which is
// the last thing anyone wants.
func retryable(err error) bool {
	switch {
	case errors.Is(err, workflow.ErrUnavailable), errors.Is(err, workflow.ErrTimeout):
		return true
	default:
		return false
	}
}

// idempotencyKey is stable for a given event and workflow, by construction: the
// same GitHub delivery, retried by us or replayed by GitHub, produces the same
// key. Anything random here would defeat the entire purpose.
func idempotencyKey(req workflow.Request) string {
	id := req.Event.ID
	if id == "" {
		id = req.CorrelationID
	}
	return req.Workflow + ":" + id
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
