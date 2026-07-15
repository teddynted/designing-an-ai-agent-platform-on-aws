package webhook

import (
	"errors"
	"strings"
	"testing"
)

// testHeaders is a plain case-insensitive header lookup for tests.
type testHeaders map[string]string

func (h testHeaders) Get(name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func headers(event, delivery string) testHeaders {
	return testHeaders{EventHeader: event, DeliveryHeader: delivery}
}

func TestParseExtractsWhatRoutingNeeds(t *testing.T) {
	tests := []struct {
		name   string
		event  string
		body   string
		assert func(*testing.T, Delivery)
	}{
		{
			name:  "a push carries branch and head sha",
			event: EventPush,
			body:  `{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"o/r","default_branch":"main"},"sender":{"login":"alice"}}`,
			assert: func(t *testing.T, d Delivery) {
				if d.Branch != "main" || d.HeadSHA != "abc123" || d.Repository != "o/r" || d.Sender != "alice" {
					t.Errorf("push = %+v", d)
				}
			},
		},
		{
			name:  "a branch deletion push is normalised to Deleted",
			event: EventPush,
			body:  `{"ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000","deleted":true,"repository":{"full_name":"o/r"}}`,
			assert: func(t *testing.T, d Delivery) {
				if !d.Deleted || d.HeadSHA != "" {
					t.Errorf("a delete push should be Deleted with no head sha; got %+v", d)
				}
			},
		},
		{
			name:  "a tag push has no branch",
			event: EventPush,
			body:  `{"ref":"refs/tags/v1.0.0","after":"abc","repository":{"full_name":"o/r"}}`,
			assert: func(t *testing.T, d Delivery) {
				if d.Branch != "" {
					t.Errorf("a tag push has no branch; got %q", d.Branch)
				}
			},
		},
		{
			name:  "a release carries its action",
			event: EventRelease,
			body:  `{"action":"published","repository":{"full_name":"o/r"}}`,
			assert: func(t *testing.T, d Delivery) {
				if d.Action != "published" {
					t.Errorf("action = %q, want published", d.Action)
				}
			},
		},
		{
			name:  "create sets branch only for a branch ref_type",
			event: EventCreate,
			body:  `{"ref":"feature-x","ref_type":"branch","repository":{"full_name":"o/r"}}`,
			assert: func(t *testing.T, d Delivery) {
				if d.Branch != "feature-x" || d.RefType != "branch" {
					t.Errorf("create = %+v", d)
				}
			},
		},
		{
			name:  "delete is marked Deleted",
			event: EventDelete,
			body:  `{"ref":"old","ref_type":"branch","repository":{"full_name":"o/r"}}`,
			assert: func(t *testing.T, d Delivery) {
				if !d.Deleted {
					t.Error("a delete event should be Deleted")
				}
			},
		},
		{
			name:  "workflow_run carries head sha and branch",
			event: EventWorkflowRun,
			body:  `{"action":"completed","workflow_run":{"head_sha":"def456","head_branch":"main"},"repository":{"full_name":"o/r"}}`,
			assert: func(t *testing.T, d Delivery) {
				if d.HeadSHA != "def456" || d.Branch != "main" {
					t.Errorf("workflow_run = %+v", d)
				}
			},
		},
		{
			name:  "a fork/archived repository sets the flags",
			event: EventPush,
			body:  `{"ref":"refs/heads/main","repository":{"full_name":"o/r","fork":true,"archived":true,"private":true}}`,
			assert: func(t *testing.T, d Delivery) {
				if !d.Fork || !d.Archived || !d.Private {
					t.Errorf("flags = %+v", d)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Parse(headers(tc.event, "delivery-1"), []byte(tc.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if d.Event != tc.event || d.DeliveryID != "delivery-1" {
				t.Errorf("event/delivery = %q/%q", d.Event, d.DeliveryID)
			}
			tc.assert(t, d)
		})
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name    string
		headers testHeaders
		body    string
		want    error
	}{
		{"no event header", testHeaders{DeliveryHeader: "d"}, `{}`, ErrMissingHeader},
		{"no delivery header", testHeaders{EventHeader: "push"}, `{}`, ErrMissingHeader},
		{"malformed json", headers("push", "d"), `{not json`, ErrMalformedPayload},
		{"no repository in a repo event", headers("push", "d"), `{"ref":"refs/heads/main"}`, ErrMalformedPayload},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.headers, []byte(tc.body))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// The correlation id is the event and the delivery id, and stable for a delivery — the
// property the whole idempotency chain depends on. A redelivery has the same delivery
// id, so it produces the same correlation id, so downstream it is recognisably the same.
func TestCorrelationIDIsStableForADelivery(t *testing.T) {
	d, err := Parse(headers("push", "12345"), []byte(`{"ref":"refs/heads/main","repository":{"full_name":"o/r"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.CorrelationID() != "push:12345" {
		t.Errorf("correlationId = %q, want push:12345", d.CorrelationID())
	}
}

// ping is allowed to have no repository — GitHub sends it to confirm the endpoint, and
// on an org hook it carries no repo. Parsing it must not fail.
func TestPingParsesWithoutARepository(t *testing.T) {
	if _, err := Parse(headers("ping", "d"), []byte(`{"zen":"Anything added dilutes everything else."}`)); err != nil {
		t.Errorf("a ping without a repository should parse: %v", err)
	}
}
