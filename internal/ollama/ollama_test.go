package ollama

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

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/httpx"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testConfig(baseURL string) Config {
	return Config{
		BaseURL:          baseURL,
		Model:            "llama3.2",
		Timeout:          2 * time.Second,
		IdleTimeout:      200 * time.Millisecond,
		RetryAttempts:    3,
		RetryDelay:       time.Millisecond,
		ContextTokens:    8192,
		MaxTokens:        512,
		Temperature:      0.2,
		Stream:           true,
		KeepAlive:        "5m",
		MaxResponseBytes: DefaultMaxResponseBytes,
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

func testRequest() llm.Request {
	return llm.Request{
		System:        "You are a technical writer.",
		Prompt:        "Summarise this diff.",
		Purpose:       "diff-summary",
		CorrelationID: "push:delivery-abc",
	}
}

// ndjson writes Ollama's streaming format: one JSON object per line, flushed as it
// goes — which is what makes it a stream rather than a slow response.
func ndjson(w http.ResponseWriter, chunks ...generateResponse) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	for _, c := range chunks {
		line, _ := json.Marshal(c)
		_, _ = w.Write(append(line, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func collect() (llm.Sink, *strings.Builder) {
	var b strings.Builder
	return func(c llm.Chunk) error {
		b.WriteString(c.Content)
		return nil
	}, &b
}

// --- streaming --------------------------------------------------------------

func TestStreamAssemblesTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("went to %s, want /api/generate", r.URL.Path)
		}
		var body generateRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			t.Error("stream must be true on a streaming call")
		}
		if body.System != "You are a technical writer." {
			t.Errorf("system = %q, want it forwarded", body.System)
		}
		if body.Options.NumPredict != 512 || body.Options.Temperature != 0.2 {
			t.Errorf("options = %+v, want the configured defaults applied", body.Options)
		}

		ndjson(w,
			generateResponse{Model: "llama3.2", Response: "Spot "},
			generateResponse{Response: "instances "},
			generateResponse{Response: "are cheap."},
			generateResponse{
				Model: "llama3.2", Done: true, DoneReason: "stop",
				PromptEvalCount: 120, EvalCount: 8,
				EvalDuration: int64(2 * time.Second), LoadDuration: int64(300 * time.Millisecond),
			},
		)
	}))
	defer srv.Close()

	sink, got := collect()
	res, err := newClient(t, testConfig(srv.URL)).Stream(context.Background(), testRequest(), sink)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if got.String() != "Spot instances are cheap." {
		t.Errorf("sink received %q, want the assembled answer", got.String())
	}
	if res.Content != got.String() {
		t.Errorf("Content = %q, want it to match what the sink saw", res.Content)
	}
	if res.Usage.PromptTokens != 120 || res.Usage.CompletionTokens != 8 {
		t.Errorf("usage = %+v, want the counters from the final chunk", res.Usage)
	}
	// The most diagnostic number in the integration: 8 tokens in 2s = 4/sec = a CPU.
	if res.Usage.TokensPerSecond != 4 {
		t.Errorf("TokensPerSecond = %v, want 4", res.Usage.TokensPerSecond)
	}
	if res.Usage.LoadDuration != 300*time.Millisecond {
		t.Errorf("LoadDuration = %v, want it reported", res.Usage.LoadDuration)
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", res.FinishReason)
	}
}

// THE test of this milestone.
//
// A stream that dies after emitting tokens must NOT be retried. The caller already has
// the beginning of an answer; a retry would hand them a second beginning, silently
// glued on, and the result reads as though the model lost its mind.
func TestABrokenStreamIsNotRetriedOnceTokensHaveEscaped(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Emit real tokens, then die without a done marker — a dropped connection
		// mid-answer.
		ndjson(w,
			generateResponse{Response: "The answer "},
			generateResponse{Response: "begins here"},
		)
	}))
	defer srv.Close()

	sink, got := collect()
	_, err := newClient(t, testConfig(srv.URL)).Stream(context.Background(), testRequest(), sink)

	if !errors.Is(err, llm.ErrStreamBroken) {
		t.Fatalf("error = %v, want ErrStreamBroken", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("Ollama was called %d times — a stream that emitted tokens must NOT be retried, "+
			"or the caller receives a second beginning appended to the first", n)
	}
	// And the caller keeps what they were given: we do not pretend it never happened.
	if got.String() != "The answer begins here" {
		t.Errorf("sink = %q, want the partial output it was actually handed", got.String())
	}
}

// The mirror image: a failure BEFORE any token is clean to retry, because the caller
// has seen nothing.
func TestAFailureBeforeTheFirstTokenIsRetried(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ndjson(w,
			generateResponse{Response: "Recovered."},
			generateResponse{Done: true, DoneReason: "stop", EvalCount: 2, EvalDuration: int64(time.Second)},
		)
	}))
	defer srv.Close()

	sink, got := collect()
	res, err := newClient(t, testConfig(srv.URL)).Stream(context.Background(), testRequest(), sink)
	if err != nil {
		t.Fatalf("a blip before any output should be absorbed, got: %v", err)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
	// Crucially, the caller sees ONE answer, not three.
	if got.String() != "Recovered." {
		t.Errorf("sink = %q, want exactly one answer", got.String())
	}
}

// A stall is not a timeout, and the difference is the whole point. A slow model on a
// CPU keeps producing tokens; a wedged one goes quiet. Only the second is broken.
func TestAStalledStreamIsDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)
		line, _ := json.Marshal(generateResponse{Response: "I am thinking"})
		_, _ = w.Write(append(line, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
		// ...and then nothing, forever. No more tokens, no close.
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.IdleTimeout = 100 * time.Millisecond
	cfg.RetryAttempts = 3 // it must decline to retry on its own, not because it cannot

	sink, _ := collect()
	start := time.Now()
	_, err := newClient(t, cfg).Stream(context.Background(), testRequest(), sink)
	elapsed := time.Since(start)

	// It must be reported as a STALL, not as a generic cancellation — "the model went
	// quiet" is actionable, "context canceled" is not.
	if !errors.Is(err, llm.ErrStalled) {
		t.Fatalf("error = %v, want it to report the CAUSE (ErrStalled)", err)
	}
	// And because this stall happened AFTER a token had escaped, it is also a broken
	// stream — which is what stops it being retried. Both facts must survive: one says
	// what went wrong, the other says what to do about it.
	if !errors.Is(err, llm.ErrStreamBroken) {
		t.Errorf("error = %v, want it ALSO to be ErrStreamBroken — a stall after output "+
			"cannot be retried, or the caller gets a second beginning", err)
	}
	if elapsed > time.Second {
		t.Errorf("took %v to notice a stall — the idle timer is not being enforced", elapsed)
	}
}

// A stall BEFORE the first token is a different story: nobody has seen anything, so a
// retry is clean — the model was probably loading and wedged, and asking again is free.
func TestAStallBeforeTheFirstTokenIsRetried(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.(http.Flusher).Flush()
			time.Sleep(2 * time.Second) // headers, then silence: no tokens at all
			return
		}
		ndjson(w,
			generateResponse{Response: "Recovered."},
			generateResponse{Done: true, DoneReason: "stop"},
		)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.IdleTimeout = 100 * time.Millisecond

	sink, got := collect()
	res, err := newClient(t, cfg).Stream(context.Background(), testRequest(), sink)
	if err != nil {
		t.Fatalf("a stall before any output is safely retryable, got: %v", err)
	}
	if res.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", res.Attempts)
	}
	if got.String() != "Recovered." {
		t.Errorf("sink = %q, want exactly one answer", got.String())
	}
}

// The idle timer must RESET on every token. A model that is producing output slowly but
// steadily is healthy, and killing it would be the timeout bug this design exists to
// avoid.
func TestASlowButSteadyStreamIsNotKilled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)
		// Eight tokens, each 60ms apart: 480ms total, far beyond the 100ms idle timeout,
		// but never 100ms of silence.
		for i := 0; i < 8; i++ {
			line, _ := json.Marshal(generateResponse{Response: "tok "})
			_, _ = w.Write(append(line, '\n'))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(60 * time.Millisecond)
		}
		final, _ := json.Marshal(generateResponse{Done: true, DoneReason: "stop"})
		_, _ = w.Write(append(final, '\n'))
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.IdleTimeout = 100 * time.Millisecond // shorter than the total generation
	cfg.RetryAttempts = 1

	sink, got := collect()
	if _, err := newClient(t, cfg).Stream(context.Background(), testRequest(), sink); err != nil {
		t.Fatalf("a slow but steady model must not be killed: %v", err)
	}
	if strings.Count(got.String(), "tok") != 8 {
		t.Errorf("sink = %q, want all 8 tokens", got.String())
	}
}

// A caller that stops the stream is not a failure, and must not be retried — they asked
// us to stop.
func TestASinkThatStopsTheStreamIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		ndjson(w,
			generateResponse{Response: "one "},
			generateResponse{Response: "two "},
			generateResponse{Response: "three"},
			generateResponse{Done: true},
		)
	}))
	defer srv.Close()

	stopAfterOne := func(c llm.Chunk) error {
		return errors.New("caller has seen enough")
	}

	_, err := newClient(t, testConfig(srv.URL)).Stream(context.Background(), testRequest(), stopAfterOne)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it reported as a cancellation, not a provider failure", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("called %d times — the caller asked us to stop; retrying ignores them", n)
	}
}

// --- non-streaming ----------------------------------------------------------

func TestGenerate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body generateRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			t.Error("stream must be false on a non-streaming call")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(generateResponse{
			Model: "llama3.2", Response: "A complete answer.", Done: true, DoneReason: "stop",
			EvalCount: 20, EvalDuration: int64(time.Second),
		})
	}))
	defer srv.Close()

	res, err := newClient(t, testConfig(srv.URL)).Generate(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Content != "A complete answer." {
		t.Errorf("Content = %q", res.Content)
	}
	if res.Usage.TokensPerSecond != 20 {
		t.Errorf("TokensPerSecond = %v, want 20", res.Usage.TokensPerSecond)
	}
}

// The chat endpoint carries its tokens in a different field. Reading the wrong one
// yields an empty completion from a perfectly successful call.
func TestChatUsesTheMessagesEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("went to %s, want /api/chat when Messages are set", r.URL.Path)
		}
		var body generateRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		// The system prompt must be prepended as a message, or it is silently dropped.
		if len(body.Messages) != 2 || body.Messages[0].Role != "system" {
			t.Errorf("messages = %+v, want the system prompt first", body.Messages)
		}
		ndjson(w,
			generateResponse{Message: chatMessage{Role: "assistant", Content: "Hello."}},
			generateResponse{Done: true, DoneReason: "stop"},
		)
	}))
	defer srv.Close()

	req := testRequest()
	req.Prompt = ""
	req.Messages = []llm.Message{{Role: llm.RoleUser, Content: "Hi"}}

	sink, got := collect()
	if _, err := newClient(t, testConfig(srv.URL)).Stream(context.Background(), req, sink); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got.String() != "Hello." {
		t.Errorf("sink = %q, want the assistant's message", got.String())
	}
}

// --- models -----------------------------------------------------------------

func TestModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("went to %s, want /api/tags", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"models":[
		  {"name":"llama3.2:latest","size":2019393189,"details":{"family":"llama","parameter_size":"3.2B","quantization_level":"Q4_K_M"}},
		  {"name":"qwen2.5-coder:7b","size":4683087519,"details":{"family":"qwen2","parameter_size":"7.6B","quantization_level":"Q4_K_M"}}
		]}`)
	}))
	defer srv.Close()

	models, err := newClient(t, testConfig(srv.URL)).Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	// Parameter size and quantisation are what decide whether a model fits on the GPU
	// you have. They are the reason this endpoint is worth modelling at all.
	if models[1].ParameterSize != "7.6B" || models[1].Quantization != "Q4_K_M" {
		t.Errorf("model = %+v, want its size and quantisation", models[1])
	}
}

// The most common failure in practice, by a wide margin: the model was never pulled.
// The error must say so, and say how to fix it.
func TestAMissingModelSaysHowToFixIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"model 'llama3.2' not found, try pulling it first"}`)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.RetryAttempts = 1

	sink, _ := collect()
	_, err := newClient(t, cfg).Stream(context.Background(), testRequest(), sink)

	if !errors.Is(err, llm.ErrModelNotFound) {
		t.Fatalf("error = %v, want ErrModelNotFound", err)
	}
	// Retrying will not download a model. The fix is one command, and the error should
	// contain it rather than send someone hunting.
	if !strings.Contains(err.Error(), "ollama pull llama3.2") {
		t.Errorf("the error should tell you how to fix it; got %q", err)
	}
}

func TestAMissingModelIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()

	sink, _ := collect()
	_, _ = newClient(t, testConfig(srv.URL)).Stream(context.Background(), testRequest(), sink)

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("called %d times — asking again will not pull the model", n)
	}
}

// --- retries ----------------------------------------------------------------

// Unlike Milestones 5 and 6, a retry here is SAFE: generation has no side effects. So
// the retryable set is deliberately wider — a timeout and a stall are both worth
// retrying, because the worst case is that we spend the compute twice.
func TestRetryPolicy(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantCalls int32
		wantErr   error
	}{
		{"503 is retried", http.StatusServiceUnavailable, "", 3, llm.ErrUnavailable},
		{"429 is retried", http.StatusTooManyRequests, "", 3, llm.ErrUnavailable},
		{"404 model-not-found is NOT retried", http.StatusNotFound, `{"error":"model not found"}`, 1, llm.ErrModelNotFound},
		{"400 is NOT retried", http.StatusBadRequest, `{"error":"bad options"}`, 1, llm.ErrInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			sink, _ := collect()
			_, err := newClient(t, testConfig(srv.URL)).Stream(context.Background(), testRequest(), sink)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got := atomic.LoadInt32(&calls); got != tt.wantCalls {
				t.Errorf("called %d times, want %d", got, tt.wantCalls)
			}
		})
	}
}

// --- responses we must not trust --------------------------------------------

func TestAnErrorInTheStreamBodyIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ollama answers 200 and puts the error in the stream.
		ndjson(w, generateResponse{Error: "out of memory loading model"})
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.RetryAttempts = 1

	sink, _ := collect()
	_, err := newClient(t, cfg).Stream(context.Background(), testRequest(), sink)

	// A 200 with an error in it is not a success. Trusting the status code is how a
	// platform reports that it generated a blog post while producing nothing.
	if !errors.Is(err, llm.ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

func TestGarbageInTheStreamIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "<html>this is not ollama</html>\n")
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.RetryAttempts = 1

	sink, _ := collect()
	if _, err := newClient(t, cfg).Stream(context.Background(), testRequest(), sink); !errors.Is(err, llm.ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

// --- the provider contract --------------------------------------------------

func TestClientImplementsProvider(t *testing.T) {
	var _ llm.Provider = (*Client)(nil)
}

// The field a future router (Milestone 10) will actually route on. Local means the
// prompt — full of somebody's source code — does not leave the network.
func TestCapabilitiesReportLocal(t *testing.T) {
	caps := newClient(t, testConfig("http://localhost:11434")).Capabilities()

	if !caps.Local {
		t.Error("Ollama is local; a router must be able to see that the prompt does not leave")
	}
	if caps.CostPer1MInputTokensUSD != 0 {
		t.Error("a local model has no per-token price — the cost is the instance")
	}
	if caps.MaxContextTokens != 8192 {
		t.Errorf("MaxContextTokens = %d, want the configured window", caps.MaxContextTokens)
	}
}

func ExampleClient_Stream() {
	fmt.Println("stream → tokens as they arrive; a stall is not a timeout")
	// Output: stream → tokens as they arrive; a stall is not a timeout
}
