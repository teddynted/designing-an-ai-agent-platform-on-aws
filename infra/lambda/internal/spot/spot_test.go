package spot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/smithy-go"
)

const (
	testInstanceID = "i-0123456789abcdef0"
	testEventTime  = "2026-07-13T12:00:00Z"
)

// --- fakes ------------------------------------------------------------------

// fakeEC2 answers DescribeInstances with a single configurable instance.
type fakeEC2 struct {
	instanceType string
	lifecycle    ec2types.InstanceLifecycleType
	tags         map[string]string
	notFound     bool // the API answers InvalidInstanceID.NotFound
	empty        bool // the API answers 200 with no reservations
	err          error

	calls int
}

// notFoundErr is the smithy error EC2 returns for an instance that no longer
// exists, which the handler must treat as an answer rather than a failure.
type notFoundErr struct{ smithy.APIError }

func (notFoundErr) ErrorCode() string    { return "InvalidInstanceID.NotFound" }
func (notFoundErr) ErrorMessage() string { return "The instance ID does not exist" }
func (notFoundErr) Error() string        { return "InvalidInstanceID.NotFound" }

func (f *fakeEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.calls++
	switch {
	case f.err != nil:
		return nil, f.err
	case f.notFound:
		return nil, notFoundErr{}
	case f.empty:
		return &ec2.DescribeInstancesOutput{}, nil
	}

	tags := make([]ec2types.Tag, 0, len(f.tags))
	for key, value := range f.tags {
		tags = append(tags, ec2types.Tag{Key: aws.String(key), Value: aws.String(value)})
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{
			Instances: []ec2types.Instance{{
				InstanceId:        aws.String(testInstanceID),
				InstanceType:      ec2types.InstanceType(f.instanceType),
				InstanceLifecycle: f.lifecycle,
				Tags:              tags,
			}},
		}},
	}, nil
}

// fakeEvents records what was published, and can fail either way PutEvents can:
// with an error, or with a 200 that accepted nothing.
type fakeEvents struct {
	err        error
	failEntry  bool
	published  []ebtypes.PutEventsRequestEntry
	callCount  int
	lastDetail Notification
}

func (f *fakeEvents) PutEvents(_ context.Context, in *eventbridge.PutEventsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error) {
	f.callCount++
	if f.err != nil {
		return nil, f.err
	}
	if f.failEntry {
		return &eventbridge.PutEventsOutput{
			FailedEntryCount: 1,
			Entries: []ebtypes.PutEventsResultEntry{{
				ErrorCode:    aws.String("NotAuthorizedForSourceException"),
				ErrorMessage: aws.String("not authorized"),
			}},
		}, nil
	}
	f.published = append(f.published, in.Entries...)
	if len(in.Entries) > 0 {
		_ = json.Unmarshal([]byte(aws.ToString(in.Entries[0].Detail)), &f.lastDetail)
	}
	return &eventbridge.PutEventsOutput{FailedEntryCount: 0}, nil
}

// fakeMetrics records the metric data it was given.
type fakeMetrics struct {
	err   error
	calls []*cloudwatch.PutMetricDataInput
}

func (f *fakeMetrics) PutMetricData(_ context.Context, in *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, in)
	return &cloudwatch.PutMetricDataOutput{}, nil
}

func (f *fakeMetrics) name() string {
	if len(f.calls) == 0 || len(f.calls[0].MetricData) == 0 {
		return ""
	}
	return aws.ToString(f.calls[0].MetricData[0].MetricName)
}

// --- helpers ----------------------------------------------------------------

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// ownedTags are the tags the compute stack puts on the platform's instances.
func ownedTags() map[string]string {
	return map[string]string{TagProject: "aiap", TagEnvironment: "dev"}
}

func newHandler(ec2api *fakeEC2, events *fakeEvents, metrics *fakeMetrics, accepts ...Kind) *Handler {
	if len(accepts) == 0 {
		accepts = []Kind{KindInterruption, KindRebalance, KindStateChange}
	}
	return &Handler{
		Cfg: Config{
			Project:     "aiap",
			Environment: "dev",
			EventBus:    "aiap-dev-bus",
			EventSource: "aiap.dev.platform",
			Namespace:   "aiap/spot",
		},
		EC2:     ec2api,
		Events:  events,
		Metrics: metrics,
		Log:     discardLogger(),
		Accepts: accepts,
	}
}

// interruptionEvent is the payload EC2 delivers for a two-minute warning.
func interruptionEvent(action string) json.RawMessage {
	return json.RawMessage(`{
	  "version": "0",
	  "detail-type": "EC2 Spot Instance Interruption Warning",
	  "source": "aws.ec2",
	  "time": "` + testEventTime + `",
	  "resources": ["arn:aws:ec2:us-east-1:123456789012:instance/` + testInstanceID + `"],
	  "detail": {"instance-id": "` + testInstanceID + `", "instance-action": "` + action + `"}
	}`)
}

func stateChangeEvent(state string) json.RawMessage {
	return json.RawMessage(`{
	  "version": "0",
	  "detail-type": "EC2 Instance State-change Notification",
	  "source": "aws.ec2",
	  "time": "` + testEventTime + `",
	  "detail": {"instance-id": "` + testInstanceID + `", "state": "` + state + `"}
	}`)
}

func rebalanceEvent() json.RawMessage {
	return json.RawMessage(`{
	  "version": "0",
	  "detail-type": "EC2 Instance Rebalance Recommendation",
	  "source": "aws.ec2",
	  "time": "` + testEventTime + `",
	  "detail": {"instance-id": "` + testInstanceID + `"}
	}`)
}

func handle(t *testing.T, h *Handler, raw json.RawMessage) (Result, error) {
	t.Helper()
	return h.Handle(context.Background(), raw)
}

// --- parsing ----------------------------------------------------------------

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		raw        json.RawMessage
		wantKind   Kind
		wantAction string
		wantState  string
	}{
		{"interruption", interruptionEvent("terminate"), KindInterruption, "terminate", ""},
		{"interruption to stop", interruptionEvent("stop"), KindInterruption, "stop", ""},
		{"rebalance", rebalanceEvent(), KindRebalance, "", ""},
		{"state change", stateChangeEvent("terminated"), KindStateChange, "", "terminated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := Parse(tt.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if event.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", event.Kind, tt.wantKind)
			}
			if event.InstanceID != testInstanceID {
				t.Errorf("InstanceID = %q, want %q", event.InstanceID, testInstanceID)
			}
			if event.Action != tt.wantAction {
				t.Errorf("Action = %q, want %q", event.Action, tt.wantAction)
			}
			if event.State != tt.wantState {
				t.Errorf("State = %q, want %q", event.State, tt.wantState)
			}
			if event.Time.Format(time.RFC3339) != testEventTime {
				t.Errorf("Time = %v, want %s", event.Time, testEventTime)
			}
		})
	}
}

func TestParseRejectsUnknownDetailType(t *testing.T) {
	raw := json.RawMessage(`{"detail-type":"EC2 Instance Rebooted","detail":{"instance-id":"i-x"}}`)
	if _, err := Parse(raw); !errors.Is(err, ErrUnknownDetailType) {
		t.Errorf("Parse = %v, want ErrUnknownDetailType", err)
	}
}

func TestParseRejectsMissingInstanceID(t *testing.T) {
	raw := json.RawMessage(`{"detail-type":"EC2 Spot Instance Interruption Warning","detail":{}}`)
	if _, err := Parse(raw); !errors.Is(err, ErrNoInstanceID) {
		t.Errorf("Parse = %v, want ErrNoInstanceID", err)
	}
}

// The drain window is the whole point of the warning: it must be measured from
// the event's own timestamp, not from whenever the Lambda happened to run.
func TestDeadlineIsTwoMinutesAfterTheNotice(t *testing.T) {
	event, err := Parse(interruptionEvent("terminate"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := "2026-07-13T12:02:00Z"
	if got := event.Deadline().UTC().Format(time.RFC3339); got != want {
		t.Errorf("Deadline = %s, want %s", got, want)
	}
}

// --- ownership --------------------------------------------------------------

// The rules fire for every instance in the region, so the overwhelmingly common
// case is an event about an instance that is not ours. It must be a quiet,
// successful no-op — never an error, and never a metric.
func TestForeignInstanceIsIgnored(t *testing.T) {
	tests := []struct {
		name string
		tags map[string]string
	}{
		{"no tags at all", nil},
		{"another project", map[string]string{TagProject: "someone-else", TagEnvironment: "dev"}},
		{"same project, another environment", map[string]string{TagProject: "aiap", TagEnvironment: "prod"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, metrics := &fakeEvents{}, &fakeMetrics{}
			h := newHandler(&fakeEC2{instanceType: "t3.xlarge", tags: tt.tags}, events, metrics)

			res, err := handle(t, h, interruptionEvent("terminate"))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if res.Owned || res.Handled {
				t.Errorf("Owned = %v, Handled = %v; want both false", res.Owned, res.Handled)
			}
			if events.callCount != 0 {
				t.Error("a foreign instance must not be published to the platform bus")
			}
			if len(metrics.calls) != 0 {
				t.Error("a foreign instance must not be counted in this platform's metrics")
			}
		})
	}
}

func TestOwnedInterruptionIsHandled(t *testing.T) {
	events, metrics := &fakeEvents{}, &fakeMetrics{}
	h := newHandler(&fakeEC2{
		instanceType: "t3.xlarge",
		lifecycle:    ec2types.InstanceLifecycleTypeSpot,
		tags:         ownedTags(),
	}, events, metrics)

	res, err := handle(t, h, interruptionEvent("terminate"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !res.Owned || !res.Handled || !res.Notified {
		t.Errorf("Owned/Handled/Notified = %v/%v/%v; want all true", res.Owned, res.Handled, res.Notified)
	}
	if res.Metric != "InterruptionWarnings" {
		t.Errorf("Metric = %q, want InterruptionWarnings", res.Metric)
	}
	if res.Lifecycle != "spot" {
		t.Errorf("Lifecycle = %q, want spot", res.Lifecycle)
	}
	if res.DrainDeadline != "2026-07-13T12:02:00Z" {
		t.Errorf("DrainDeadline = %q, want 2026-07-13T12:02:00Z", res.DrainDeadline)
	}
	if got := metrics.name(); got != "InterruptionWarnings" {
		t.Errorf("metric published = %q, want InterruptionWarnings", got)
	}
}

// The platform bus is a different bus from the one the EC2 event arrived on, and
// the rule listening on it filters by source. Publishing with the wrong source,
// or onto the wrong bus, is a silent no-op in production — so pin both.
func TestNotificationIsAddressedToThePlatformBus(t *testing.T) {
	events, metrics := &fakeEvents{}, &fakeMetrics{}
	h := newHandler(&fakeEC2{instanceType: "t3.xlarge", lifecycle: ec2types.InstanceLifecycleTypeSpot, tags: ownedTags()}, events, metrics)

	if _, err := handle(t, h, interruptionEvent("terminate")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(events.published) != 1 {
		t.Fatalf("published %d events, want 1", len(events.published))
	}

	entry := events.published[0]
	if got := aws.ToString(entry.EventBusName); got != "aiap-dev-bus" {
		t.Errorf("EventBusName = %q, want aiap-dev-bus", got)
	}
	if got := aws.ToString(entry.Source); got != "aiap.dev.platform" {
		t.Errorf("Source = %q, want aiap.dev.platform", got)
	}
	if got := aws.ToString(entry.DetailType); got != "Spot Interruption Warning" {
		t.Errorf("DetailType = %q, want Spot Interruption Warning", got)
	}

	// The detail must carry enough for a subscriber to act without calling EC2.
	detail := events.lastDetail
	if detail.InstanceID != testInstanceID || detail.InstanceType != "t3.xlarge" {
		t.Errorf("detail = %+v, want the instance and its type", detail)
	}
	if detail.DrainDeadline != "2026-07-13T12:02:00Z" {
		t.Errorf("detail.DrainDeadline = %q, want the deadline", detail.DrainDeadline)
	}
}

// --- state changes ----------------------------------------------------------

func TestStateChangeMetrics(t *testing.T) {
	tests := []struct {
		state       string
		wantMetric  string
		wantHandled bool
	}{
		{"running", "InstancesLaunched", true},
		{"stopped", "InstancesStopped", true},
		{"terminated", "InstancesTerminated", true},
		// Transitional states have no rule and no metric. If one arrives anyway it
		// is a no-op, not a failure.
		{"pending", "", false},
		{"stopping", "", false},
		{"shutting-down", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			events, metrics := &fakeEvents{}, &fakeMetrics{}
			h := newHandler(&fakeEC2{instanceType: "t3.xlarge", tags: ownedTags()}, events, metrics, KindStateChange)

			res, err := handle(t, h, stateChangeEvent(tt.state))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if res.Handled != tt.wantHandled {
				t.Errorf("Handled = %v, want %v", res.Handled, tt.wantHandled)
			}
			if res.Metric != tt.wantMetric {
				t.Errorf("Metric = %q, want %q", res.Metric, tt.wantMetric)
			}
			if got := metrics.name(); got != tt.wantMetric {
				t.Errorf("metric published = %q, want %q", got, tt.wantMetric)
			}
		})
	}
}

// An untracked state must not even cost a DescribeInstances call: the rules
// never subscribe to it, so paying for the lookup would be pure waste.
func TestUntrackedStateDoesNotCallEC2(t *testing.T) {
	ec2api := &fakeEC2{instanceType: "t3.xlarge", tags: ownedTags()}
	h := newHandler(ec2api, &fakeEvents{}, &fakeMetrics{}, KindStateChange)

	if _, err := handle(t, h, stateChangeEvent("pending")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if ec2api.calls != 0 {
		t.Errorf("DescribeInstances called %d times, want 0", ec2api.calls)
	}
}

// --- routing ----------------------------------------------------------------

// Each function is wired to one rule set. An event arriving at the wrong
// function means a rule points at the wrong target — a deployment bug that must
// be loud, because the "right" function never sees the event at all.
func TestMisroutedEventIsAnError(t *testing.T) {
	h := newHandler(&fakeEC2{tags: ownedTags()}, &fakeEvents{}, &fakeMetrics{}, KindStateChange)

	_, err := handle(t, h, interruptionEvent("terminate"))
	if !errors.Is(err, ErrUnexpectedKind) {
		t.Errorf("Handle = %v, want ErrUnexpectedKind", err)
	}
}

func TestInterruptionHandlerAcceptsRebalance(t *testing.T) {
	events, metrics := &fakeEvents{}, &fakeMetrics{}
	h := newHandler(&fakeEC2{
		instanceType: "t3.xlarge",
		lifecycle:    ec2types.InstanceLifecycleTypeSpot,
		tags:         ownedTags(),
	}, events, metrics, KindInterruption, KindRebalance)

	res, err := handle(t, h, rebalanceEvent())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Handled || res.Metric != "RebalanceRecommendations" {
		t.Errorf("Handled = %v, Metric = %q; want true, RebalanceRecommendations", res.Handled, res.Metric)
	}
	// A rebalance recommendation is advisory: nothing is being reclaimed yet, so
	// there is no deadline to publish.
	if res.DrainDeadline != "" {
		t.Errorf("DrainDeadline = %q, want empty for an advisory signal", res.DrainDeadline)
	}
}

// --- failures ---------------------------------------------------------------

// An instance EC2 has already forgotten cannot be attributed to a project, and
// guessing would either miscount someone else's instance or drop one of ours.
// Say so, and do nothing.
func TestVanishedInstanceIsANoOp(t *testing.T) {
	for _, tt := range []struct {
		name   string
		ec2api *fakeEC2
	}{
		{"API says not found", &fakeEC2{notFound: true}},
		{"API returns no reservations", &fakeEC2{empty: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events, metrics := &fakeEvents{}, &fakeMetrics{}
			h := newHandler(tt.ec2api, events, metrics)

			res, err := handle(t, h, stateChangeEvent("terminated"))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if res.Handled || res.Owned {
				t.Error("a vanished instance must not be handled or claimed")
			}
			if events.callCount != 0 || len(metrics.calls) != 0 {
				t.Error("a vanished instance must produce no metric and no event")
			}
		})
	}
}

// A DescribeInstances failure is not an answer — it is a failure. It must
// surface so Lambda retries, rather than being mistaken for "not ours".
func TestDescribeFailureSurfaces(t *testing.T) {
	h := newHandler(&fakeEC2{err: errors.New("throttled")}, &fakeEvents{}, &fakeMetrics{})

	if _, err := handle(t, h, interruptionEvent("terminate")); err == nil {
		t.Error("a DescribeInstances failure must be returned")
	}
}

// PutEvents answers 200 even when it accepted nothing. Trusting the status code
// is how an event pipeline silently drops events; the entry count is the truth.
func TestRejectedEntryIsAFailure(t *testing.T) {
	events := &fakeEvents{failEntry: true}
	metrics := &fakeMetrics{}
	h := newHandler(&fakeEC2{instanceType: "t3.xlarge", tags: ownedTags()}, events, metrics)

	res, err := handle(t, h, interruptionEvent("terminate"))
	if err == nil {
		t.Fatal("a rejected entry must be returned as an error, so Lambda retries")
	}
	if res.Notified {
		t.Error("Notified must be false when the bus rejected the entry")
	}
	// The metric still went out: one recording failing must not suppress the other.
	if metrics.name() != "InterruptionWarnings" {
		t.Error("the metric should still have been published")
	}
}

// The two recordings are independent. A CloudWatch outage must not stop the
// platform event that later milestones react to — and both failures must be
// reported, not just the first.
func TestBothFailuresAreReported(t *testing.T) {
	events := &fakeEvents{err: errors.New("bus down")}
	metrics := &fakeMetrics{err: errors.New("cloudwatch down")}
	h := newHandler(&fakeEC2{instanceType: "t3.xlarge", tags: ownedTags()}, events, metrics)

	res, err := handle(t, h, interruptionEvent("terminate"))
	if err == nil {
		t.Fatal("both recordings failing must return an error")
	}
	if !res.Handled {
		t.Error("Handled should still record that the event was ours and acted on")
	}
	for _, want := range []string{"cloudwatch down", "bus down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestMetricFailureStillEmitsPlatformEvent(t *testing.T) {
	events := &fakeEvents{}
	metrics := &fakeMetrics{err: errors.New("cloudwatch down")}
	h := newHandler(&fakeEC2{instanceType: "t3.xlarge", tags: ownedTags()}, events, metrics)

	res, _ := handle(t, h, interruptionEvent("terminate"))
	if !res.Notified || events.callCount != 1 {
		t.Error("a metric failure must not suppress the platform event")
	}
}

func TestMalformedEventIsAnError(t *testing.T) {
	h := newHandler(&fakeEC2{}, &fakeEvents{}, &fakeMetrics{})

	if _, err := handle(t, h, json.RawMessage(`{"detail-type":`)); err == nil {
		t.Error("a malformed event must be returned as an error")
	}
}

// --- configuration ----------------------------------------------------------

// A handler with no configuration would publish into the void and report
// success. It must refuse to start instead.
func TestConfigFromEnvRequiresEverything(t *testing.T) {
	for _, missing := range []string{EnvProject, EnvEnvironment, EnvEventBus, EnvEventSource, EnvNamespace} {
		t.Run("missing "+missing, func(t *testing.T) {
			for name, value := range map[string]string{
				EnvProject:     "aiap",
				EnvEnvironment: "dev",
				EnvEventBus:    "aiap-dev-bus",
				EnvEventSource: "aiap.dev.platform",
				EnvNamespace:   "aiap/spot",
			} {
				if name == missing {
					t.Setenv(name, "")
					continue
				}
				t.Setenv(name, value)
			}
			if _, err := ConfigFromEnv(); err == nil {
				t.Errorf("ConfigFromEnv should fail when %s is unset", missing)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvProject, "aiap")
	t.Setenv(EnvEnvironment, "dev")
	t.Setenv(EnvEventBus, "aiap-dev-bus")
	t.Setenv(EnvEventSource, "aiap.dev.platform")
	t.Setenv(EnvNamespace, "aiap/spot")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Project != "aiap" || cfg.EventBus != "aiap-dev-bus" || cfg.Namespace != "aiap/spot" {
		t.Errorf("Config = %+v, want the environment's values", cfg)
	}
}
