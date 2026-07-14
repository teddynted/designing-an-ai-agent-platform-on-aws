package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/agent"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
)

const testToken = "oc-super-secret-token-do-not-log"

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testConfig(baseURL string) Config {
	return Config{
		BaseURL:    baseURL,
		Token:      testToken,
		AuthHeader: DefaultAuthHeader,
		Agents: map[agent.TaskType]string{
			agent.TaskBlogDraft:    "writer",
			agent.TaskRepoAnalysis: "analyst",
		},
		Timeout:          2 * time.Second,
		RetryAttempts:    3,
		RetryDelay:       time.Millisecond,
		PollInterval:     time.Millisecond,
		MaxResponseBytes: DefaultMaxResponseBytes,
		Limits: agent.Limits{
			MaxSteps:       DefaultMaxSteps,
			MaxDuration:    DefaultMaxDuration,
			MaxOutputBytes: DefaultMaxOutputBytes,
		},
	}
}

func newClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg, discardLogger(),
		WithSleep(func(context.Context, time.Duration) error { return nil }),
		WithJitter(httpx.NoJitter),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func testRequest() agent.Request {
	return agent.Request{
		CorrelationID:       "push:delivery-abc-123",
		WorkflowExecutionID: "n8n-exec-42",
		Task: agent.Task{
			Type:         agent.TaskBlogDraft,
			Instructions: "Draft a technical post about the changes in this commit.",
			Repository: agent.Repository{
				Name:      "teddynted/platform",
				URL:       "https://github.com/teddynted/platform",
				Branch:    "main",
				CommitSHA: "deadbeef",
			},
			Limits: agent.Limits{MaxSteps: 10, MaxDuration: time.Minute, MaxOutputBytes: 1000},
		},
	}
}

// --- submit -----------------------------------------------------------------

func TestSubmitSendsTheContract(t *testing.T) {
	var got submitBody
	var headers http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/executions" || r.Method != http.MethodPost {
			t.Errorf("submit went to %s %s, want POST /v1/executions", r.Method, r.URL.Path)
		}
		headers = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"exec-1","agent":"writer","status":"queued"}`))
	}))
	defer srv.Close()

	exec, err := newClient(t, testConfig(srv.URL)).Submit(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if exec.ID != "exec-1" || exec.Status != agent.StatusQueued {
		t.Errorf("execution = %+v, want exec-1/queued", exec)
	}
	// Submit must return as soon as the work is ACCEPTED. If it waited for the agent,
	// nothing in the platform could call it.
	if exec.Status.Terminal() {
		t.Error("Submit returned a terminal status — it must not wait for the agent")
	}

	// The task type is mapped to an agent HERE, so the caller never names one.
	if got.Agent != "writer" {
		t.Errorf("agent = %q, want writer (chosen from the task type)", got.Agent)
	}
	if got.Task.Repository.CommitSHA != "deadbeef" || got.Task.Instructions == "" {
		t.Errorf("task = %+v, want the repository and instructions forwarded", got.Task)
	}
	// Limits must go on the wire. An agent trusted to have its own sensible default
	// is an agent that will eventually spend all night thinking.
	if got.Task.Limits.MaxSteps != 10 || got.Task.Limits.MaxDurationSeconds != 60 {
		t.Errorf("limits = %+v, want the caller's budget sent explicitly", got.Task.Limits)
	}

	if headers.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("auth header = %q, want a bearer token", headers.Get("Authorization"))
	}
	if headers.Get(HeaderCorrelationID) != "push:delivery-abc-123" {
		t.Errorf("%s = %q", HeaderCorrelationID, headers.Get(HeaderCorrelationID))
	}
	// The chain: GitHub delivery → n8n run → this agent execution.
	if headers.Get(HeaderWorkflowExecutionID) != "n8n-exec-42" {
		t.Errorf("%s = %q, want the n8n execution", HeaderWorkflowExecutionID, headers.Get(HeaderWorkflowExecutionID))
	}
	if headers.Get(HeaderIdempotencyKey) == "" {
		t.Error("every submit must carry an idempotency key")
	}
}

// The stakes are higher than Milestone 5's. A retried n8n trigger wastes a webhook;
// a retried agent submit spends money on a second model run and can open a second
// pull request.
func TestRetriedSubmitReusesTheSameIdempotencyKey(t *testing.T) {
	var keys []string
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get(HeaderIdempotencyKey))
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"exec-1","status":"queued"}`))
	}))
	defer srv.Close()

	exec, err := newClient(t, testConfig(srv.URL)).Submit(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if exec.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", exec.Attempts)
	}
	for i, k := range keys {
		if k == "" || k != keys[0] {
			t.Fatalf("attempt %d used key %q but attempt 1 used %q — OpenClaw would start a SECOND agent", i+1, k, keys[0])
		}
	}
}

func TestIdempotencyKeyIsDerivedNotGenerated(t *testing.T) {
	if a, b := testRequest().IdempotencyKey(), testRequest().IdempotencyKey(); a != b {
		t.Errorf("the same request produced two keys: %q and %q", a, b)
	}
	// A different task for the same event is a different execution.
	other := testRequest()
	other.Task.Type = agent.TaskRepoAnalysis
	if other.IdempotencyKey() == testRequest().IdempotencyKey() {
		t.Error("two task types for one event must not share a key")
	}
}

func TestSubmitRejectsAnExecutionWithNoID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"queued"}`)) // accepted, but no ID
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.RetryAttempts = 1
	// A handle to nothing: we could never poll it, cancel it, or find out what it did.
	if _, err := newClient(t, cfg).Submit(context.Background(), testRequest()); !errors.Is(err, agent.ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

func TestUnknownTaskNeverReachesTheNetwork(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	req := testRequest()
	req.Task.Type = "no-such-task"

	if _, err := newClient(t, testConfig(srv.URL)).Submit(context.Background(), req); !errors.Is(err, agent.ErrUnknownTask) {
		t.Fatalf("error = %v, want ErrUnknownTask", err)
	}
	if called {
		t.Error("an unmapped task must fail locally, not as a 404 from OpenClaw")
	}
}

// --- retry policy -----------------------------------------------------------

func TestRetryPolicy(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantCalls int32
		wantErr   error
		why       string
	}{
		{"503 is retried", http.StatusServiceUnavailable, 3, agent.ErrUnavailable, "OpenClaw restarting is transient"},
		{"429 is retried", http.StatusTooManyRequests, 3, agent.ErrUnavailable, "backpressure, not refusal"},
		{"401 is NOT retried", http.StatusUnauthorized, 1, agent.ErrUnauthorized, "the token will not become valid"},
		{"404 is NOT retried", http.StatusNotFound, 1, agent.ErrNotFound, ""},
		{"400 is NOT retried", http.StatusBadRequest, 1, agent.ErrInvalidRequest, "a malformed request stays malformed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			_, err := newClient(t, testConfig(srv.URL)).Submit(context.Background(), testRequest())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got := atomic.LoadInt32(&calls); got != tt.wantCalls {
				t.Errorf("OpenClaw was called %d times, want %d (%s)", got, tt.wantCalls, tt.why)
			}
		})
	}
}

// A submit timeout is the dangerous one: the execution may exist anyway, with the
// answer lost on the way back. The message must say so, because a human reading it
// needs to go and look before resubmitting.
func TestSubmitTimeoutSaysTheExecutionMayExist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Timeout = 20 * time.Millisecond
	cfg.RetryAttempts = 1

	_, err := newClient(t, cfg).Submit(context.Background(), testRequest())
	if !errors.Is(err, agent.ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if !strings.Contains(err.Error(), "may still have been created") {
		t.Errorf("a submit timeout must warn that the execution may exist; got %q", err)
	}
}

// --- status and result ------------------------------------------------------

func TestStatusAndResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/executions/exec-1":
			_, _ = w.Write([]byte(`{"id":"exec-1","agent":"writer","status":"running","steps":3}`))
		case "/v1/executions/exec-1/result":
			_, _ = w.Write([]byte(`{"id":"exec-1","status":"succeeded","steps":12,"costUsd":0.42,
			  "startedAt":"2026-07-14T10:00:00Z","finishedAt":"2026-07-14T10:04:00Z",
			  "output":{"content":"# A draft\n\nIt works.","artifacts":[{"path":"post.md","uri":"s3://bucket/post.md","bytes":21}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newClient(t, testConfig(srv.URL))

	exec, err := c.Status(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if exec.Status != agent.StatusRunning || exec.Steps != 3 {
		t.Errorf("execution = %+v, want running with 3 steps", exec)
	}
	if exec.Status.Terminal() {
		t.Error("running must not be terminal, or polling would stop early")
	}

	res, err := c.Result(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if res.Status != agent.StatusSucceeded || res.Cost != 0.42 || res.Steps != 12 {
		t.Errorf("result = %+v, want the cost and steps reported", res.Execution)
	}
	if !strings.Contains(res.Output.Content, "It works") {
		t.Errorf("content = %q, want the draft", res.Output.Content)
	}
	if len(res.Output.Artifacts) != 1 || res.Output.Artifacts[0].Path != "post.md" {
		t.Errorf("artifacts = %+v, want the file it wrote", res.Output.Artifacts)
	}
	if res.Duration() != 4*time.Minute {
		t.Errorf("Duration = %v, want 4m", res.Duration())
	}
}

// Asking for the result of a running execution is a caller bug, and it must not
// look like an empty success.
func TestResultOfARunningExecution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"exec-1","status":"running"}`))
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.RetryAttempts = 1
	if _, err := newClient(t, cfg).Result(context.Background(), "exec-1"); !errors.Is(err, agent.ErrStillRunning) {
		t.Fatalf("error = %v, want ErrStillRunning", err)
	}
}

// An unrecognised status must map to "running", never to a terminal state. Treating
// an unknown state as terminal would either discard a live execution or invent a
// result for one that has produced nothing.
func TestUnknownStatusIsTreatedAsRunning(t *testing.T) {
	for _, s := range []string{"", "reticulating-splines", "paused"} {
		if got := toStatus(s); got != agent.StatusRunning {
			t.Errorf("toStatus(%q) = %q, want running (the safe direction)", s, got)
		}
	}
	// And the ones we do know.
	for raw, want := range map[string]agent.Status{
		"queued": agent.StatusQueued, "in_progress": agent.StatusRunning,
		"success": agent.StatusSucceeded, "COMPLETED": agent.StatusSucceeded,
		"error": agent.StatusFailed, "canceled": agent.StatusCancelled,
		"timeout": agent.StatusTimedOut,
	} {
		if got := toStatus(raw); got != want {
			t.Errorf("toStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCancel(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusAccepted) // no body: legitimate
	}))
	defer srv.Close()

	// A 202 with an empty body must not be read as an invalid response — cancelling
	// is the one call whose answer is genuinely "yes, fine".
	if err := newClient(t, testConfig(srv.URL)).Cancel(context.Background(), "exec-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if path != "/v1/executions/exec-1/cancel" {
		t.Errorf("cancel went to %s", path)
	}
}

// --- responses we must not trust --------------------------------------------

func TestInvalidResponses(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantErr     error
	}{
		{"HTML is a proxy or a login page, not OpenClaw", "text/html", "<html>Please log in</html>", agent.ErrInvalidResponse},
		{"broken JSON is not trusted", "application/json", `{"id":`, agent.ErrInvalidResponse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			cfg := testConfig(srv.URL)
			cfg.RetryAttempts = 1
			if _, err := newClient(t, cfg).Submit(context.Background(), testRequest()); !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// --- secrets ----------------------------------------------------------------

func TestTheTokenNeverLeaks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// A real gateway has echoed the rejected credential back like this.
		_, _ = io.WriteString(w, `{"message":"invalid token: `+testToken+`"}`)
	}))
	defer srv.Close()

	var logs strings.Builder
	c, err := New(testConfig(srv.URL),
		slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		WithSleep(func(context.Context, time.Duration) error { return nil }),
		WithJitter(httpx.NoJitter),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Submit(context.Background(), testRequest())
	if !errors.Is(err, agent.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the token leaked into the error: %v", err)
	}
	if strings.Contains(logs.String(), testToken) {
		t.Error("the token leaked into the logs")
	}
}

// --- agent output is untrusted ----------------------------------------------

// THE most important test in this package.
//
// The agent read a repository (attacker-influenced on any public repo) and its
// output is about to become a pull request. A credential in it means the agent read
// a secret — and publishing the draft would exfiltrate it, with a nice title.
func TestOutputWithACredentialIsRejectedNotPublished(t *testing.T) {
	leaks := map[string]string{
		"an AWS key":       `Here is the config: AKIAIOSFODNN7EXAMPLE`,
		"a GitHub token":   "The token is ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"an Anthropic key": "key: sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
		"a private key":    "-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----",
	}

	for name, content := range leaks {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				payload, _ := json.Marshal(map[string]any{
					"id": "exec-1", "status": "succeeded",
					"output": map[string]any{"content": content},
				})
				_, _ = w.Write(payload)
			}))
			defer srv.Close()

			cfg := testConfig(srv.URL)
			cfg.RetryAttempts = 1

			res, err := newClient(t, cfg).Result(context.Background(), "exec-1")
			if !errors.Is(err, agent.ErrOutputRejected) {
				t.Fatalf("error = %v, want ErrOutputRejected — this would have been published", err)
			}
			// The error must NOT quote the secret: an error that helpfully includes the
			// leaked credential has leaked it into the logs, which is the thing we are
			// preventing.
			if strings.Contains(err.Error(), "AKIAIOSFODNN7EXAMPLE") || strings.Contains(err.Error(), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
				t.Errorf("the error quoted the secret: %v", err)
			}
			// And it must tell a human to rotate it.
			if !strings.Contains(err.Error(), "rotate") {
				t.Errorf("the error should tell someone to rotate the secret; got %q", err)
			}
			if res.Output.Content != "" {
				t.Error("no content may be returned from a rejected output")
			}
		})
	}
}

// The scanner must be narrow. One that fires on every base64-looking string gets
// switched off within a week, and a scanner that is switched off protects nothing.
func TestOrdinaryProseIsNotFlaggedAsASecret(t *testing.T) {
	fine := []string{
		"This milestone uses a token bucket to rate-limit requests.",
		"Set AWS_PROFILE and run make deploy. The password field is redacted in logs.",
		"sk-lines are used in the diagram", // short, not a key
		"# Milestone 6\n\nThe agent has a shell, so its credentials are the boundary.",
	}
	for _, s := range fine {
		if found := scanForCredentials(s); len(found) > 0 {
			t.Errorf("false positive %v on ordinary prose: %q", found, s)
		}
	}
}

func TestOversizedOutputIsRejected(t *testing.T) {
	body := resultBody{}
	body.Output.Content = strings.Repeat("A", 500)

	if _, err := validateOutput(body, 100); !errors.Is(err, agent.ErrOutputRejected) {
		t.Fatalf("error = %v, want ErrOutputRejected", err)
	}
}

func TestInvalidUTF8OutputIsRejected(t *testing.T) {
	body := resultBody{}
	// This is going into a git commit and an HTML page.
	body.Output.Content = string([]byte{0xff, 0xfe, 0xfd})

	if _, err := validateOutput(body, 1000); !errors.Is(err, agent.ErrOutputRejected) {
		t.Fatalf("error = %v, want ErrOutputRejected", err)
	}
}

func TestGoodOutputPassesThrough(t *testing.T) {
	body := resultBody{}
	body.Output.Content = "# A perfectly ordinary blog post\n\nAbout Spot instances."

	out, err := validateOutput(body, 1000)
	if err != nil {
		t.Fatalf("validateOutput: %v", err)
	}
	if !strings.Contains(out.Content, "Spot instances") {
		t.Errorf("content = %q, want it passed through intact", out.Content)
	}
}

// --- the runtime contract ---------------------------------------------------

func TestClientImplementsRuntime(t *testing.T) {
	var _ agent.Runtime = (*Client)(nil)
}

func TestDefaultAgentIsUsedOnlyWhenConfigured(t *testing.T) {
	cfg := testConfig("https://openclaw.example.com")

	if _, ok := cfg.AgentFor("unmapped-task"); ok {
		t.Error("with no default, an unmapped task must NOT silently pick an agent — that is how you get a blog post where you wanted release notes")
	}

	cfg.DefaultAgent = "generalist"
	if name, ok := cfg.AgentFor("unmapped-task"); !ok || name != "generalist" {
		t.Errorf("AgentFor = %q/%v, want the configured default", name, ok)
	}
	// An explicit mapping always wins over the default.
	if name, _ := cfg.AgentFor(agent.TaskBlogDraft); name != "writer" {
		t.Errorf("AgentFor(blog-draft) = %q, want the explicit mapping", name)
	}
}

func ExampleClient_Submit() {
	fmt.Println("submit → execution ID; poll → status; fetch → result")
	// Output: submit → execution ID; poll → status; fetch → result
}
