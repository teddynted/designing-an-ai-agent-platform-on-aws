// Package roadmap reads and writes RELEASES.yaml, the registry of planned,
// in-progress, and shipped releases.
//
// The registry exists because git tags answer "what shipped" but not "what is
// next", and a roadmap that lives only in prose drifts from the versions that
// implement it. Keeping both in one hand-editable file means the roadmap and the
// release tooling cannot disagree about which version delivers which milestone.
//
// YAML rather than JSON because a human edits this file and wants comments.
// goccy/go-yaml rather than gopkg.in/yaml.v3 because the latter is effectively
// unmaintained. This is the module's only third-party dependency; the reasoning
// is recorded in ADR-0013.
//
// Because comments are the reason for choosing YAML, they must survive a
// release. Marshalling a struct back to YAML reconstructs the document from the
// data and silently discards everything that is not data, so a Registry carries
// the comments it was parsed with and re-emits them on Save. Without this, the
// first release deletes the header explaining the file, and every subsequent one
// deletes whatever a human wrote since.
//
// Comments are keyed by document path, so a comment on `$.releases[1]` follows
// the second entry rather than the release it was written about. Appending a
// release is safe — entries sort ascending and a newer version lands last —
// but inserting one before a commented entry would move that comment.
package roadmap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// Filename is the registry's name at the repository root.
const Filename = "RELEASES.yaml"

// entry is the on-disk shape of one release. It is separate from
// release.Release so that the file format can change without disturbing the
// domain, and so that YAML sees plain strings rather than value objects.
type entry struct {
	Version    string   `yaml:"version"`
	Title      string   `yaml:"title"`
	Status     string   `yaml:"status"`
	Date       string   `yaml:"date,omitempty"`
	Milestone  int      `yaml:"milestone,omitempty"`
	Summary    string   `yaml:"summary,omitempty"`
	Highlights []string `yaml:"highlights,omitempty"`
}

type document struct {
	Releases []entry `yaml:"releases"`
}

// Registry is the set of releases this project has planned and shipped, ordered
// ascending by version.
type Registry struct {
	Releases []release.Release

	// comments are the ones Parse found, re-emitted by Marshal. A Registry
	// built in code rather than parsed simply has none.
	comments yaml.CommentMap
}

// Parse reads a registry from YAML, validating each entry and retaining the
// document's comments.
func Parse(data []byte) (*Registry, error) {
	var doc document
	comments := yaml.CommentMap{}
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.CommentToMap(comments)); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML: %w", Filename, err)
	}

	registry := &Registry{comments: comments}
	seen := map[string]bool{}

	for i, e := range doc.Releases {
		v, err := version.Parse(e.Version)
		if err != nil {
			return nil, fmt.Errorf("%s: release %d: %w", Filename, i+1, err)
		}
		if seen[v.String()] {
			return nil, fmt.Errorf("%s: %s appears twice", Filename, v)
		}
		seen[v.String()] = true

		status, err := release.ParseStatus(e.Status)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", Filename, v.Tag(), err)
		}

		var when time.Time
		if e.Date != "" {
			when, err = time.Parse(release.DateFormat, e.Date)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: date %q is not YYYY-MM-DD", Filename, v.Tag(), e.Date)
			}
		}
		if e.Title == "" {
			return nil, fmt.Errorf("%s: %s has no title", Filename, v.Tag())
		}

		r := release.Release{
			Version:    v,
			Title:      e.Title,
			Status:     status,
			Date:       when,
			Milestone:  e.Milestone,
			Summary:    e.Summary,
			Highlights: e.Highlights,
		}
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", Filename, err)
		}
		registry.Releases = append(registry.Releases, r)
	}

	registry.sort()
	return registry, nil
}

// Marshal renders the registry as YAML.
func (r *Registry) Marshal() ([]byte, error) {
	doc := document{Releases: make([]entry, 0, len(r.Releases))}
	for _, rel := range r.Releases {
		e := entry{
			Version:    rel.Version.String(),
			Title:      rel.Title,
			Status:     rel.Status.String(),
			Milestone:  rel.Milestone,
			Summary:    rel.Summary,
			Highlights: rel.Highlights,
		}
		if !rel.Date.IsZero() {
			e.Date = rel.Date.Format(release.DateFormat)
		}
		doc.Releases = append(doc.Releases, e)
	}
	// IndentSequence so a sequence item is indented under its key, which is how
	// a person writes it and therefore how the file arrived.
	options := []yaml.EncodeOption{yaml.Indent(2), yaml.IndentSequence(true)}
	if len(r.comments) > 0 {
		options = append(options, yaml.WithComment(r.comments))
	}

	data, err := yaml.MarshalWithOptions(doc, options...)
	if err != nil {
		return nil, fmt.Errorf("could not render %s: %w", Filename, err)
	}
	return data, nil
}

func (r *Registry) sort() {
	slices.SortFunc(r.Releases, func(a, b release.Release) int { return a.Version.Compare(b.Version) })
}

// Find returns the release for a version, or nil.
func (r *Registry) Find(v version.Version) *release.Release {
	for i := range r.Releases {
		if r.Releases[i].Version.Equal(v) {
			return &r.Releases[i]
		}
	}
	return nil
}

// Latest is the highest shipped release, or nil when none has shipped.
func (r *Registry) Latest() *release.Release {
	for i := len(r.Releases) - 1; i >= 0; i-- {
		if r.Releases[i].IsReleased() {
			return &r.Releases[i]
		}
	}
	return nil
}

// Next is the lowest release that has not shipped, or nil when none is planned.
func (r *Registry) Next() *release.Release {
	for i := range r.Releases {
		if !r.Releases[i].IsReleased() {
			return &r.Releases[i]
		}
	}
	return nil
}

// Planned returns every release that has not yet shipped, ascending.
func (r *Registry) Planned() []release.Release {
	var out []release.Release
	for _, rel := range r.Releases {
		if !rel.IsReleased() {
			out = append(out, rel)
		}
	}
	return out
}

// Upsert adds a release, or replaces the entry for a version already present.
func (r *Registry) Upsert(rel release.Release) error {
	if err := rel.Validate(); err != nil {
		return err
	}
	if existing := r.Find(rel.Version); existing != nil {
		*existing = rel
		return nil
	}
	r.Releases = append(r.Releases, rel)
	r.sort()
	return nil
}

// MarkReleased transitions a version to released on the given date.
//
// A version absent from the registry is added rather than rejected: a release
// cut without a roadmap entry is a real thing that happens, and refusing it
// would fail the pipeline over bookkeeping.
func (r *Registry) MarkReleased(v version.Version, title string, when time.Time) error {
	if existing := r.Find(v); existing != nil {
		*existing = existing.ReleasedOn(when)
		return nil
	}
	return r.Upsert(release.Release{
		Version: v,
		Title:   title,
		Status:  release.Released,
		Date:    when,
	})
}

// File maintains RELEASES.yaml on disk.
type File struct {
	Path string
}

// NewFile locates RELEASES.yaml under root.
func NewFile(root string) *File {
	return &File{Path: filepath.Join(root, Filename)}
}

// Exists reports whether the registry is present.
func (f *File) Exists() bool {
	_, err := os.Stat(f.Path)
	return err == nil
}

// Load reads the registry. A missing file yields an empty registry rather than
// an error: the roadmap is optional, and a project without one still releases.
func (f *File) Load() (*Registry, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return &Registry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", f.Path, err)
	}
	return Parse(data)
}

// Save writes the registry back.
func (f *File) Save(registry *Registry) error {
	data, err := registry.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(f.Path, data, 0o644); err != nil {
		return fmt.Errorf("could not write %s: %w", f.Path, err)
	}
	return nil
}
