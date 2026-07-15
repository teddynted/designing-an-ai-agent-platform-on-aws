package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// decode reads the single JSON log line the logger wrote into a map, failing the
// test if the buffer does not hold exactly one parseable line.
func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no log line was written")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("expected one line, got several:\n%s", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log line is not valid JSON: %v\n%s", err, line)
	}
	return m
}

func TestServiceIsStampedOnEveryLine(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{Service: "webhook"}, &buf)

	log.Info("hello")

	m := decode(t, &buf)
	if m[FieldService] != "webhook" {
		t.Errorf("service = %v, want webhook", m[FieldService])
	}
	if m["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", m["msg"])
	}
}

func TestContextFieldsAreStamped(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{Service: "workflow"}, &buf)

	ctx := WithFields(context.Background(), Fields{
		CorrelationID: "push:delivery-abc",
		WorkflowID:    "blog-generator",
		ExecutionID:   "exec-1",
	})

	log.InfoContext(ctx, "workflow completed")

	m := decode(t, &buf)
	for k, want := range map[string]string{
		FieldCorrelationID: "push:delivery-abc",
		FieldWorkflowID:    "blog-generator",
		FieldExecutionID:   "exec-1",
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
}

// A field set explicitly at the call site must win over the coarser one from the
// context — otherwise the more specific value is silently lost.
func TestCallSiteFieldWinsOverContext(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{}, &buf)

	ctx := WithFields(context.Background(), Fields{ExecutionID: "from-context"})
	log.InfoContext(ctx, "msg", FieldExecutionID, "from-call")

	m := decode(t, &buf)
	if m[FieldExecutionID] != "from-call" {
		t.Errorf("executionId = %v, want from-call (call site must win)", m[FieldExecutionID])
	}
}

func TestWithFieldsMergesRatherThanReplaces(t *testing.T) {
	base := WithFields(context.Background(), Fields{CorrelationID: "corr-1"})
	extended := WithFields(base, Fields{Component: "engine"})

	got := FieldsFrom(extended)
	if got.CorrelationID != "corr-1" {
		t.Errorf("correlation lost on merge: %q", got.CorrelationID)
	}
	if got.Component != "engine" {
		t.Errorf("component not added: %q", got.Component)
	}
}

// Redaction is a property of the handler: a caller that logs a secret by mistake
// still does not leak it.
func TestSecretsAreRedactedInLogs(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{}, &buf)

	log.Info("configured",
		"token", "super-secret-value",
		"n8n_api_key", "another-secret",
		"repository", "teddynted/platform", // not sensitive: must survive
	)

	line := buf.String()
	if strings.Contains(line, "super-secret-value") || strings.Contains(line, "another-secret") {
		t.Errorf("a secret reached the log line:\n%s", line)
	}
	m := decode(t, &buf)
	if m["token"] != redacted {
		t.Errorf("token = %v, want %q", m["token"], redacted)
	}
	if m["repository"] != "teddynted/platform" {
		t.Errorf("non-sensitive field was redacted: %v", m["repository"])
	}
}

func TestLevelIsHonoured(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{Level: slog.LevelWarn}, &buf)

	log.Info("this should be filtered out")
	if buf.Len() != 0 {
		t.Errorf("info line was emitted at warn level: %s", buf.String())
	}

	log.Warn("this should appear")
	if buf.Len() == 0 {
		t.Error("warn line was filtered out at warn level")
	}
}

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	log := newWithWriter(Config{Format: "text", Service: "cli"}, &buf)
	log.Info("hello")
	if !strings.Contains(buf.String(), "service=cli") {
		t.Errorf("text handler not used or service missing:\n%s", buf.String())
	}
}
