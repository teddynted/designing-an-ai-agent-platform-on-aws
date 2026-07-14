package n8n

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/workflow"
)

const testToken = "super-secret-token-do-not-log"

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// testConfig points at a stub n8n. Retries are instant in tests — the backoff is
// verified separately, and no test should ever sleep for real.
func testConfig(baseURL string) Config {
	return Config{
		BaseURL:          baseURL,
		Token:            testToken,
		AuthHeader:       DefaultAuthHeader,
		Workflows:        map[string]string{"blog-generator": "/webhook/blog"},
		Timeout:          2 * time.Second,
		RetryAttempts:    3,
		RetryDelay:       time.Millisecond,
		MaxPayloadBytes:  DefaultMaxPayloadBytes,
		MaxResponseBytes: DefaultMaxResponseBytes,
	}
}

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg, discardLogger(),
		// Never actually wait in a test.
		WithSleep(func(context.Context, time.Duration) error { return nil }),
		WithJitter(func(d time.Duration) time.Duration { return d }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func testRequest() workflow.Request {
	return workflow.Request{
		Workflow:      "blog-generator",
		CorrelationID: "push:12345",
		Event: workflow.Event{
			ID:            "12345",
			Type:          "push",
			Repository:    "teddynted/platform",
			RepositoryURL: "https://github.com/teddynted/platform",
			Branch:        "main",
			CommitSHA:     "abc123",
			CommitMessage: "feat: add a thing",
			Actor:         "teddynted",
		},
	}
}

func trigger(t *testing.T, c *Client, req workflow.Request) (workflow.Result, error) {
	t.Helper()
	return c.Trigger(context.Background(), req)
}

// --- the happy path ---------------------------------------------------------

func TestTriggerSendsTheContract(t *testing.T) {
	var got body
	var headers http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"executionId":"exec-99","status":"success"}`))
	}))
	defer srv.Close()

	res, err := trigger(t, newTestClient(t, testConfig(srv.URL)), testRequest())
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	if res.Status != workflow.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", res.Status)
	}
	if res.ExecutionID != "exec-99" {
		t.Errorf("ExecutionID = %q, want exec-99", res.ExecutionID)
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}

	// The body is a contract an n8n workflow reads. If these field names change,
	// somebody's workflow silently stops finding the commit.
	if got.Event.CommitSHA != "abc123" || got.Event.Repository != "teddynted/platform" {
		t.Errorf("event = %+v, want the commit and repository forwarded", got.Event)
	}
	if got.Event.Branch != "main" || got.Event.CommitMessage != "feat: add a thing" {
		t.Errorf("event = %+v, want the branch and commit message forwarded", got.Event)
	}
	if got.CorrelationID != "push:12345" {
		t.Errorf("correlationId = %q, want push:12345", got.CorrelationID)
	}

	// Authentication.
	if headers.Get(DefaultAuthHeader) != testToken {
		t.Errorf("auth header = %q, want the token", headers.Get(DefaultAuthHeader))
	}
	// Correlation and idempotency travel as headers too, so a workflow can route
	// on them without parsing a body.
	if headers.Get(HeaderCorrelationID) != "push:12345" {
		t.Errorf("%s = %q", HeaderCorrelationID, headers.Get(HeaderCorrelationID))
	}
	if headers.Get(HeaderIdempotencyKey) == "" {
		t.Error("every request must carry an idempotency key")
	}
}

// --- idempotency: the one that actually matters -----------------------------

// A retried trigger MUST carry the same idempotency key as the attempt it is
// retrying. If it does not, n8n cannot tell a retry from a new event, and a
// blog-generating workflow opens two pull requests.
func TestRetriesReuseTheSameIdempotencyKey(t *testing.T) {
	var keys []string
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get(HeaderIdempotencyKey))
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway) // retryable
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"executionId":"exec-1"}`))
	}))
	defer srv.Close()

	res, err := trigger(t, newTestClient(t, testConfig(srv.URL)), testRequest())
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
	if len(keys) != 3 {
		t.Fatalf("n8n saw %d requests, want 3", len(keys))
	}
	for i, k := range keys {
		if k != keys[0] {
			t.Fatalf("attempt %d used key %q, but attempt 1 used %q — n8n cannot deduplicate this", i+1, k, keys[0])
		}
		if k == "" {
			t.Fatal("idempotency key is empty")
		}
	}
}

// The key must be derived from the event, so that the SAME delivery replayed by
// GitHub tomorrow produces the same key — not a fresh one.
func TestIdempotencyKeyIsDerivedFromTheEvent(t *testing.T) {
	first := idempotencyKey(testRequest())
	second := idempotencyKey(testRequest())
	if first != second {
		t.Errorf("the same event produced two keys: %q and %q", first, second)
	}

	other := testRequest()
	other.Event.ID = "99999"
	if idempotencyKey(other) == first {
		t.Error("different events must not share an idempotency key")
	}

	// A different workflow for the same event is a different execution.
	sameEventOtherWorkflow := testRequest()
	sameEventOtherWorkflow.Workflow = "release-notes"
	if idempotencyKey(sameEventOtherWorkflow) == first {
		t.Error("two workflows for one event must not share an idempotency key")
	}
}

// --- retries ----------------------------------------------------------------

func TestRetryPolicy(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantCalls   int32
		wantErr     error
		description string
	}{
		{"503 is retried", http.StatusServiceUnavailable, 3, workflow.ErrUnavailable, "n8n restarting is the textbook transient failure"},
		{"429 is retried", http.StatusTooManyRequests, 3, workflow.ErrUnavailable, "backpressure, not refusal"},
		{"500 is retried", http.StatusInternalServerError, 3, workflow.ErrUnavailable, ""},
		// Retrying these cannot help, and retrying an auth failure repeatedly is
		// how you get an account locked.
		{"401 is NOT retried", http.StatusUnauthorized, 1, workflow.ErrUnauthorized, "the token will not become valid"},
		{"403 is NOT retried", http.StatusForbidden, 1, workflow.ErrUnauthorized, ""},
		{"404 is NOT retried", http.StatusNotFound, 1, workflow.ErrUnknownWorkflow, "the workflow is not active in n8n"},
		{"400 is NOT retried", http.StatusBadRequest, 1, workflow.ErrInvalidRequest, "a malformed request stays malformed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			res, err := trigger(t, newTestClient(t, testConfig(srv.URL)), testRequest())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got := atomic.LoadInt32(&calls); got != tt.wantCalls {
				t.Errorf("n8n was called %d times, want %d (%s)", got, tt.wantCalls, tt.description)
			}
			if res.Attempts != int(tt.wantCalls) {
				t.Errorf("Attempts = %d, want %d", res.Attempts, tt.wantCalls)
			}
		})
	}
}

func TestRetriesExhaustedWrapsTheCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := trigger(t, newTestClient(t, testConfig(srv.URL)), testRequest())
	if !errors.Is(err, workflow.ErrRetriesExhausted) {
		t.Errorf("error = %v, want ErrRetriesExhausted", err)
	}
	// The cause must survive the wrapping, or an alert cannot tell "n8n is down"
	// from "n8n is timing out".
	if !errors.Is(err, workflow.ErrUnavailable) {
		t.Errorf("error = %v, must still unwrap to the cause (ErrUnavailable)", err)
	}
}

func TestRecoversAfterATransientFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"executionId":"exec-2"}`))
	}))
	defer srv.Close()

	res, err := trigger(t, newTestClient(t, testConfig(srv.URL)), testRequest())
	if err != nil {
		t.Fatalf("a blip should be absorbed, got: %v", err)
	}
	if res.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", res.Attempts)
	}
	if res.Status != workflow.StatusAccepted {
		t.Errorf("Status = %q, want accepted", res.Status)
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var slept []time.Duration
	c, err := New(testConfig(srv.URL), discardLogger(),
		WithSleep(func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }),
		WithJitter(func(d time.Duration) time.Duration { return d }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := trigger(t, c, testRequest()); !errors.Is(err, workflow.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	for _, d := range slept {
		// The server asked for 2s. Our exponential backoff would have said 1ms.
		// Ignoring the server is how you finish knocking over a struggling n8n.
		if d != 2*time.Second {
			t.Errorf("waited %v, want the 2s the server asked for", d)
		}
	}
}

// --- timeouts ---------------------------------------------------------------

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Timeout = 20 * time.Millisecond
	cfg.RetryAttempts = 1 // do not retry: this test is about the classification

	_, err := trigger(t, newTestClient(t, cfg), testRequest())
	if !errors.Is(err, workflow.ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	// The message must warn that the work may have started anyway — that is the
	// whole hazard of a timeout on a non-idempotent trigger.
	if !strings.Contains(err.Error(), "may still be running") {
		t.Errorf("a timeout must say the workflow may still be running; got %q", err)
	}
}

func TestUnavailableWhenNothingIsListening(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	cfg := testConfig(url)
	cfg.RetryAttempts = 2

	_, err := trigger(t, newTestClient(t, cfg), testRequest())
	if !errors.Is(err, workflow.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

// --- responses --------------------------------------------------------------

func TestResponseHandling(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
		wantErr     error
		wantStatus  workflow.Status
	}{
		{
			name: "empty body is an accepted trigger",
			// An n8n webhook set to "respond immediately" returns nothing at all.
			status: http.StatusOK, wantStatus: workflow.StatusAccepted,
		},
		{
			name: "202 is accepted", status: http.StatusAccepted,
			contentType: "application/json", body: `{"executionId":"e1"}`,
			wantStatus: workflow.StatusAccepted,
		},
		{
			name: "a 200 with an error in the body is a FAILURE",
			// n8n answers 200 and puts the error in the body when a workflow throws.
			// Trusting the status code here is how a platform reports success while
			// nothing works.
			status: http.StatusOK, contentType: "application/json",
			body:    `{"status":"error","message":"node 'Draft' failed"}`,
			wantErr: workflow.ErrWorkflowFailed,
		},
		{
			name:   "a 200 with HTML is not n8n answering",
			status: http.StatusOK, contentType: "text/html",
			body:    `<html><body>Please log in</body></html>`,
			wantErr: workflow.ErrInvalidResponse,
		},
		{
			name:   "a 200 with unparseable JSON is not trusted",
			status: http.StatusOK, contentType: "application/json",
			body:    `{"executionId": `,
			wantErr: workflow.ErrInvalidResponse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			cfg := testConfig(srv.URL)
			cfg.RetryAttempts = 1
			res, err := trigger(t, newTestClient(t, cfg), testRequest())

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Trigger: %v", err)
			}
			if res.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", res.Status, tt.wantStatus)
			}
		})
	}
}

// An engine that answers with far more than we asked for must not be able to
// exhaust this process's memory — once per retry, no less.
func TestOversizedResponseIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"executionId":"e1","junk":"`+strings.Repeat("A", 5000)+`"}`)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.MaxResponseBytes = 128 // truncation makes it invalid JSON, which we must not trust
	cfg.RetryAttempts = 1

	if _, err := trigger(t, newTestClient(t, cfg), testRequest()); !errors.Is(err, workflow.ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse (a truncated body is not trustworthy)", err)
	}
}

// --- secrets ----------------------------------------------------------------

// The token must never reach a log line or an error string, including when n8n
// rejects it and helpfully echoes it back at us.
func TestTheTokenNeverLeaks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// A real gateway has done exactly this.
		_, _ = io.WriteString(w, `{"message":"invalid api key: `+testToken+`"}`)
	}))
	defer srv.Close()

	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c, err := New(testConfig(srv.URL), logger,
		WithSleep(func(context.Context, time.Duration) error { return nil }),
		WithJitter(func(d time.Duration) time.Duration { return d }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = trigger(t, c, testRequest())
	if !errors.Is(err, workflow.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the token leaked into the error: %q", err)
	}
	if strings.Contains(logs.String(), testToken) {
		t.Error("the token leaked into the logs")
	}
}

func TestRedactedConfigHidesTheToken(t *testing.T) {
	out := testConfig("https://n8n.example.com").Redacted()
	rendered, _ := json.Marshal(out)
	if strings.Contains(string(rendered), testToken) {
		t.Errorf("Redacted() leaked the token: %s", rendered)
	}
	if !strings.Contains(string(rendered), "set,") {
		t.Errorf("Redacted() should still say a token IS set: %s", rendered)
	}
}

// --- payload sanitisation ---------------------------------------------------

func TestSanitisePayloadRedactsCredentials(t *testing.T) {
	raw := json.RawMessage(`{
	  "repository": {"full_name": "teddynted/platform"},
	  "installation": {"access_token": "ghs_LIVE_TOKEN", "id": 42},
	  "nested": [{"client_secret": "shhh"}, {"safe": "keep me"}],
	  "github_token": "ghp_ANOTHER",
	  "tokenizer": "not a secret",
	  "sender": {"login": "teddynted"}
	}`)

	cleaned, err := sanitisePayload(raw, DefaultMaxPayloadBytes)
	if err != nil {
		t.Fatalf("sanitisePayload: %v", err)
	}
	got := string(cleaned)

	for _, secret := range []string{"ghs_LIVE_TOKEN", "shhh", "ghp_ANOTHER"} {
		if strings.Contains(got, secret) {
			t.Errorf("secret %q survived sanitisation: %s", secret, got)
		}
	}
	// The structure and the useful data must survive — a sanitiser that guts the
	// payload is a sanitiser nobody will keep enabled.
	for _, keep := range []string{"teddynted/platform", "keep me", "not a secret", `"id":42`} {
		if !strings.Contains(got, keep) {
			t.Errorf("sanitisation destroyed useful data (%q missing): %s", keep, got)
		}
	}
}

func TestSanitiseRejectsAnOversizedPayload(t *testing.T) {
	big := json.RawMessage(`{"data":"` + strings.Repeat("A", 500) + `"}`)
	if _, err := sanitisePayload(big, 100); !errors.Is(err, workflow.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
}

func TestSanitiseRejectsInvalidJSON(t *testing.T) {
	if _, err := sanitisePayload(json.RawMessage(`{"broken":`), DefaultMaxPayloadBytes); !errors.Is(err, workflow.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
}

// What actually goes over the wire must be the sanitised payload, not the
// original. It is not enough for the sanitiser to be correct in isolation.
func TestTheWireCarriesTheSanitisedPayload(t *testing.T) {
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	req := testRequest()
	req.Event.Payload = json.RawMessage(`{"installation":{"access_token":"ghs_LEAKED"}}`)

	if _, err := trigger(t, newTestClient(t, testConfig(srv.URL)), req); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if strings.Contains(string(sent), "ghs_LEAKED") {
		t.Errorf("an unsanitised secret was sent to n8n: %s", sent)
	}
	if !strings.Contains(string(sent), "REDACTED") {
		t.Errorf("the redaction marker is missing from what we sent: %s", sent)
	}
}

// --- backoff ----------------------------------------------------------------
//
// The backoff, the jitter and the Retry-After handling now live in internal/httpx,
// because the OpenClaw integration needs exactly the same mechanics and a second
// copy would drift. Their tests moved with them — see httpx_test.go. What stays
// here is the POLICY: which failures this client considers worth retrying, which is
// the part only this package can know.

// --- engine contract --------------------------------------------------------

func TestClientImplementsEngine(t *testing.T) {
	var _ workflow.Engine = (*Client)(nil)
}

func TestUnknownWorkflowNeverReachesTheNetwork(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	req := testRequest()
	req.Workflow = "does-not-exist"

	if _, err := trigger(t, newTestClient(t, testConfig(srv.URL)), req); !errors.Is(err, workflow.ErrUnknownWorkflow) {
		t.Fatalf("error = %v, want ErrUnknownWorkflow", err)
	}
	if called {
		t.Error("an unregistered workflow must fail locally, not as a 404 from n8n")
	}
}
