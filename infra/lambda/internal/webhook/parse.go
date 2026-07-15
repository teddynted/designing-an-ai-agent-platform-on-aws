package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The GitHub events this platform understands. Adding one is a line here plus,
// usually, a field or two in [payload] — never a change to the handler, the filter,
// or the routing. That is the "additional events without modifying core
// architecture" the milestone asks for, and it is true because the handler treats an
// event as a name and a handful of extracted fields, not as a schema it branches on.
const (
	EventPush        = "push"
	EventRelease     = "release"
	EventCreate      = "create"
	EventDelete      = "delete"
	EventWorkflowRun = "workflow_run"
	EventRepository  = "repository"
	EventPing        = "ping"
)

// supportedEvents is the built-in set. It is the default; an operator can narrow it
// further with configuration, but never widen it beyond what the parser understands —
// a supported event the parser cannot read would be accepted and then fail, which is
// worse than refusing it up front.
var supportedEvents = map[string]bool{
	EventPush:        true,
	EventRelease:     true,
	EventCreate:      true,
	EventDelete:      true,
	EventWorkflowRun: true,
	EventRepository:  true,
	EventPing:        true,
}

// IsSupportedEvent reports whether the parser understands an event type at all.
func IsSupportedEvent(event string) bool { return supportedEvents[event] }

// ErrMissingHeader means a required GitHub header was absent. A real GitHub delivery
// always carries the event type and the delivery id; a request without them is not
// one, and is refused before any parsing.
var ErrMissingHeader = errors.New("missing required GitHub header")

// ErrMalformedPayload means the body was not the JSON this event should carry.
var ErrMalformedPayload = errors.New("malformed webhook payload")

// Delivery is one parsed, verified webhook: the event, the repository it concerns,
// and the handful of fields the platform routes and filters on. It is deliberately
// SMALL. GitHub's push payload alone is tens of kilobytes of commits, files, and
// author emails; almost none of it is anything the platform decides with, and
// carrying it further would mean forwarding repository content — and sometimes an
// author's email — into every downstream system for no reason. So the parser keeps
// what routing needs and drops the rest, and that pruning is also the platform's
// redaction: what is never extracted is never logged and never published.
type Delivery struct {
	// Event is the GitHub event type, from the X-GitHub-Event header.
	Event string

	// DeliveryID is GitHub's unique id for this delivery, from X-GitHub-Delivery. It
	// is unique per delivery INCLUDING redeliveries of the same event, which is what
	// makes it the right key for downstream idempotency.
	DeliveryID string

	// Action is the sub-type some events carry — a release is "published" or
	// "deleted", a repository is "archived" or "created". Empty for events (like push)
	// that have no action.
	Action string

	// Repository is the full name, "owner/name".
	Repository string

	// DefaultBranch is the repository's default branch, for filters that care.
	DefaultBranch string

	// Branch is the branch this event concerns, where the concept applies — extracted
	// from the push ref, or the created/deleted ref. Empty for events without a branch.
	Branch string

	// Ref is the raw git ref ("refs/heads/main", "refs/tags/v1.0.0"), kept because a
	// tag and a branch are both refs and a consumer may need to tell them apart.
	Ref string

	// RefType is "branch" or "tag", set by create/delete events which say which.
	RefType string

	// Sender is the GitHub login that triggered the event.
	Sender string

	// HeadSHA is the commit at the tip, where the event has one — the thing an agent
	// would check out. From push (after) or workflow_run (head_sha).
	HeadSHA string

	// Private, Fork, Archived, Deleted are the repository/ref facts the filters act
	// on. They are booleans and not an object because the filter asks yes/no questions.
	Private  bool
	Fork     bool
	Archived bool
	Deleted  bool
}

// payload is the subset of GitHub's JSON the platform reads. Every field here is one
// the parser extracts into a [Delivery] field; nothing is decoded that is not used,
// because a field decoded is a field that has to be kept in sync with GitHub's schema
// forever, and the ones that earn that cost are few.
type payload struct {
	Action string `json:"action"`

	Ref     string `json:"ref"`
	RefType string `json:"ref_type"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	Created bool   `json:"created"`

	Repository struct {
		FullName      string `json:"full_name"`
		Private       bool   `json:"private"`
		Fork          bool   `json:"fork"`
		Archived      bool   `json:"archived"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`

	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`

	WorkflowRun struct {
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
	} `json:"workflow_run"`
}

// Headers is the small slice of the HTTP headers the parser needs. It is an interface
// method rather than a map so the handler can pass a case-insensitive lookup — GitHub
// sends X-GitHub-Event, a Lambda Function URL lower-cases it to x-github-event, and a
// parser that cared which was a parser with a latent bug.
type Headers interface {
	// Get returns the value of a header by name, case-insensitively, or "".
	Get(name string) string
}

// Parse turns a verified request into a [Delivery]. It is called AFTER the signature
// has been checked — parsing an unverified body would be doing an attacker's decoding
// for them — and it reads only what the platform routes on.
func Parse(headers Headers, body []byte) (Delivery, error) {
	event := strings.TrimSpace(headers.Get(EventHeader))
	if event == "" {
		return Delivery{}, fmt.Errorf("%w: %s", ErrMissingHeader, EventHeader)
	}
	deliveryID := strings.TrimSpace(headers.Get(DeliveryHeader))
	if deliveryID == "" {
		return Delivery{}, fmt.Errorf("%w: %s", ErrMissingHeader, DeliveryHeader)
	}

	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Delivery{}, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}

	d := Delivery{
		Event:         event,
		DeliveryID:    deliveryID,
		Action:        p.Action,
		Repository:    p.Repository.FullName,
		DefaultBranch: p.Repository.DefaultBranch,
		Ref:           p.Ref,
		RefType:       p.RefType,
		Sender:        p.Sender.Login,
		Private:       p.Repository.Private,
		Fork:          p.Repository.Fork,
		Archived:      p.Repository.Archived,
		Deleted:       p.Deleted,
	}

	// Per-event extraction. Each event spells its branch and its commit differently,
	// and this switch is the only place that knows those spellings — so a new event's
	// quirks land here and nowhere else.
	switch event {
	case EventPush:
		d.Branch = branchFromRef(p.Ref)
		d.HeadSHA = p.After
		// A push that DELETES a branch arrives with after = all-zeroes and deleted =
		// true. Normalise it, so the filter's "ignore deleted branches" rule has a
		// single fact to read rather than a magic SHA to recognise.
		if p.Deleted || isZeroSHA(p.After) {
			d.Deleted = true
			d.HeadSHA = ""
		}
	case EventCreate, EventDelete:
		// create/delete carry the ref bare ("main", "v1.0.0") and say its type.
		d.Branch = refName(p.Ref, p.RefType)
		if event == EventDelete {
			d.Deleted = true
		}
	case EventWorkflowRun:
		d.HeadSHA = p.WorkflowRun.HeadSHA
		d.Branch = p.WorkflowRun.HeadBranch
	}

	// ping is special: it carries no repository on some org-level hooks, and it exists
	// only to confirm the endpoint is reachable. The handler treats it specially (see
	// Handle); here we just make sure it parses.
	if event != EventPing && d.Repository == "" {
		return Delivery{}, fmt.Errorf("%w: no repository in %s payload", ErrMalformedPayload, event)
	}

	return d, nil
}

// CorrelationID is the id that follows this event through the whole platform:
// webhook → EventBridge → n8n → agent → inference. It is the event type and the
// delivery id — "push:a1b2c3…" — and it is STABLE for a given delivery, including a
// redelivery, which is the entire point. The agent derives its idempotency key from
// this (see the agent package), so a webhook GitHub sends twice produces one agent
// run, not two. A random id here would look fine and quietly break that.
func (d Delivery) CorrelationID() string {
	return d.Event + ":" + d.DeliveryID
}

// branchFromRef pulls "main" out of "refs/heads/main". A ref that is not a branch
// (a tag push, say) yields "", which is correct: it has no branch.
func branchFromRef(ref string) string {
	const prefix = "refs/heads/"
	if strings.HasPrefix(ref, prefix) {
		return strings.TrimPrefix(ref, prefix)
	}
	return ""
}

// refName returns the branch name only when the ref is a branch. create/delete send
// the name already bare, so this just gates it on ref_type.
func refName(ref, refType string) string {
	if refType == "branch" {
		return ref
	}
	return ""
}

// isZeroSHA reports whether a SHA is the all-zeroes sentinel GitHub uses for "there
// is no commit here" — the after-SHA of a branch deletion.
func isZeroSHA(sha string) bool {
	return sha != "" && strings.Trim(sha, "0") == ""
}
