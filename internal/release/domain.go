// Package release holds the release domain and the services that assemble one.
//
// A release is the pairing of a version with human intent: a title, a status,
// and optionally the milestone it delivers. It knows nothing about GitHub —
// there is no HTMLURL, no NodeID, no UploadURL here. The GitHub release object
// is a projection of this, built in the adapter layer.
//
// On milestones and versions: they are two sequences, not one. A tooling release
// delivers no architectural milestone, so Milestone is optional. Conflating them
// would force every milestone reference in docs/ to be renumbered whenever a
// tooling release lands.
package release

import (
	"fmt"
	"strings"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// DateFormat is the calendar-date layout used across changelogs and the roadmap.
const DateFormat = "2006-01-02"

// Status is where a release sits in its lifecycle.
type Status string

const (
	Planned    Status = "planned"
	InProgress Status = "in-progress"
	Released   Status = "released"
)

func (s Status) String() string { return string(s) }

// ParseStatus reads a status from the roadmap file.
func ParseStatus(text string) (Status, error) {
	switch s := Status(strings.ToLower(strings.TrimSpace(text))); s {
	case Planned, InProgress, Released:
		return s, nil
	default:
		return "", fmt.Errorf("unknown status %q; expected one of: %s, %s, %s", text, Planned, InProgress, Released)
	}
}

// Release is a planned, in-progress, or completed release.
type Release struct {
	Version    version.Version
	Title      string
	Status     Status
	Date       time.Time // zero unless released
	Milestone  int       // 0 means "delivers no milestone"
	Summary    string
	Highlights []string
}

// Validate rejects the one inconsistent state: a released version with no date,
// which cannot be ordered on the roadmap.
func (r Release) Validate() error {
	if r.Status == Released && r.Date.IsZero() {
		return fmt.Errorf("%s is marked released but has no date", r.Tag())
	}
	return nil
}

// Tag is the git tag naming this release.
func (r Release) Tag() string { return r.Version.Tag() }

// IsReleased reports whether this release has shipped.
func (r Release) IsReleased() bool { return r.Status == Released }

// HasMilestone reports whether this release delivers an architectural milestone.
func (r Release) HasMilestone() bool { return r.Milestone > 0 }

// ReleasedOn returns a copy marked released. Releases are immutable; this is the
// transition.
func (r Release) ReleasedOn(when time.Time) Release {
	r.Status = Released
	r.Date = when
	return r
}

func (r Release) String() string { return r.Tag() + " — " + r.Title }

// Category is Keep a Changelog's six section types.
//
// The order of Categories below is the order that specification prescribes, and
// it is relied upon when rendering so that every release document in this
// repository lists its sections identically.
type Category string

const (
	Added      Category = "Added"
	Changed    Category = "Changed"
	Deprecated Category = "Deprecated"
	Removed    Category = "Removed"
	Fixed      Category = "Fixed"
	Security   Category = "Security"
)

// Categories is the canonical rendering order.
var Categories = []Category{Added, Changed, Deprecated, Removed, Fixed, Security}

func (c Category) String() string { return string(c) }

// Notes is everything needed to announce one release, as structured data rather
// than as a string.
//
// Rendering lives in the changelog package. Keeping them apart means the
// changelog and the GitHub release body can render the same notes differently —
// the changelog wants a `##` heading and no summary paragraph, the release body
// wants the summary and a compare link — without either format leaking into the
// domain.
type Notes struct {
	Version          version.Version
	Date             time.Time
	Summary          string
	Highlights       []string
	Sections         map[Category][]string
	KnownLimitations []string
	Comparison       *git.Comparison
}

// IsEmpty reports whether no category carries an entry.
//
// An empty release is not necessarily an error — a re-tag, for instance — but it
// is worth the caller noticing before publishing it.
func (n Notes) IsEmpty() bool {
	for _, entries := range n.Sections {
		if len(entries) > 0 {
			return false
		}
	}
	return true
}

// Entries returns the bullets filed under one category.
func (n Notes) Entries(category Category) []string { return n.Sections[category] }

// Section pairs a category with its entries.
type Section struct {
	Category Category
	Entries  []string
}

// PopulatedSections returns the non-empty sections, in Keep a Changelog order.
func (n Notes) PopulatedSections() []Section {
	var sections []Section
	for _, category := range Categories {
		if entries := n.Sections[category]; len(entries) > 0 {
			sections = append(sections, Section{Category: category, Entries: entries})
		}
	}
	return sections
}

// Clock is injected so that generated notes and changelog entries are testable.
//
// A service that calls time.Now directly cannot be tested for the date it
// writes, and a changelog is a document about dates.
type Clock interface {
	Today() time.Time
}

// SystemClock is the real clock, truncated to a calendar date in UTC.
type SystemClock struct{}

func (SystemClock) Today() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// FixedClock is stuck at one day. For tests, and for reproducible builds.
type FixedClock struct{ When time.Time }

func (f FixedClock) Today() time.Time { return f.When }

// MustDate parses a calendar date in DateFormat, panicking on failure. For tests
// and for constants.
func MustDate(text string) time.Time {
	when, err := time.Parse(DateFormat, text)
	if err != nil {
		panic(err)
	}
	return when
}
