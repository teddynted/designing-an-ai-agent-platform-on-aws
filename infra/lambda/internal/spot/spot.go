// Package spot handles the EC2 lifecycle events AWS emits when it reclaims,
// rebalances, or changes the state of the platform's Spot capacity.
//
// # What this package can and cannot do
//
// A Spot interruption warning arrives about two minutes before AWS reclaims the
// instance. Two minutes is a long time for a control plane and no time at all
// for a Lambda that wants to save someone else's work: this handler runs in the
// account, not on the instance, so it cannot flush a model's KV cache, finish an
// in-flight inference, or upload a half-written artifact. That is the job of the
// on-host drain agent in the compute stack's user data, which polls the instance
// metadata service and reacts in-process.
//
// So this package deliberately does the things only an account-side handler can
// do, and nothing it would have to lie about:
//
//   - record that the interruption happened, as a CloudWatch metric, so the
//     interruption rate of an instance type is a number rather than a feeling;
//   - re-publish the event onto the platform's own EventBridge bus, which is the
//     seam later milestones hook into (drain a queue, reschedule a job, launch a
//     replacement) without touching this code;
//   - log it, in structured form, so a post-mortem has something to read.
//
// # Why events are filtered by tag
//
// EC2 emits these events on the account's default bus for every instance in the
// region, including instances this platform has never heard of. The event
// payload carries no tags, so the handler resolves the instance and checks that
// it is tagged for this project and environment. An event for someone else's
// instance is a successful no-op, never an error — a handler that failed on
// other people's instances would page you for someone else's deploy.
package spot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/smithy-go"
)

// The EventBridge detail-types this package understands. They are AWS's, spelled
// exactly as EC2 emits them; the EventBridge rules match on the same strings.
const (
	DetailTypeInterruption = "EC2 Spot Instance Interruption Warning"
	DetailTypeRebalance    = "EC2 Instance Rebalance Recommendation"
	DetailTypeStateChange  = "EC2 Instance State-change Notification"
)

// Tags the compute stack puts on every instance it launches. Ownership is
// decided by these, because the events themselves carry no tags.
const (
	TagProject     = "Project"
	TagEnvironment = "Environment"
)

// DrainWindow is how long AWS gives a Spot instance between the interruption
// warning and the reclaim. It is a ceiling, not a promise: capacity can be taken
// sooner, so a drain that needs the full two minutes is already too slow.
const DrainWindow = 2 * time.Minute

// Kind classifies an event by what it means, rather than by how it is spelled.
type Kind string

const (
	// KindInterruption is the two-minute warning: this instance is going away.
	KindInterruption Kind = "spot-interruption"
	// KindRebalance is the earlier, softer signal that this instance is at
	// elevated risk of interruption. It is advisory: nothing is reclaimed yet.
	KindRebalance Kind = "rebalance-recommendation"
	// KindStateChange is an instance entering a new state (running, stopped,
	// terminated, ...) for any reason, Spot-related or not.
	KindStateChange Kind = "state-change"
)

// Event is the part of an EventBridge envelope this package needs. The unused
// envelope fields (id, account, version, ...) are ignored deliberately.
type Event struct {
	Kind       Kind
	DetailType string
	InstanceID string
	// Action is the interruption's verb — terminate, stop, or hibernate — and is
	// set only on KindInterruption.
	Action string
	// State is the instance's new state and is set only on KindStateChange.
	State string
	// Time is when EC2 emitted the event. For an interruption it is the start of
	// the drain window, so it is the clock the deadline is measured from.
	Time time.Time
}

// envelope mirrors the JSON EventBridge delivers. The three detail fields are
// unioned into one struct because only one of them is ever populated per event.
type envelope struct {
	DetailType string    `json:"detail-type"`
	Source     string    `json:"source"`
	Time       time.Time `json:"time"`
	Detail     struct {
		InstanceID     string `json:"instance-id"`
		InstanceAction string `json:"instance-action"`
		State          string `json:"state"`
	} `json:"detail"`
}

// ErrUnknownDetailType is returned when an event arrives that no rule in this
// stack should have delivered. It is an error, not a no-op: it means a rule is
// misconfigured, and silently dropping it would hide that.
var ErrUnknownDetailType = errors.New("unknown detail-type")

// ErrNoInstanceID is returned for an event whose detail carries no instance.
var ErrNoInstanceID = errors.New("event has no instance-id")

// Parse turns a raw EventBridge payload into an Event.
func Parse(raw json.RawMessage) (Event, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Event{}, fmt.Errorf("decoding event: %w", err)
	}

	var kind Kind
	switch env.DetailType {
	case DetailTypeInterruption:
		kind = KindInterruption
	case DetailTypeRebalance:
		kind = KindRebalance
	case DetailTypeStateChange:
		kind = KindStateChange
	default:
		return Event{}, fmt.Errorf("%w: %q", ErrUnknownDetailType, env.DetailType)
	}

	if env.Detail.InstanceID == "" {
		return Event{}, fmt.Errorf("%w: detail-type %q", ErrNoInstanceID, env.DetailType)
	}

	return Event{
		Kind:       kind,
		DetailType: env.DetailType,
		InstanceID: env.Detail.InstanceID,
		Action:     env.Detail.InstanceAction,
		State:      env.Detail.State,
		Time:       env.Time,
	}, nil
}

// Deadline is the instant by which a drain must be finished: the end of the
// two-minute window that opened when the warning was emitted. It is meaningful
// only for an interruption.
func (e Event) Deadline() time.Time { return e.Time.Add(DrainWindow) }

// metricName maps an event to the CloudWatch metric that counts it, and reports
// whether the event is one this platform tracks at all.
//
// Only three of the seven EC2 instance states are counted. The transitional ones
// (pending, stopping, shutting-down) are noise — every one of them is followed by
// the terminal state that is counted — and no rule in this stack subscribes to
// them. An event for one arriving anyway is a no-op, not an error.
func metricName(e Event) (string, bool) {
	switch e.Kind {
	case KindInterruption:
		return "InterruptionWarnings", true
	case KindRebalance:
		return "RebalanceRecommendations", true
	case KindStateChange:
		switch e.State {
		case "running":
			return "InstancesLaunched", true
		case "stopped":
			return "InstancesStopped", true
		case "terminated":
			return "InstancesTerminated", true
		}
	}
	return "", false
}

// detailType is the name this platform re-publishes the event under, on its own
// bus. It is deliberately not the AWS spelling: a subscriber on the platform bus
// should not have to know that this event began life as an EC2 service event.
func detailType(e Event) string {
	switch e.Kind {
	case KindInterruption:
		return "Spot Interruption Warning"
	case KindRebalance:
		return "Spot Rebalance Recommendation"
	default:
		return "EC2 Instance State Change"
	}
}

// EC2API is the slice of the EC2 client this package uses. It exists so tests can
// substitute a fake instead of an AWS account.
type EC2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// EventsAPI is the slice of the EventBridge client this package uses.
type EventsAPI interface {
	PutEvents(context.Context, *eventbridge.PutEventsInput, ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error)
}

// MetricsAPI is the slice of the CloudWatch client this package uses.
type MetricsAPI interface {
	PutMetricData(context.Context, *cloudwatch.PutMetricDataInput, ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

// Instance is what the handler needs to know about the instance an event names.
type Instance struct {
	ID string
	// Type is the instance type, carried into the metric as a dimension: the
	// interruption rate that matters is per instance type, not per fleet.
	Type string
	// Lifecycle is "spot" or "on-demand". A state-change event for an On-Demand
	// instance is perfectly normal; it is simply not a Spot interruption.
	Lifecycle string
	// Owned reports whether the instance is tagged for this project/environment.
	Owned bool
}

// Config is the handler's deployment-specific wiring, all of it supplied by the
// CloudFormation stack as environment variables. Nothing here is hard-coded.
type Config struct {
	Project     string
	Environment string
	// EventBus is the platform's own bus — not the default bus the EC2 events
	// arrive on. Re-publishing crosses from one to the other.
	EventBus string
	// EventSource must match the source the platform bus's rule filters on,
	// otherwise the event is published to a bus where nothing is listening.
	EventSource string
	// Namespace is the CloudWatch namespace the metrics land in. The Lambda's IAM
	// policy allows PutMetricData only into this namespace.
	Namespace string
}

// Result is the structured outcome of one invocation. It is returned to Lambda,
// so it shows up directly in a console test, a CLI invoke, and the logs.
type Result struct {
	InstanceID   string `json:"instanceId"`
	Kind         Kind   `json:"kind"`
	Action       string `json:"action,omitempty"`
	State        string `json:"state,omitempty"`
	InstanceType string `json:"instanceType,omitempty"`
	Lifecycle    string `json:"lifecycle,omitempty"`
	// Owned is false for an instance belonging to something else in the account.
	Owned bool `json:"owned"`
	// Handled is true only when the event was this platform's and was acted on.
	Handled bool `json:"handled"`
	// Notified is true when the event was re-published to the platform bus.
	Notified bool `json:"notified"`
	// Metric is the CloudWatch metric that was incremented, if any.
	Metric string `json:"metric,omitempty"`
	// DrainDeadline is when the instance is expected to be gone. Interruptions
	// only.
	DrainDeadline string `json:"drainDeadline,omitempty"`
	Message       string `json:"message"`
}

// Notification is the detail this platform re-publishes onto its own bus. It is
// flatter and friendlier than the EC2 event it came from: a subscriber gets the
// instance type and lifecycle already resolved, and a deadline it can act on.
type Notification struct {
	Kind          Kind   `json:"kind"`
	InstanceID    string `json:"instanceId"`
	InstanceType  string `json:"instanceType,omitempty"`
	Lifecycle     string `json:"lifecycle,omitempty"`
	Action        string `json:"action,omitempty"`
	State         string `json:"state,omitempty"`
	NoticeTime    string `json:"noticeTime"`
	DrainDeadline string `json:"drainDeadline,omitempty"`
	Project       string `json:"project"`
	Environment   string `json:"environment"`
}

// Handler processes one event. Its dependencies are interfaces so the whole of
// Handle is testable without an AWS account.
type Handler struct {
	Cfg     Config
	EC2     EC2API
	Events  EventsAPI
	Metrics MetricsAPI
	Log     *slog.Logger
	// Accepts is the set of kinds this function is wired to receive. The
	// interruption function and the state-change function are separate Lambdas
	// with separate log groups, and each rejects the other's events: an event
	// arriving at the wrong function means a rule is pointing at the wrong
	// target, and that should fail loudly rather than quietly work anyway.
	Accepts []Kind
}

// ErrUnexpectedKind means a rule delivered an event to the wrong function.
var ErrUnexpectedKind = errors.New("event kind not handled by this function")

// Handle processes a single EventBridge event.
//
// It returns an error only for a genuine failure — a malformed event, a
// misrouted rule, or an AWS call that did not succeed — because an error is what
// makes Lambda (and EventBridge's retry policy) try again. Everything else, and
// in particular an event about an instance this platform does not own, is a
// successful no-op.
func (h *Handler) Handle(ctx context.Context, raw json.RawMessage) (Result, error) {
	event, err := Parse(raw)
	if err != nil {
		// Log the raw payload: if it could not be parsed, the payload is the only
		// evidence of what went wrong.
		h.Log.Error("could not parse event", "error", err, "event", string(raw))
		return Result{}, err
	}

	log := h.Log.With(
		"kind", string(event.Kind),
		"instanceId", event.InstanceID,
		"detailType", event.DetailType,
		"eventTime", event.Time.UTC().Format(time.RFC3339),
	)

	result := Result{
		InstanceID: event.InstanceID,
		Kind:       event.Kind,
		Action:     event.Action,
		State:      event.State,
	}

	if !h.accepts(event.Kind) {
		err := fmt.Errorf("%w: %s", ErrUnexpectedKind, event.Kind)
		log.Error("misrouted event", "error", err, "accepts", h.Accepts)
		return result, err
	}

	if event.Kind == KindInterruption {
		result.DrainDeadline = event.Deadline().UTC().Format(time.RFC3339)
		// The single most useful line in the log during an interruption: what is
		// going away, when, and how long anything on it has left.
		log.Warn("spot interruption warning received",
			"action", event.Action,
			"drainDeadline", result.DrainDeadline,
			"drainWindow", DrainWindow.String(),
		)
	} else {
		log.Info("event received", "state", event.State)
	}

	metric, tracked := metricName(event)
	if !tracked {
		result.Message = fmt.Sprintf("state %q is not tracked; nothing to do", event.State)
		log.Info(result.Message)
		return result, nil
	}

	instance, found, err := h.describe(ctx, event.InstanceID)
	if err != nil {
		log.Error("could not describe instance", "error", err)
		return result, err
	}
	if !found {
		// The instance is already gone from the API. EC2 keeps a terminated
		// instance describable for roughly an hour, so this is rare — and since
		// ownership is decided by the instance's tags, an instance that cannot be
		// described cannot be attributed. Say so rather than guess.
		result.Message = "instance no longer exists; ownership cannot be determined"
		log.Warn(result.Message)
		return result, nil
	}

	result.InstanceType, result.Lifecycle, result.Owned = instance.Type, instance.Lifecycle, instance.Owned
	log = log.With("instanceType", instance.Type, "lifecycle", instance.Lifecycle)

	if !instance.Owned {
		// Expected and frequent: the rules match every instance in the region.
		result.Message = "instance is not tagged for this platform; ignoring"
		log.Info(result.Message, "project", h.Cfg.Project, "environment", h.Cfg.Environment)
		return result, nil
	}

	// Both actions are recordings of something that already happened, so neither
	// is a precondition of the other: attempt both, and report every failure,
	// rather than letting a CloudWatch blip suppress the platform event.
	var failures []error

	if err := h.record(ctx, metric, event, instance); err != nil {
		log.Error("could not publish metric", "error", err, "metric", metric)
		failures = append(failures, err)
	} else {
		result.Metric = metric
		log.Info("metric published", "metric", metric, "namespace", h.Cfg.Namespace)
	}

	if err := h.notify(ctx, event, instance, result.DrainDeadline); err != nil {
		log.Error("could not emit platform event", "error", err, "bus", h.Cfg.EventBus)
		failures = append(failures, err)
	} else {
		result.Notified = true
		log.Info("platform event emitted", "bus", h.Cfg.EventBus, "source", h.Cfg.EventSource)
	}

	result.Handled = true
	result.Message = fmt.Sprintf("%s handled", event.Kind)

	if len(failures) > 0 {
		// Returning the error asks Lambda to retry. Both actions are safe to
		// repeat: the metric is a count of events at a timestamp, and a duplicate
		// platform event is a duplicate notification, not a duplicate side effect.
		return result, errors.Join(failures...)
	}

	log.Info(result.Message, "notified", result.Notified, "metric", result.Metric)
	return result, nil
}

func (h *Handler) accepts(kind Kind) bool {
	for _, k := range h.Accepts {
		if k == kind {
			return true
		}
	}
	return false
}

// describe resolves the instance behind an event, and decides whether it is ours.
func (h *Handler) describe(ctx context.Context, instanceID string) (Instance, bool, error) {
	out, err := h.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		// A terminated instance eventually disappears entirely; asking about it
		// then is not a failure, it is an answer.
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidInstanceID.NotFound" {
			return Instance{}, false, nil
		}
		return Instance{}, false, fmt.Errorf("describing %s: %w", instanceID, err)
	}

	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			return Instance{
				ID:        instanceID,
				Type:      string(instance.InstanceType),
				Lifecycle: lifecycleOf(instance),
				Owned:     h.owns(instance.Tags),
			}, true, nil
		}
	}
	return Instance{}, false, nil
}

// lifecycleOf reports how the instance was purchased. EC2 sets InstanceLifecycle
// only for Spot and Scheduled instances; an empty value means On-Demand.
func lifecycleOf(instance ec2types.Instance) string {
	if instance.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot {
		return "spot"
	}
	return "on-demand"
}

// owns reports whether an instance's tags mark it as this platform's.
func (h *Handler) owns(tags []ec2types.Tag) bool {
	var project, environment string
	for _, tag := range tags {
		switch aws.ToString(tag.Key) {
		case TagProject:
			project = aws.ToString(tag.Value)
		case TagEnvironment:
			environment = aws.ToString(tag.Value)
		}
	}
	return project == h.Cfg.Project && environment == h.Cfg.Environment
}

// record counts the event in CloudWatch. The metric is dimensioned by instance
// type, because "how often is this interrupted?" is a question about a type — it
// is the number that decides whether a workload belongs on Spot at all.
func (h *Handler) record(ctx context.Context, metric string, event Event, instance Instance) error {
	_, err := h.Metrics.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(h.Cfg.Namespace),
		MetricData: []cwtypes.MetricDatum{{
			MetricName: aws.String(metric),
			Value:      aws.Float64(1),
			Unit:       cwtypes.StandardUnitCount,
			// The event's own timestamp, not now: a retried invocation must not
			// smear one interruption across two minutes of the graph.
			Timestamp: aws.Time(event.Time),
			Dimensions: []cwtypes.Dimension{
				{Name: aws.String("Environment"), Value: aws.String(h.Cfg.Environment)},
				{Name: aws.String("InstanceType"), Value: aws.String(instance.Type)},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("putting metric %s/%s: %w", h.Cfg.Namespace, metric, err)
	}
	return nil
}

// notify re-publishes the event onto the platform's own bus.
func (h *Handler) notify(ctx context.Context, event Event, instance Instance, deadline string) error {
	detail, err := json.Marshal(Notification{
		Kind:          event.Kind,
		InstanceID:    event.InstanceID,
		InstanceType:  instance.Type,
		Lifecycle:     instance.Lifecycle,
		Action:        event.Action,
		State:         event.State,
		NoticeTime:    event.Time.UTC().Format(time.RFC3339),
		DrainDeadline: deadline,
		Project:       h.Cfg.Project,
		Environment:   h.Cfg.Environment,
	})
	if err != nil {
		return fmt.Errorf("encoding notification: %w", err)
	}

	out, err := h.Events.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []ebtypes.PutEventsRequestEntry{{
			EventBusName: aws.String(h.Cfg.EventBus),
			Source:       aws.String(h.Cfg.EventSource),
			DetailType:   aws.String(detailType(event)),
			Detail:       aws.String(string(detail)),
			Resources:    []string{event.InstanceID},
			Time:         aws.Time(event.Time),
		}},
	})
	if err != nil {
		return fmt.Errorf("putting event on %s: %w", h.Cfg.EventBus, err)
	}

	// PutEvents answers 200 even when it accepted none of the entries: the
	// per-entry failures are in the body. Not reading them is the classic way to
	// build an event pipeline that silently drops events.
	if out.FailedEntryCount > 0 {
		reason := "unknown"
		if len(out.Entries) > 0 {
			reason = fmt.Sprintf("%s: %s",
				aws.ToString(out.Entries[0].ErrorCode),
				aws.ToString(out.Entries[0].ErrorMessage))
		}
		return fmt.Errorf("event bus %s rejected the event (%s)", h.Cfg.EventBus, reason)
	}
	return nil
}
