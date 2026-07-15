package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

const testSecret = "test-webhook-secret"

// fakeEvents is a stand-in EventBridge: it records what was published and can be made
// to fail, or to reject the entry (the 200-with-FailedEntryCount trap).
type fakeEvents struct {
	published []GitHubEvent
	err       error
	rejects   bool
	calls     int
}

func (f *fakeEvents) PutEvents(_ context.Context, in *eventbridge.PutEventsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.rejects {
		return &eventbridge.PutEventsOutput{
			FailedEntryCount: 1,
			Entries:          []ebtypes.PutEventsResultEntry{{ErrorCode: strptr("ThrottlingException"), ErrorMessage: strptr("slow down")}},
		}, nil
	}
	for _, e := range in.Entries {
		var ge GitHubEvent
		_ = json.Unmarshal([]byte(*e.Detail), &ge)
		f.published = append(f.published, ge)
	}
	return &eventbridge.PutEventsOutput{}, nil
}

func strptr(s string) *string { return &s }

func newHandler(t *testing.T, cfg Config, events EventsAPI) *Handler {
	t.Helper()
	if cfg.Secret == "" {
		cfg.Secret = testSecret
	}
	if cfg.EventBus == "" {
		cfg.EventBus = "test-bus"
	}
	if cfg.EventSource == "" {
		cfg.EventSource = "aiap.test.github"
	}
	cfg.Project, cfg.Environment = "aiap", "test"
	return &Handler{Cfg: cfg, Events: events, Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// signedRequest builds a Request the way GitHub would: a body, the matching signature,
// and the two headers a real delivery always carries.
func signedRequest(event, delivery, body, secret string) Request {
	return Request{
		Headers: testHeaders{
			EventHeader:     event,
			DeliveryHeader:  delivery,
			SignatureHeader: Sign([]byte(body), secret),
		},
		Body: []byte(body),
	}
}

// The happy path, end to end: a signed push is verified, parsed, filtered, and
// published as a curated event carrying the correlation id.
func TestHandlePublishesAValidPush(t *testing.T) {
	events := &fakeEvents{}
	h := newHandler(t, Config{}, events)

	body := `{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/platform","default_branch":"main"},"sender":{"login":"alice"}}`
	resp, result := h.Handle(context.Background(), signedRequest("push", "delivery-1", body, testSecret))

	if resp.StatusCode != 202 {
		t.Fatalf("status = %d, want 202 (accepted, published)", resp.StatusCode)
	}
	if result.Disposition != Accepted || !result.Published {
		t.Fatalf("result = %+v, want accepted and published", result)
	}
	if len(events.published) != 1 {
		t.Fatalf("published %d events, want 1", len(events.published))
	}
	ev := events.published[0]
	if ev.CorrelationID != "push:delivery-1" || ev.Repository != "acme/platform" || ev.Branch != "main" || ev.HeadSHA != "abc123" {
		t.Errorf("published event = %+v", ev)
	}
	// The curated event must NOT be the raw payload — no sender email, no commit list.
	// A cheap proxy: it round-trips to exactly the GitHubEvent shape and nothing more.
	if ev.Project != "aiap" || ev.Environment != "test" {
		t.Errorf("published event missing platform context: %+v", ev)
	}
}

// THE security test: a bad signature is refused with 401 and NOTHING is published. An
// unauthenticated request must never reach the bus.
func TestHandleRefusesABadSignature(t *testing.T) {
	events := &fakeEvents{}
	h := newHandler(t, Config{}, events)

	body := `{"ref":"refs/heads/main","repository":{"full_name":"acme/platform"}}`
	req := Request{
		Headers: testHeaders{
			EventHeader:     "push",
			DeliveryHeader:  "d",
			SignatureHeader: Sign([]byte(body), "the-WRONG-secret"),
		},
		Body: []byte(body),
	}

	resp, result := h.Handle(context.Background(), req)
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if events.calls != 0 {
		t.Fatal("an unauthenticated request reached EventBridge — the front door failed open")
	}
	if result.Published {
		t.Error("nothing should have been published")
	}
}

// A missing signature is also refused, and distinctly (it usually means a misconfigured
// hook, not a forgery).
func TestHandleRefusesAMissingSignature(t *testing.T) {
	events := &fakeEvents{}
	h := newHandler(t, Config{}, events)
	body := `{"repository":{"full_name":"o/r"}}`
	req := Request{Headers: testHeaders{EventHeader: "push", DeliveryHeader: "d"}, Body: []byte(body)}

	resp, _ := h.Handle(context.Background(), req)
	if resp.StatusCode != 401 || events.calls != 0 {
		t.Errorf("status=%d calls=%d, want 401 and nothing published", resp.StatusCode, events.calls)
	}
}

// A valid signature over unparseable JSON is a 400 — the sender is wrong, not the auth —
// and it must NOT ask GitHub to retry (a retry would fail identically forever).
func TestHandleRejectsAMalformedButSignedBody(t *testing.T) {
	events := &fakeEvents{}
	h := newHandler(t, Config{}, events)
	body := `{not json`
	resp, _ := h.Handle(context.Background(), signedRequest("push", "d", body, testSecret))

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (bad request, terminal — not a retryable 5xx)", resp.StatusCode)
	}
	if events.calls != 0 {
		t.Error("a malformed body must not be published")
	}
}

// An authentic but filtered event is a 200 (the request was fine) and publishes nothing.
func TestHandleIgnoresAFilteredEvent(t *testing.T) {
	events := &fakeEvents{}
	h := newHandler(t, Config{IgnoreForks: true}, events)

	body := `{"ref":"refs/heads/main","repository":{"full_name":"acme/platform","fork":true}}`
	resp, result := h.Handle(context.Background(), signedRequest("push", "d", body, testSecret))

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (valid request, deliberately ignored)", resp.StatusCode)
	}
	if result.Disposition != Ignored || result.Published {
		t.Errorf("result = %+v, want ignored and not published", result)
	}
	if events.calls != 0 {
		t.Error("a filtered event must not be published")
	}
}

// A ping is acknowledged with a 200 and publishes nothing.
func TestHandleAcknowledgesAPing(t *testing.T) {
	events := &fakeEvents{}
	h := newHandler(t, Config{}, events)
	body := `{"zen":"Non-blocking is better than blocking."}`
	resp, result := h.Handle(context.Background(), signedRequest("ping", "d", body, testSecret))

	if resp.StatusCode != 200 || result.Disposition != Acknowledged {
		t.Errorf("status=%d disposition=%q, want 200 and acknowledged", resp.StatusCode, result.Disposition)
	}
	if events.calls != 0 {
		t.Error("a ping must not be published")
	}
}

// A publish failure is the ONE case that returns 500, so GitHub redelivers — the event
// is authentic and wanted and simply was not stored.
func TestHandleReturns500WhenPublishFails(t *testing.T) {
	events := &fakeEvents{err: errors.New("eventbridge down")}
	h := newHandler(t, Config{}, events)
	body := `{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"acme/platform"}}`
	resp, result := h.Handle(context.Background(), signedRequest("push", "d", body, testSecret))

	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500 (retryable — the event was wanted and not stored)", resp.StatusCode)
	}
	if result.Published {
		t.Error("Published must be false when the put failed")
	}
}

// The 200-with-FailedEntryCount trap: EventBridge accepted the call but rejected the
// entry. That is a failure and must surface as a 500, not a silent success.
func TestHandleTreatsARejectedEntryAsAFailure(t *testing.T) {
	events := &fakeEvents{rejects: true}
	h := newHandler(t, Config{}, events)
	body := `{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"acme/platform"}}`
	resp, _ := h.Handle(context.Background(), signedRequest("push", "d", body, testSecret))

	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500 — a rejected entry is a dropped event, not a success", resp.StatusCode)
	}
}

// A missing secret fails closed with a 500 and publishes nothing — a webhook that
// cannot verify must not accept.
func TestHandleFailsClosedWithNoSecret(t *testing.T) {
	events := &fakeEvents{}
	h := newHandler(t, Config{Secret: " "}, events) // whitespace secret → treated as none by VerifySignature? no: set empty
	h.Cfg.Secret = ""
	body := `{"repository":{"full_name":"o/r"}}`
	resp, _ := h.Handle(context.Background(), signedRequest("push", "d", body, testSecret))

	if resp.StatusCode != 500 || events.calls != 0 {
		t.Errorf("status=%d calls=%d, want 500 and nothing published (fail closed)", resp.StatusCode, events.calls)
	}
}
