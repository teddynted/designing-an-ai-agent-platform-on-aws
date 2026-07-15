package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

// DetailType is the name the platform publishes GitHub events under, on its own bus.
// It is the platform's spelling, not GitHub's: a subscriber on the platform bus routes
// on "GitHub Event" and reads a [GitHubEvent], and never has to know GitHub's webhook
// schema — which is the entire point of putting EventBridge between the two.
const DetailType = "GitHub Event"

// EventsAPI is the slice of the EventBridge client the handler uses. It is an interface
// so the whole of [Handler.Handle] runs in a unit test against a fake, with no AWS
// account — the same seam the spot package uses.
type EventsAPI interface {
	PutEvents(context.Context, *eventbridge.PutEventsInput, ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error)
}

// GitHubEvent is the CURATED detail the platform publishes for a GitHub webhook. It is
// what n8n, an audit consumer, or anything else on the bus receives — and it is
// deliberately not GitHub's payload.
//
// # Why a curated event and not the raw payload
//
// Three reasons, and each would be sufficient on its own:
//
//   - **Decoupling.** A consumer reads these stable fields and never parses GitHub's
//     schema. When GitHub changes a payload — and it does — the change is absorbed here,
//     in the parser, and no consumer notices. Forward the raw payload and every
//     consumer is coupled to GitHub's JSON forever.
//   - **Redaction.** GitHub's payloads carry commit messages, file lists, and author
//     emails. None of that is anything the platform routes on, and all of it is content
//     that should not be sprayed across every downstream system. What is not in this
//     struct is not forwarded.
//   - **Size.** A push payload can be tens of kilobytes; EventBridge caps an entry at
//     256 KB, and a large repository's push can approach it. This is a few hundred bytes.
type GitHubEvent struct {
	// CorrelationID threads this event through the whole platform. It is the first
	// field because it is the one every downstream log line will carry.
	CorrelationID string `json:"correlationId"`

	// DeliveryID is GitHub's own id, kept so a consumer can dedupe redeliveries and so
	// a platform log can be tied back to a specific delivery in GitHub's UI.
	DeliveryID string `json:"deliveryId"`

	Event      string `json:"event"`
	Action     string `json:"action,omitempty"`
	Repository string `json:"repository"`
	Branch     string `json:"branch,omitempty"`
	Ref        string `json:"ref,omitempty"`
	RefType    string `json:"refType,omitempty"`
	HeadSHA    string `json:"headSha,omitempty"`
	Sender     string `json:"sender,omitempty"`
	Private    bool   `json:"private"`

	Project     string `json:"project"`
	Environment string `json:"environment"`
}

// Config is defined in config.go; Handler is here.

// Handler processes one webhook request. Its only AWS dependency is [EventsAPI], so it
// is fully testable, and it holds the secret it verifies against — fetched once, at
// cold start, never from the request.
type Handler struct {
	Cfg    Config
	Events EventsAPI
	Log    *slog.Logger
}

// Response is the handler's HTTP-shaped answer, independent of how the Lambda is
// invoked (a Function URL, an API Gateway). The command's main maps it to whatever the
// runtime expects, so this package never imports the Lambda events types and stays
// testable with plain values.
type Response struct {
	StatusCode int
	Body       string
}

// Request is the handler's HTTP-shaped input, again independent of the invocation
// mechanism. The main builds it from the Function URL request.
type Request struct {
	Headers Headers
	Body    []byte
}

// Result is the structured outcome, returned alongside the Response so a caller (and a
// console test, and the logs) can see what the handler decided without parsing the body.
type Result struct {
	Disposition Disposition `json:"disposition"`
	Event       string      `json:"event,omitempty"`
	DeliveryID  string      `json:"deliveryId,omitempty"`
	Repository  string      `json:"repository,omitempty"`
	Reason      string      `json:"reason,omitempty"`
	Published   bool        `json:"published"`
}

// Handle runs one request through the pipeline: verify, parse, filter, publish.
//
// # What the HTTP status codes mean, and why they are what they are
//
//	200  the request was authentic and we are done with it — whether we published it,
//	     ignored it, or acknowledged a ping. GitHub only needs to know we RECEIVED it;
//	     what we chose to do with a valid event is not GitHub's concern, and a 4xx for
//	     an event we simply do not care about would light up GitHub's "recent
//	     deliveries" with red that means nothing.
//	401  the signature was missing or wrong. The one case where GitHub — or whoever is
//	     calling — should be told the request was REFUSED.
//	400  the request was authentic but malformed: a valid signature over unparseable
//	     JSON, or missing the headers a GitHub delivery always has. Rare, and worth a
//	     distinct code because it means something is wrong with the sender, not the auth.
//	500  we failed to publish an authentic, wanted event. This is the ONLY case that
//	     asks GitHub to retry, because it is the only failure a retry can fix.
//
// # Why only a publish failure returns 500
//
// A 5xx makes GitHub redeliver. Redelivery is welcome for a transient EventBridge
// failure — the event is wanted and was not published, so trying again is exactly
// right, and the delivery id stays the same so nothing downstream double-processes. But
// a 5xx for a bad signature or malformed body would make GitHub retry something that
// will fail identically forever, which is how you turn one broken delivery into a
// retry storm. So those are 4xx: terminal, by design.
func (h *Handler) Handle(ctx context.Context, req Request) (Response, Result) {
	start := time.Now()

	// 1. VERIFY — before anything else touches the body. An unverified request is not
	// decoded, not logged in detail, not acted on. This ordering is the security
	// property; everything else is plumbing.
	sig := req.Headers.Get(SignatureHeader)
	if err := VerifySignature(req.Body, sig, h.Cfg.Secret); err != nil {
		// Log the failure with the delivery id if present (it is unverified, but it is
		// only an identifier, and it is what ties a rejected request to GitHub's UI).
		// Do NOT log the body: an unauthenticated body is attacker-controlled input.
		h.Log.Warn("signature verification failed",
			"error", err,
			"errorKind", signatureErrorKind(err),
			"deliveryId", req.Headers.Get(DeliveryHeader),
			"event", req.Headers.Get(EventHeader),
			"bodyBytes", len(req.Body),
		)
		if errors.Is(err, ErrNoSecret) {
			// A misconfiguration, not a bad request: the endpoint cannot verify anything.
			// Fail closed with a 500 — but this is a deploy-time bug, and the alarm on it
			// is the point.
			return Response{StatusCode: 500, Body: `{"error":"webhook not configured"}`},
				Result{Disposition: Ignored, Reason: "no secret configured"}
		}
		return Response{StatusCode: 401, Body: `{"error":"invalid signature"}`},
			Result{Disposition: Ignored, Reason: "invalid signature"}
	}

	// 2. PARSE — now that the bytes are trusted.
	delivery, err := Parse(req.Headers, req.Body)
	if err != nil {
		h.Log.Warn("could not parse webhook", "error", err,
			"event", req.Headers.Get(EventHeader), "deliveryId", req.Headers.Get(DeliveryHeader))
		return Response{StatusCode: 400, Body: `{"error":"malformed webhook"}`},
			Result{Disposition: Ignored, Reason: err.Error()}
	}

	log := h.Log.With(
		"deliveryId", delivery.DeliveryID,
		"correlationId", delivery.CorrelationID(),
		"event", delivery.Event,
		"action", delivery.Action,
		"repository", delivery.Repository,
		"branch", delivery.Branch,
	)

	result := Result{
		Event:      delivery.Event,
		DeliveryID: delivery.DeliveryID,
		Repository: delivery.Repository,
	}

	// 3. FILTER — decide whether the platform cares, before publishing anything.
	decision := Filter(delivery, h.Cfg)
	result.Disposition = decision.Disposition
	result.Reason = decision.Reason

	if decision.Disposition != Accepted {
		// Ignored or acknowledged: authentic, and nothing to publish. A 200, because the
		// request was fine. This is the common case for a busy repository, and it is a
		// success, logged at INFO so the volume of "correctly did nothing" is visible.
		log.Info("webhook received; not published",
			"disposition", string(decision.Disposition),
			"reason", decision.Reason,
			"durationMs", time.Since(start).Milliseconds(),
		)
		return Response{StatusCode: 200, Body: okBody(result)}, result
	}

	// 4. PUBLISH — the one thing this Lambda exists to do.
	if err := h.publish(ctx, delivery); err != nil {
		log.Error("could not publish event to EventBridge",
			"error", err, "bus", h.Cfg.EventBus,
			"durationMs", time.Since(start).Milliseconds())
		// The ONLY 500. The event is authentic and wanted and was not published, so ask
		// GitHub to redeliver — same delivery id, so this is safe to repeat.
		return Response{StatusCode: 500, Body: `{"error":"could not publish event"}`}, result
	}

	result.Published = true
	log.Info("webhook published to EventBridge",
		"bus", h.Cfg.EventBus, "source", h.Cfg.EventSource, "detailType", DetailType,
		"durationMs", time.Since(start).Milliseconds())
	return Response{StatusCode: 202, Body: okBody(result)}, result
}

// publish puts the curated event on the platform bus, and — critically — checks that
// EventBridge actually accepted it. PutEvents answers 200 even when it accepted none of
// the entries; the per-entry failures are in the body, and not reading them is the
// classic way to build an event pipeline that silently drops events (the spot package
// makes the same point, because it is the same trap).
func (h *Handler) publish(ctx context.Context, d Delivery) error {
	detail, err := json.Marshal(GitHubEvent{
		CorrelationID: d.CorrelationID(),
		DeliveryID:    d.DeliveryID,
		Event:         d.Event,
		Action:        d.Action,
		Repository:    d.Repository,
		Branch:        d.Branch,
		Ref:           d.Ref,
		RefType:       d.RefType,
		HeadSHA:       d.HeadSHA,
		Sender:        d.Sender,
		Private:       d.Private,
		Project:       h.Cfg.Project,
		Environment:   h.Cfg.Environment,
	})
	if err != nil {
		return fmt.Errorf("encoding event: %w", err)
	}

	out, err := h.Events.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []ebtypes.PutEventsRequestEntry{{
			EventBusName: aws.String(h.Cfg.EventBus),
			Source:       aws.String(h.Cfg.EventSource),
			DetailType:   aws.String(DetailType),
			Detail:       aws.String(string(detail)),
			Resources:    []string{d.Repository},
		}},
	})
	if err != nil {
		return fmt.Errorf("putting event on %s: %w", h.Cfg.EventBus, err)
	}
	if out.FailedEntryCount > 0 {
		reason := "unknown"
		if len(out.Entries) > 0 {
			reason = fmt.Sprintf("%s: %s",
				aws.ToString(out.Entries[0].ErrorCode), aws.ToString(out.Entries[0].ErrorMessage))
		}
		return fmt.Errorf("event bus %s rejected the event (%s)", h.Cfg.EventBus, reason)
	}
	return nil
}

// signatureErrorKind maps a verification error to a stable label for logs and alarms —
// the sentinel's name, not the message. "no_signature" and "bad_signature" mean
// different things operationally (a misconfigured hook vs a forgery attempt), and an
// alarm that wants to fire on forgeries should not fire every time someone installs a
// hook without a secret.
func signatureErrorKind(err error) string {
	switch {
	case errors.Is(err, ErrNoSecret):
		return "no_secret"
	case errors.Is(err, ErrNoSignature):
		return "no_signature"
	case errors.Is(err, ErrBadSignature):
		return "bad_signature"
	default:
		return "unknown"
	}
}

func okBody(r Result) string {
	b, err := json.Marshal(r)
	if err != nil {
		return `{"disposition":"` + string(r.Disposition) + `"}`
	}
	return string(b)
}
