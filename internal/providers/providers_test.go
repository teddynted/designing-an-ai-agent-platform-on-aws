package providers

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// ollamaEnv is a valid Ollama configuration.
func ollamaEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OLLAMA_BASE_URL", "http://localhost:11434")
	t.Setenv("OLLAMA_MODEL", "llama3.2")
}

// bedrockEnv is a valid Bedrock configuration. Note what is NOT in it: a credential.
func bedrockEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BEDROCK_REGION", "us-east-1")
	t.Setenv("BEDROCK_MODEL_ID", "anthropic.claude-3-5-haiku-20241022-v1:0")
	// Point the SDK at a dead endpoint and give it fake static credentials, so building the
	// client cannot reach for the EC2 metadata service and hang in CI. Nothing is called.
	t.Setenv("BEDROCK_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
}

// THE test of "switch providers through configuration rather than code".
//
// One environment variable, two entirely different inference backends — one on hardware
// you own, one managed by AWS — and the caller receives the same interface either way.
func TestTheProviderIsChosenByConfiguration(t *testing.T) {
	t.Run("ollama", func(t *testing.T) {
		ollamaEnv(t)
		t.Setenv(EnvProvider, "ollama")

		provider, info, err := New(context.Background(), discardLogger())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if provider.Name() != "ollama" || info.Provider != "ollama" {
			t.Errorf("built %q, want ollama", provider.Name())
		}
		// The prompt does not leave.
		if !provider.Capabilities().Local {
			t.Error("Ollama must report Local")
		}
	})

	t.Run("bedrock", func(t *testing.T) {
		bedrockEnv(t)
		t.Setenv(EnvProvider, "bedrock")

		provider, info, err := New(context.Background(), discardLogger())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if provider.Name() != "bedrock" || info.Provider != "bedrock" {
			t.Errorf("built %q, want bedrock", provider.Name())
		}
		// The prompt LEAVES. Same interface, opposite answer to the question that matters
		// most — which is exactly what a router will read in Milestone 10.
		if provider.Capabilities().Local {
			t.Error("Bedrock must NOT report Local — the prompt leaves the VPC")
		}
	})
}

// The default must be the one where the prompt does not leave. A platform that ships
// somebody's source code to a hosted service because nobody set an environment variable
// has made that choice on their behalf, and made it badly.
func TestTheDefaultIsTheOneThatKeepsThePromptAtHome(t *testing.T) {
	ollamaEnv(t)
	t.Setenv(EnvProvider, "") // unset

	provider, _, err := New(context.Background(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !provider.Capabilities().Local {
		t.Errorf("the default provider is %q, which is not local — the safe default is the one "+
			"that does not send source code to a third party", provider.Name())
	}
}

func TestAnUnknownProviderSaysWhatIsKnown(t *testing.T) {
	t.Setenv(EnvProvider, "gpt-9")

	_, _, err := New(context.Background(), discardLogger())
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"bedrock", "ollama"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should list what IS known; got %q", err)
		}
	}
}

// A misconfigured provider must fail at start-up, not on the first inference of the day.
func TestAMisconfiguredProviderFailsToBuild(t *testing.T) {
	t.Setenv(EnvProvider, "bedrock")
	t.Setenv("BEDROCK_MODEL_ID", "") // required

	if _, _, err := New(context.Background(), discardLogger()); err == nil {
		t.Fatal("a Bedrock provider with no model must not build")
	}
}

// Info exists so a caller can describe the provider without importing a vendor package.
// The moment a CLI has to type bedrock.Config to print a model name, the abstraction has
// sprung a leak.
func TestInfoDescribesTheProviderWithoutLeakingItsType(t *testing.T) {
	bedrockEnv(t)
	t.Setenv(EnvProvider, "bedrock")

	_, info, err := New(context.Background(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if info.Model == "" || info.Endpoint == "" {
		t.Errorf("Info = %+v, want the model and where it lives, in plain terms", info)
	}
	// And it must be printable without leaking anything, because there is nothing to leak.
	creds, _ := info.Redacted["credentials"].(string)
	if !strings.Contains(creds, "IAM") {
		t.Errorf("Redacted credentials = %q, want it to say IAM", creds)
	}
}
