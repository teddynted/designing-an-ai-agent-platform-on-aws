// Package ollama talks to a self-hosted Ollama instance. It is one implementation of
// llm.Provider, and the only code in this repository that knows Ollama exists.
//
// # The boundary
//
// Ollama's deployment — the instance, the GPU, the models on disk — lives in the
// ollama-on-aws repository. This one owns the contract with it, which for once is not
// ours to define: Ollama has a published HTTP API and we speak it.
//
//	POST /api/generate   single-shot prompt → completion (streaming or not)
//	POST /api/chat       messages → completion
//	GET  /api/tags       what models are on this box
//
// This milestone does not pull models, build them, or manage the GPU. It calls an
// instance that already exists, and it tells you clearly when the model you asked for
// is not on it.
//
// # Two things about inference that are not like the rest of the platform
//
// **A retry is safe here.** Milestone 5's retry could run a workflow twice; Milestone
// 6's could open a second pull request and spend real money. Generation has no side
// effects: it reads a prompt and produces tokens. Retrying costs compute and nothing
// else. After two milestones of being careful, that is a relief — and it is worth
// being explicit about, because the instinct built up over those milestones is now
// wrong.
//
// **Except once a stream has started.** If a stream fails after the caller has already
// seen tokens, retrying would hand them a second beginning, silently glued onto the
// first. So the retry loop covers the request and the response headers, and stops the
// moment the first token escapes. See [Client.Stream].
//
// # And the timeout that actually works
//
// A total timeout is nearly useless for inference. Set it long enough for a legitimate
// slow generation on a CPU and it will wait just as patiently for a model that hung
// instantly. The useful question is not "has this finished?" but "has it produced a
// single token in the last thirty seconds?" — and only a stream can answer it.
package ollama

import (
	"bufio"
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
	"sync/atomic"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

const userAgent = "aiap-platform/llm (+https://github.com/teddynted/designing-an-ai-agent-platform-on-aws)"

// Client is an llm.Provider backed by Ollama.
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
		cfg: cfg,
		log: log,
		// No timeout on the client itself: inference deadlines are per-request, and a
		// client-wide timeout would apply to a stream that is legitimately running for
		// minutes.
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

// Name implements llm.Provider.
func (c *Client) Name() string { return "ollama" }

// Capabilities implements llm.Provider.
//
// Local is the field that matters, and it is true: the prompt does not leave the
// network. For a platform whose prompts are full of somebody's source code, that is
// not a performance characteristic — it is the reason to run this at all.
func (c *Client) Capabilities() llm.Capabilities {
	return llm.Capabilities{
		Local:            true,
		Streaming:        true,
		MaxContextTokens: c.cfg.ContextTokens,
		// Zero: a local model has no per-token price. The cost is the instance, and it
		// is paid whether or not a token is generated — which is exactly the trade a
		// router (Milestone 10) will have to weigh against a hosted provider's bill.
		// --- Milestone 9: what this model cannot do --------------------------
		//
		// False, and stated rather than omitted.
		//
		// Ollama can technically pass a tool schema to some models, and a 3B model handed
		// one will produce something that LOOKS like a tool call. That is precisely the
		// problem. The failure mode of a weak model given a capability it does not really
		// have is not an error — it is confident, well-formed, invented output, which is
		// the same trap as silent truncation (see llm.ErrContextExceeded) in different
		// clothes.
		//
		// So the platform declares it false and REFUSES, rather than asking and hoping.
		// If that is ever wrong for a specific, tested model on a specific box, it is one
		// line to change — and it should be changed deliberately, by someone who has
		// actually watched that model use a tool correctly, twenty times in a row.
		Tools:            false,
		StructuredOutput: false,
		Reasoning:        false,

		CostPer1MInputTokensUSD:  0,
		CostPer1MOutputTokensUSD: 0,
	}
}

// --- the wire ---------------------------------------------------------------

type generateRequest struct {
	Model     string         `json:"model"`
	Prompt    string         `json:"prompt,omitempty"`
	System    string         `json:"system,omitempty"`
	Messages  []chatMessage  `json:"messages,omitempty"`
	Stream    bool           `json:"stream"`
	KeepAlive string         `json:"keep_alive,omitempty"`
	Options   requestOptions `json:"options"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type requestOptions struct {
	Temperature float64  `json:"temperature"`
	NumPredict  int      `json:"num_predict,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Seed        int      `json:"seed,omitempty"`
}

// generateResponse is one NDJSON line of a stream, or the whole body of a
// non-streaming call. Ollama uses the same shape for both, which is a kindness.
type generateResponse struct {
	Model      string      `json:"model"`
	Response   string      `json:"response"` // /api/generate
	Message    chatMessage `json:"message"`  // /api/chat
	Done       bool        `json:"done"`
	DoneReason string      `json:"done_reason"`
	Error      string      `json:"error"`

	// Counters, present on the final chunk. Durations are nanoseconds.
	PromptEvalCount int   `json:"prompt_eval_count"`
	EvalCount       int   `json:"eval_count"`
	EvalDuration    int64 `json:"eval_duration"`
	LoadDuration    int64 `json:"load_duration"`
	TotalDuration   int64 `json:"total_duration"`
}

// token returns the text this chunk carries, from whichever field the endpoint used.
func (r generateResponse) token() string {
	if r.Response != "" {
		return r.Response
	}
	return r.Message.Content
}

type tagsResponse struct {
	Models []struct {
		Name       string    `json:"name"`
		Size       int64     `json:"size"`
		ModifiedAt time.Time `json:"modified_at"`
		Details    struct {
			Family            string `json:"family"`
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

// --- models -----------------------------------------------------------------

// Models implements llm.Provider: what is actually on this box.
func (c *Client) Models(ctx context.Context) ([]llm.Model, error) {
	var models []llm.Model

	_, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, _ int) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		res, err := c.send(ctx, http.MethodGet, "/api/tags", nil)
		if err != nil {
			return err
		}
		defer httpx.Drain(res, c.cfg.MaxResponseBytes)

		raw, err := httpx.ReadBounded(res.Body, c.cfg.MaxResponseBytes)
		if err != nil {
			return fmt.Errorf("%w: reading models: %v", llm.ErrInvalidResponse, err)
		}
		if err := c.checkStatus(res, raw); err != nil {
			return err
		}

		var body tagsResponse
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("%w: /api/tags is not a model list: %s", llm.ErrInvalidResponse, httpx.Snippet(raw))
		}

		models = make([]llm.Model, 0, len(body.Models))
		for _, m := range body.Models {
			models = append(models, llm.Model{
				Name:          m.Name,
				Family:        m.Details.Family,
				ParameterSize: m.Details.ParameterSize,
				Quantization:  m.Details.QuantizationLevel,
				SizeBytes:     m.Size,
				ModifiedAt:    m.ModifiedAt,
			})
		}
		return nil
	})

	return models, err
}

// --- generation -------------------------------------------------------------

// Generate implements llm.Provider: run an inference and return the whole completion.
//
// It uses a non-streaming call, which means it is bounded by a TOTAL timeout — and a
// total timeout cannot tell a slow model from a hung one. Prefer [Client.Stream] for
// anything that might take a while, which is most things.
func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	var out llm.Response

	attempts, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, _ int) error {
		ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()

		res, err := c.send(ctx, http.MethodPost, c.path(req), c.body(req, false))
		if err != nil {
			return err
		}
		defer httpx.Drain(res, c.cfg.MaxResponseBytes)

		raw, err := httpx.ReadBounded(res.Body, c.cfg.MaxResponseBytes)
		if err != nil {
			return c.readError(ctx, err)
		}
		if err := c.checkStatus(res, raw); err != nil {
			return err
		}

		var body generateResponse
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("%w: not a completion: %s", llm.ErrInvalidResponse, httpx.Snippet(raw))
		}
		if body.Error != "" {
			return c.bodyError(body.Error)
		}

		out = toResponse(body, body.token())
		return nil
	})

	out.Attempts = attempts
	return out, err
}

// Stream implements llm.Provider.
//
// # Retries stop at the first token
//
// The retry loop wraps the request and the response headers. Once a single token has
// been handed to the sink, `emitted` is set, and any subsequent failure is returned as
// [llm.ErrStreamBroken] — which the retry policy does not retry.
//
// It has to work that way. The sink is a *side effect*: the caller may have written
// those tokens to a terminal, a websocket, or a file. Retrying would produce a second
// beginning, appended to the first, and the result would look like the model lost its
// mind rather than like the network dropped.
//
// # And the stall timer
//
// A timer is armed before the read and reset on every token. If it fires, the request
// context is cancelled and the read fails — and because we set `stalled` first, the
// failure is reported as [llm.ErrStalled] rather than as a generic cancellation. That
// distinction is the whole reason this exists: "the model went quiet" is actionable,
// "context canceled" is not.
func (c *Client) Stream(ctx context.Context, req llm.Request, sink llm.Sink) (llm.Response, error) {
	var out llm.Response
	var emitted atomic.Bool

	attempts, err := httpx.Do(ctx, c.policy(), func(ctx context.Context, _ int) error {
		if emitted.Load() {
			// Defensive: httpx will not retry a broken stream, but if the policy ever
			// changed, resending here would double-emit into the caller's sink.
			return fmt.Errorf("%w: refusing to resend a stream that has already produced output", llm.ErrStreamBroken)
		}

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		res, err := c.send(ctx, http.MethodPost, c.path(req), c.body(req, true))
		if err != nil {
			return err
		}
		defer httpx.Drain(res, c.cfg.MaxResponseBytes)

		// An error status still has a body worth reading — it is where Ollama puts
		// "model not found".
		if res.StatusCode >= 400 {
			raw, _ := httpx.ReadBounded(res.Body, c.cfg.MaxResponseBytes)
			return c.checkStatus(res, raw)
		}

		var stalled atomic.Bool
		stall := time.AfterFunc(c.cfg.IdleTimeout, func() {
			stalled.Store(true)
			cancel() // makes the in-flight read fail
		})
		defer stall.Stop()

		var content strings.Builder
		var final generateResponse

		// Ollama streams NDJSON: one JSON object per line. A scanner is the right tool,
		// but its default 64 KiB line budget is not — a single chunk carrying a large
		// token run would silently end the stream early.
		scanner := bufio.NewScanner(res.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), int(min64(c.cfg.MaxResponseBytes, 8<<20)))

		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}

			var chunk generateResponse
			if err := json.Unmarshal(line, &chunk); err != nil {
				return c.streamError(emitted.Load(),
					fmt.Errorf("%w: a stream chunk is not JSON: %s", llm.ErrInvalidResponse, httpx.Snippet(line)))
			}
			if chunk.Error != "" {
				return c.streamError(emitted.Load(), c.bodyError(chunk.Error))
			}

			// A token arrived: the model is alive. Give it another idle window.
			stall.Reset(c.cfg.IdleTimeout)

			if token := chunk.token(); token != "" {
				content.WriteString(token)
				emitted.Store(true)
				if err := sink(llm.Chunk{Content: token}); err != nil {
					// The CALLER stopped it. That is not a failure of ours, and it must not
					// be retried — they asked us to stop.
					return fmt.Errorf("%w: %v", context.Canceled, err)
				}
			}

			if chunk.Done {
				final = chunk
				_ = sink(llm.Chunk{Done: true})
				break
			}
		}

		// A stall goes through streamError like any other mid-flight failure.
		//
		// That matters, and getting it wrong is exactly the bug this comment now guards:
		// a stall BEFORE the first token is safely retryable (the model was loading and
		// wedged; nobody has seen anything). A stall AFTER tokens have escaped is NOT —
		// the caller already has half an answer, and retrying would hand them a second
		// beginning. It is the same rule as a broken stream, because it IS a broken
		// stream; it just broke by going quiet rather than by dropping.
		//
		// The cause survives the wrapping, so errors.Is still finds ErrStalled and the
		// log still says "the model went quiet" rather than something about resending.
		if err := scanner.Err(); err != nil {
			if stalled.Load() {
				return c.streamError(emitted.Load(),
					fmt.Errorf("%w: no token for %s (the model may be swapping, or wedged)",
						llm.ErrStalled, c.cfg.IdleTimeout))
			}
			return c.streamError(emitted.Load(), c.readError(ctx, err))
		}
		if stalled.Load() {
			return c.streamError(emitted.Load(),
				fmt.Errorf("%w: no token for %s", llm.ErrStalled, c.cfg.IdleTimeout))
		}
		if !final.Done {
			// The stream ended without a done marker: the connection dropped mid-answer.
			// If we have already emitted tokens the caller has a partial answer, and a
			// retry would append a second one.
			return c.streamError(emitted.Load(),
				fmt.Errorf("%w: the stream ended without finishing", llm.ErrInvalidResponse))
		}

		out = toResponse(final, content.String())
		return nil
	})

	out.Attempts = attempts
	return out, err
}

// streamError makes a failure terminal once output has escaped.
//
// It wraps BOTH errors, deliberately: ErrStreamBroken is the consequence (we must not
// retry, because the caller has half an answer) and the underlying error is the cause
// (the model went quiet, the connection dropped). A caller needs both — the first to
// know not to retry, the second to know what actually went wrong — and %v would have
// thrown the cause away, leaving a log line that says only "the stream broke" when it
// could have said "the model stalled".
func (c *Client) streamError(emitted bool, err error) error {
	if !emitted {
		return err // nothing was sent to the caller; a retry is clean
	}
	return fmt.Errorf("%w: %w", llm.ErrStreamBroken, err)
}

// --- plumbing ---------------------------------------------------------------

func (c *Client) path(req llm.Request) string {
	if len(req.Messages) > 0 {
		return "/api/chat"
	}
	return "/api/generate"
}

// body builds the request. It applies the configured defaults for anything the caller
// left unset, so a caller who does not care about temperature does not have to have an
// opinion about it.
func (c *Client) body(req llm.Request, stream bool) []byte {
	model := req.Model
	if model == "" {
		model = c.cfg.Model
	}
	maxTokens := req.Options.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.cfg.MaxTokens
	}
	temperature := req.Options.Temperature
	if temperature == 0 {
		temperature = c.cfg.Temperature
	}

	body := generateRequest{
		Model:     model,
		Stream:    stream,
		KeepAlive: c.cfg.KeepAlive,
		Options: requestOptions{
			Temperature: temperature,
			NumPredict:  maxTokens,
			Stop:        req.Options.Stop,
			Seed:        req.Options.Seed,
		},
	}

	if len(req.Messages) > 0 {
		if req.System != "" {
			body.Messages = append(body.Messages, chatMessage{Role: string(llm.RoleSystem), Content: req.System})
		}
		for _, m := range req.Messages {
			body.Messages = append(body.Messages, chatMessage{Role: string(m.Role), Content: m.Content})
		}
	} else {
		body.Prompt = req.Prompt
		body.System = req.System
	}

	payload, _ := json.Marshal(body)
	return payload
}

// send performs one HTTP request.
func (c *Client) send(ctx context.Context, method, path string, payload []byte) (*http.Response, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("%w: building request: %v", llm.ErrInvalidRequest, err)
	}
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("User-Agent", userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Ollama has no auth of its own; a token only exists when it is behind a proxy
	// that does.
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, c.readError(ctx, err)
	}
	return res, nil
}

// checkStatus turns a non-2xx into one of llm's errors.
func (c *Client) checkStatus(res *http.Response, raw []byte) error {
	if res.StatusCode < 400 {
		return nil
	}

	// Ollama puts a human-readable reason in the body, and the most common one by far
	// is a model that has not been pulled.
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)

	switch {
	case res.StatusCode == http.StatusNotFound || strings.Contains(strings.ToLower(body.Error), "not found"):
		return c.bodyError(body.Error)
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		// Without the body: a proxy's auth error can echo the credential it rejected.
		return fmt.Errorf("%w: the proxy in front of Ollama answered %d", llm.ErrUnavailable, res.StatusCode)
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		err := fmt.Errorf("%w: Ollama answered %d: %s", llm.ErrUnavailable, res.StatusCode, httpx.Snippet(raw))
		if delay := httpx.ParseRetryAfter(res.Header.Get("Retry-After")); delay > 0 {
			return httpx.RetryAfterError{Err: err, Delay: delay}
		}
		return err
	default:
		return fmt.Errorf("%w: Ollama answered %d: %s", llm.ErrInvalidRequest, res.StatusCode, httpx.Snippet(raw))
	}
}

// bodyError classifies an error Ollama reported in a body.
func (c *Client) bodyError(msg string) error {
	if strings.Contains(strings.ToLower(msg), "not found") {
		// The single most common failure in practice, and the fix is one command — so
		// the error had better say which one.
		return fmt.Errorf("%w: %s. Pull it on the Ollama host: ollama pull %s",
			llm.ErrModelNotFound, msg, c.cfg.Model)
	}
	return fmt.Errorf("%w: Ollama reported: %s", llm.ErrInvalidResponse, msg)
}

// readError classifies a transport failure.
func (c *Client) readError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: no answer within the budget (a non-streaming generation cannot tell a slow model from a hung one — prefer streaming)",
			llm.ErrTimeout)
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %v", context.Canceled, err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", llm.ErrTimeout, err)
	}
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return fmt.Errorf("%w: TLS verification failed: %v", llm.ErrInvalidResponse, err)
	}
	return fmt.Errorf("%w: %v", llm.ErrUnavailable, err)
}

// policy is the retry policy for one HTTP call.
func (c *Client) policy() httpx.Policy {
	return httpx.Policy{
		Attempts:  c.cfg.RetryAttempts,
		Delay:     c.cfg.RetryDelay,
		Retryable: retryable,
		Sleep:     c.sleep,
		Jitter:    c.jitter,
	}
}

// retryable.
//
// Note what IS here that was not in the last two milestones: a timeout and a stall are
// both retryable, because generation has no side effects. Nothing is duplicated by
// asking again; the worst case is that we spend the compute twice.
//
// And note what is NOT here: ErrStreamBroken. A stream that failed after emitting
// tokens has already handed the caller half an answer, and a retry would hand them a
// second beginning.
func retryable(err error) bool {
	switch {
	case errors.Is(err, llm.ErrStreamBroken):
		return false
	case errors.Is(err, llm.ErrUnavailable), errors.Is(err, llm.ErrTimeout), errors.Is(err, llm.ErrStalled):
		return true
	default:
		return false
	}
}

// toResponse maps Ollama's final chunk onto ours, computing the number that matters.
func toResponse(body generateResponse, content string) llm.Response {
	usage := llm.Usage{
		PromptTokens:     body.PromptEvalCount,
		CompletionTokens: body.EvalCount,
		LoadDuration:     time.Duration(body.LoadDuration),
		EvalDuration:     time.Duration(body.EvalDuration),
	}
	// tokens/sec: the single most diagnostic number in the whole integration. Below
	// about ten, you are on a CPU, and everything the platform does with this model is
	// about to take minutes instead of seconds.
	if body.EvalDuration > 0 && body.EvalCount > 0 {
		usage.TokensPerSecond = float64(body.EvalCount) / (float64(body.EvalDuration) / float64(time.Second))
	}

	reason := body.DoneReason
	if reason == "" {
		reason = "stop"
	}

	return llm.Response{
		Model:        body.Model,
		Content:      content,
		Usage:        usage,
		FinishReason: reason,
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
