package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeProvider stands in for Ollama. The Service must be fully testable without one:
// if it were not, every test of the platform's inference logic would need an HTTP
// server, and the Provider interface would not be earning its keep.
type fakeProvider struct {
	caps   Capabilities
	models []Model
	res    Response
	chunks []string
	err    error

	calls int
	got   Request
}

func (f *fakeProvider) Name() string               { return "fake" }
func (f *fakeProvider) Capabilities() Capabilities { return f.caps }

func (f *fakeProvider) Models(context.Context) ([]Model, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

func (f *fakeProvider) Generate(_ context.Context, req Request) (Response, error) {
	f.calls++
	f.got = req
	return f.res, f.err
}

func (f *fakeProvider) Stream(_ context.Context, req Request, sink Sink) (Response, error) {
	f.calls++
	f.got = req
	if f.err != nil {
		return Response{}, f.err
	}
	for _, c := range f.chunks {
		if err := sink(Chunk{Content: c}); err != nil {
			return Response{}, err
		}
	}
	_ = sink(Chunk{Done: true})
	return f.res, nil
}

func newFake() *fakeProvider {
	return &fakeProvider{
		caps:   Capabilities{Local: true, Streaming: true, MaxContextTokens: 8192},
		models: []Model{{Name: "llama3.2:latest", ParameterSize: "3.2B"}},
		res: Response{
			Model: "llama3.2", Content: "A summary.", FinishReason: "stop", Attempts: 1,
			Usage: Usage{PromptTokens: 100, CompletionTokens: 20, TokensPerSecond: 42.5},
		},
		chunks: []string{"A ", "summary."},
	}
}

func newService(p Provider) (*Service, *strings.Builder) {
	var logs strings.Builder
	return NewService(p, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))), &logs
}

func testRequest() Request {
	return Request{
		System:              "You are a technical writer.",
		Prompt:              "Summarise this diff.",
		Purpose:             "diff-summary",
		CorrelationID:       "push:delivery-abc",
		WorkflowExecutionID: "n8n-42",
	}
}

// The logs are a deliverable. Assert on FIELDS, not substrings: a grep would pass even
// if the field names were wrong, and the field names are the contract with CloudWatch.
func logLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("log line is not JSON (it must be, for CloudWatch): %q", line)
		}
		out = append(out, e)
	}
	return out
}

func find(entries []map[string]any, msg string) map[string]any {
	for _, e := range entries {
		if e["msg"] == msg {
			return e
		}
	}
	return nil
}

func sink() (Sink, *strings.Builder) {
	var b strings.Builder
	return func(c Chunk) error { b.WriteString(c.Content); return nil }, &b
}

// --- the happy path ---------------------------------------------------------

func TestGenerate(t *testing.T) {
	p := newFake()
	svc, _ := newService(p)

	res, err := svc.Generate(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Content != "A summary." {
		t.Errorf("Content = %q", res.Content)
	}
	if p.got.System == "" {
		t.Error("the system prompt must reach the provider")
	}
}

func TestStreamPassesChunksThrough(t *testing.T) {
	p := newFake()
	svc, _ := newService(p)

	s, got := sink()
	if _, err := svc.Stream(context.Background(), testRequest(), s); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got.String() != "A summary." {
		t.Errorf("sink = %q, want the assembled answer", got.String())
	}
}

// --- the silent-truncation guard --------------------------------------------

// THE most valuable check in this package.
//
// A model asked to read more than its context window does not refuse. It silently drops
// the beginning of the prompt and answers confidently from what is left — producing a
// plausible, wrong summary, with no error anywhere. Refusing to send it is the only way
// to see the problem at all.
func TestAnOversizedPromptIsRefusedNotTruncated(t *testing.T) {
	p := newFake()
	p.caps.MaxContextTokens = 1000 // ~3000 characters
	svc, logs := newService(p)

	req := testRequest()
	req.Prompt = strings.Repeat("x", 10_000) // ~3333 tokens: far too big

	_, err := svc.Generate(context.Background(), req)
	if !errors.Is(err, ErrContextExceeded) {
		t.Fatalf("error = %v, want ErrContextExceeded", err)
	}
	if p.calls != 0 {
		t.Error("an oversized prompt must never reach the provider — it would be silently truncated")
	}
	// The error has to explain the *consequence*, or nobody will understand why the
	// platform refused something the model would have "accepted".
	if !strings.Contains(err.Error(), "silently drop") {
		t.Errorf("the error should explain what truncation would do; got %q", err)
	}

	entry := find(logLines(t, logs.String()), "prompt does not fit")
	if entry == nil || entry["errorKind"] != "context_exceeded" {
		t.Errorf("want a context_exceeded log; got %v", entry)
	}
	if entry["contextWindow"] != float64(1000) {
		t.Errorf("the log must record the window it did not fit in; got %v", entry["contextWindow"])
	}
}

func TestAPromptThatFitsIsSent(t *testing.T) {
	p := newFake()
	p.caps.MaxContextTokens = 8192
	svc, _ := newService(p)

	req := testRequest()
	req.Prompt = strings.Repeat("x", 9000) // ~3000 tokens: fits

	if _, err := svc.Generate(context.Background(), req); err != nil {
		t.Fatalf("a prompt that fits must be sent: %v", err)
	}
	if p.calls != 1 {
		t.Error("the provider should have been called")
	}
}

// --- prompts are not logged -------------------------------------------------

// Prompts contain repository content: source code, commit messages, and on a bad day
// something nobody meant to commit. Logging them ships all of that to CloudWatch.
func TestThePromptIsNeverLogged(t *testing.T) {
	p := newFake()
	svc, logs := newService(p)

	req := testRequest()
	req.Prompt = "func secretBusinessLogic() { apiKey := \"hunter2\" }"
	req.System = "You are a reviewer of PROPRIETARY code."

	if _, err := svc.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	out := logs.String()
	for _, leak := range []string{"secretBusinessLogic", "hunter2", "PROPRIETARY"} {
		if strings.Contains(out, leak) {
			t.Errorf("the prompt leaked into the logs (%q): %s", leak, out)
		}
	}

	// But the log must still be USEFUL: a size, and a hash so two log lines can be
	// recognised as the same prompt without either containing it.
	requested := find(logLines(t, out), "inference requested")
	if requested == nil {
		t.Fatal("an inference must be logged")
	}
	if requested["promptHash"] == nil || requested["promptHash"] == "" {
		t.Error("the log needs a prompt hash — an identifier, not the content")
	}
	if requested["promptChars"] == nil {
		t.Error("the log needs the prompt size")
	}
}

// Nor is the completion. It is derived from the prompt and can contain the same things.
func TestTheCompletionIsNeverLogged(t *testing.T) {
	p := newFake()
	p.res.Content = "The API key is hunter2 and the internal host is db.internal"
	svc, logs := newService(p)

	if _, err := svc.Generate(context.Background(), testRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.Contains(logs.String(), "hunter2") || strings.Contains(logs.String(), "db.internal") {
		t.Error("the completion leaked into the logs")
	}
	// Its size is logged instead, which is what you actually need.
	if find(logLines(t, logs.String()), "inference completed")["outputChars"] == nil {
		t.Error("the completion's size should be logged")
	}
}

// --- results we must not trust ----------------------------------------------

// A 200 with zero tokens is not a success. Passing an empty string back as though it
// were an answer is how a platform publishes an empty blog post.
func TestAnEmptyCompletionIsAFailure(t *testing.T) {
	p := newFake()
	p.res.Content = "   "
	svc, logs := newService(p)

	if _, err := svc.Generate(context.Background(), testRequest()); !errors.Is(err, ErrEmptyCompletion) {
		t.Fatalf("error = %v, want ErrEmptyCompletion", err)
	}
	if find(logLines(t, logs.String()), "inference produced nothing") == nil {
		t.Error("an empty completion must be logged as a failure")
	}
}

// "length" means the model was CUT OFF by the token budget. A truncated blog post looks
// a great deal like a finished one until somebody reads the end of it.
func TestHittingTheTokenLimitIsWarnedAbout(t *testing.T) {
	p := newFake()
	p.res.FinishReason = "length"
	svc, logs := newService(p)

	res, err := svc.Generate(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("a cut-off answer is still an answer: %v", err)
	}
	if res.FinishReason != "length" {
		t.Errorf("FinishReason = %q, want it surfaced to the caller", res.FinishReason)
	}

	entries := logLines(t, logs.String())
	warned := find(entries, "inference hit the token limit and was cut off — the output is INCOMPLETE")
	if warned == nil {
		t.Fatal("being cut off must be a WARNING, not a silent success")
	}
	if warned["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", warned["level"])
	}
}

// --- model validation -------------------------------------------------------

func TestEnsureModel(t *testing.T) {
	p := newFake()
	svc, _ := newService(p)

	// The tag is optional: "llama3.2" and "llama3.2:latest" are the same model, and a
	// config that omits the tag should not be an error.
	if err := svc.EnsureModel(context.Background(), "llama3.2"); err != nil {
		t.Errorf("EnsureModel: %v", err)
	}
	if err := svc.EnsureModel(context.Background(), "llama3.2:latest"); err != nil {
		t.Errorf("EnsureModel with an explicit tag: %v", err)
	}
}

// The error must list what IS there. Otherwise the next thing that happens is somebody
// SSHing into the box to run `ollama list`.
func TestAMissingModelSaysWhatIsAvailable(t *testing.T) {
	p := newFake()
	p.models = []Model{{Name: "qwen2.5-coder:7b"}, {Name: "gemma2:2b"}}
	svc, _ := newService(p)

	err := svc.EnsureModel(context.Background(), "llama3.2")
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("error = %v, want ErrModelNotFound", err)
	}
	for _, want := range []string{"qwen2.5-coder:7b", "gemma2:2b", "ollama pull llama3.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q; got %q", want, err)
		}
	}
}

// --- observability ----------------------------------------------------------

func TestTheCorrelationChainIsLogged(t *testing.T) {
	p := newFake()
	svc, logs := newService(p)

	if _, err := svc.Generate(context.Background(), testRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	entries := logLines(t, logs.String())
	requested, completed := find(entries, "inference requested"), find(entries, "inference completed")
	if requested == nil || completed == nil {
		t.Fatal("want both 'inference requested' and 'inference completed'")
	}

	for _, e := range []map[string]any{requested, completed} {
		// GitHub delivery → n8n → inference. Without the chain, a slow generation is a
		// mystery rather than a step in something.
		for _, field := range []string{"correlationId", "workflowExecutionId", "provider", "purpose", "model"} {
			if e[field] == nil || e[field] == "" {
				t.Errorf("log %q is missing %q", e["msg"], field)
			}
		}
	}

	// tokens/sec is the number that tells you, from a log line, whether the model is on
	// a GPU or quietly fell back to a CPU.
	if completed["tokensPerSecond"] != 42.5 {
		t.Errorf("tokensPerSecond = %v, want it logged", completed["tokensPerSecond"])
	}
	if completed["promptTokens"] != float64(100) || completed["completionTokens"] != float64(20) {
		t.Errorf("token counts must be logged; got %v / %v", completed["promptTokens"], completed["completionTokens"])
	}
}

// Purpose is what makes "what is this platform spending its tokens on?" answerable —
// the first question anyone asks when the GPU bill arrives.
func TestPurposeIsLogged(t *testing.T) {
	p := newFake()
	svc, logs := newService(p)

	req := testRequest()
	req.Purpose = "release-notes"

	if _, err := svc.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if find(logLines(t, logs.String()), "inference completed")["purpose"] != "release-notes" {
		t.Error("purpose must be logged, or token spend cannot be attributed")
	}
}

func TestStreamLogsTimeToFirstToken(t *testing.T) {
	p := newFake()

	// A clock that advances 100ms per call, so the timings are exact and no test sleeps.
	var ticks int
	clock := func() time.Time {
		t := time.Unix(0, 0).Add(time.Duration(ticks) * 100 * time.Millisecond)
		ticks++
		return t
	}

	var logs strings.Builder
	svc := NewService(p, slog.New(slog.NewJSONHandler(&logs, nil)), WithClock(clock))

	s, _ := sink()
	if _, err := svc.Stream(context.Background(), testRequest(), s); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Time to first token is the latency a human actually experiences, and it separates
	// "the model is slow" from "the model was not loaded".
	completed := find(logLines(t, logs.String()), "inference completed")
	if completed["firstTokenMs"] == nil {
		t.Error("a streamed inference must log time-to-first-token")
	}
}

func TestFailuresAreLoggedWithAStableKind(t *testing.T) {
	for _, tt := range []struct {
		err  error
		kind string
	}{
		{ErrUnavailable, "unavailable"},
		{ErrTimeout, "timeout"},
		{ErrStalled, "stalled"},
		{ErrModelNotFound, "model_not_found"},
		{ErrStreamBroken, "stream_broken"},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			p := newFake()
			p.err = tt.err
			svc, logs := newService(p)

			if _, err := svc.Generate(context.Background(), testRequest()); err == nil {
				t.Fatal("want an error")
			}
			failed := find(logLines(t, logs.String()), "inference failed")
			if failed == nil || failed["errorKind"] != tt.kind {
				t.Errorf("errorKind = %v, want %q", failed["errorKind"], tt.kind)
			}
		})
	}
}

// ErrRetriesExhausted wraps its cause, so Kind must still report the cause: "we gave up"
// is far less useful on call than "we gave up on a stall".
func TestKindReportsTheCauseNotTheWrapper(t *testing.T) {
	if got := Kind(errors.Join(ErrRetriesExhausted, ErrStalled)); got != "stalled" {
		t.Errorf("Kind = %q, want stalled (the cause)", got)
	}
}

// --- validation -------------------------------------------------------------

func TestBadRequestsNeverReachTheProvider(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{"no prompt and no messages", func(r *Request) { r.Prompt = "" }},
		{"both prompt and messages", func(r *Request) {
			r.Messages = []Message{{Role: RoleUser, Content: "hi"}}
		}},
		{"temperature out of range", func(r *Request) { r.Options.Temperature = 5 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newFake()
			svc, _ := newService(p)

			req := testRequest()
			tt.mutate(&req)

			if _, err := svc.Generate(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
			if p.calls != 0 {
				t.Error("an invalid request must never reach the provider")
			}
		})
	}
}

// --- the seam ---------------------------------------------------------------

// The point of the Provider interface: Bedrock (M8), Claude (M9), and the router that
// chooses between them (M10) implement this, and nothing above changes. If this test
// ever needs an HTTP server, the abstraction has failed.
func TestTheProviderIsReplaceable(t *testing.T) {
	var p Provider = newFake()
	svc := NewService(p, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	if _, err := svc.Generate(context.Background(), testRequest()); err != nil {
		t.Fatalf("Generate against a non-Ollama provider: %v", err)
	}
	// Capabilities is what a router will route ON. Local decides whether the prompt —
	// full of somebody's source code — leaves the network at all.
	if !svc.Provider().Capabilities().Local {
		t.Error("the fake claims to be local; the Service must surface that")
	}
}
