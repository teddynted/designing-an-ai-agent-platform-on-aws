package router

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// fake is a provider that does what the test tells it to.
//
// Every test in this package runs against these. There is no HTTP server, no AWS SDK, and no
// model — which is the point of the seam and not merely a convenience: the router's job is to
// CHOOSE, and a test of choosing that needed a GPU would be a test nobody ran.
type fake struct {
	name string
	caps llm.Capabilities

	mu    sync.Mutex
	calls int

	// err is returned instead of a completion. The whole of fallback is tested by setting
	// this to llm.ErrUnavailable on one provider and nothing on another.
	err error

	// chunks are streamed before err is returned. Non-empty + err is the case the router
	// must never fall over from: the caller already has part of an answer.
	chunks []string

	// models is what Models reports. modelsErr overrides it.
	models    []llm.Model
	modelsErr error

	// delay lets a test observe that an unhealthy provider is being skipped rather than
	// merely being fast.
	delay time.Duration
}

func (f *fake) Name() string                   { return f.name }
func (f *fake) Capabilities() llm.Capabilities { return f.caps }

func (f *fake) Models(context.Context) ([]llm.Model, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return f.models, nil
}

func (f *fake) Generate(_ context.Context, _ llm.Request) (llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return llm.Response{Attempts: 1}, f.err
	}
	return llm.Response{
		Model:        f.name + "-model",
		Content:      "answered by " + f.name,
		Attempts:     1,
		FinishReason: "stop",
	}, nil
}

func (f *fake) Stream(_ context.Context, _ llm.Request, sink llm.Sink) (llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	// Whatever tokens the test configured, sent first — this is what the caller "sees", and
	// what the stream guard counts.
	for _, c := range f.chunks {
		if err := sink(llm.Chunk{Content: c}); err != nil {
			return llm.Response{}, err
		}
	}
	if f.err != nil {
		return llm.Response{Attempts: 1}, f.err
	}

	// A clean stream emits its answer as a chunk, like a real provider does, rather than
	// smuggling it out only in the returned Response — otherwise a test could not tell a
	// stream that spoke from one that did not.
	content := "answered by " + f.name
	if len(f.chunks) == 0 {
		if err := sink(llm.Chunk{Content: content}); err != nil {
			return llm.Response{}, err
		}
	} else {
		content = strings.Join(f.chunks, "")
	}
	_ = sink(llm.Chunk{Done: true})

	return llm.Response{
		Model:        f.name + "-model",
		Content:      content,
		Attempts:     1,
		FinishReason: "stop",
	}, nil
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// The two providers the platform actually has, in the only terms the router knows them by:
// a name and a set of capabilities. Note that the router is never told which is which — it
// reads these facts through llm.Capabilities, exactly as it would for a provider written
// next year.

// local is an Ollama-shaped provider: the prompt does not leave, it costs nothing per token,
// its window is small, and it cannot call tools.
func local(name string) *fake {
	return &fake{
		name: name,
		caps: llm.Capabilities{
			Local:            true,
			Streaming:        true,
			MaxContextTokens: 8192,
			Tools:            false,
			Reasoning:        false,
		},
		models: []llm.Model{{Name: "llama3.2", ParameterSize: "3B"}},
	}
}

// hosted is a Bedrock-shaped provider: the prompt leaves, it is billed per token, its window
// is large, and it can do everything.
func hosted(name string) *fake {
	return &fake{
		name: name,
		caps: llm.Capabilities{
			Local:                    false,
			Streaming:                true,
			MaxContextTokens:         200_000,
			Tools:                    true,
			StructuredOutput:         true,
			Reasoning:                true,
			CostPer1MInputTokensUSD:  3,
			CostPer1MOutputTokensUSD: 15,
		},
		models: []llm.Model{{Name: "claude", ParameterSize: "?"}},
	}
}

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// build wires a router over the given fakes, in the order given.
func build(t testingT, cfg Config, fakes ...*fake) *Router {
	t.Helper()

	providers := map[string]llm.Provider{}
	if cfg.Providers == nil {
		for _, f := range fakes {
			cfg.Providers = append(cfg.Providers, f.name)
		}
	}
	for _, f := range fakes {
		providers[f.name] = f
	}
	if cfg.Default == "" {
		cfg.Default = cfg.Providers[0]
	}
	if cfg.Strategy == "" {
		cfg.Strategy = StrategyFixed
	}
	if cfg.HealthThreshold == 0 {
		cfg.HealthThreshold = 1 // one failure is enough, so tests do not have to fail twice
	}
	if cfg.HealthCooldown == 0 {
		cfg.HealthCooldown = time.Minute
	}

	r, err := New(providers, cfg, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// testingT is the slice of *testing.T the helper needs, so the helper can be used from a
// benchmark or a fuzz target later without changing.
type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
}
