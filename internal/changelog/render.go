package changelog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// DefaultNotesTemplate renders the body of a GitHub Release.
//
// It is the built-in layout, and the reference for anyone writing a custom one:
// every field it uses is documented on Data. Override it with Options.Template.
const DefaultNotesTemplate = `## What's Changed

{{if .Groups}}{{range .Groups}}### {{.Heading}}

{{range .Items}}- {{.Text}}{{with .Link}} {{.}}{{end}}
{{with .BreakingNote}}  {{.}}
{{end}}{{end}}
{{end}}{{else}}_No user-facing changes._

{{end}}{{if .IsFirstRelease}}Initial release.
{{else if .CompareURL}}Compare changes:
{{.CompareURL}}
{{end}}`

// DefaultEntryTemplate renders one version's section of CHANGELOG.md, in the
// Keep a Changelog layout. The compare link lives in the heading, so the entry
// has no footer.
const DefaultEntryTemplate = `## [{{.Version}}]{{with .HistoryURL}}({{.}}){{end}} - {{.Date}}

{{if .Groups}}{{range .Groups}}### {{.Heading}}

{{range .Items}}- {{.Text}}{{with .Link}} {{.}}{{end}}
{{with .BreakingNote}}  {{.}}
{{end}}{{end}}
{{end}}{{else}}_No user-facing changes._
{{end}}`

// Data is what a release-notes template is executed against.
//
// The whole surface is documented because it is a public contract: a project
// that writes a custom template depends on these names.
type Data struct {
	// Tag is the Git tag being released, such as "v1.3.0".
	Tag string
	// Version is the tag without its prefix, such as "1.3.0".
	Version string
	// PreviousTag is the tag this release is compared against. It is empty for
	// a first release.
	PreviousTag string
	// Date is the release date in ISO-8601 form, "2006-01-02".
	Date string
	// Bump is "major", "minor", or "patch", when known.
	Bump string

	// IsFirstRelease is true when there is no previous tag to compare against.
	IsFirstRelease bool
	// CompareURL diffs the previous tag against this one. Empty for a first
	// release, or when the repository has no known remote.
	CompareURL string
	// HistoryURL is CompareURL, falling back to the full commit history for a
	// first release.
	HistoryURL string
	// Repository is the owner and name behind the links.
	Repository Repository

	// Groups holds the non-empty categories, Breaking Changes first.
	Groups []Group
	// Stats summarises the release.
	Stats Stats
}

// Options controls rendering. The zero value renders the default layout with
// the default categories.
type Options struct {
	// Categories replaces DefaultCategories when non-nil.
	Categories []Category
	// Template replaces the built-in template when non-nil.
	Template *template.Template
}

func (o Options) categories() []Category {
	if o.Categories != nil {
		return o.Categories
	}
	return DefaultCategories()
}

// NewData builds the template input for a release. It is exported so that a
// caller can inspect the statistics without rendering anything.
func NewData(rel Release, categories []Category) Data {
	entries := ParseAll(rel.Commits)

	return Data{
		Tag:            rel.Tag,
		Version:        rel.Version.String(),
		PreviousTag:    rel.PreviousTag,
		Date:           rel.Date.Format(time.DateOnly),
		Bump:           rel.Bump,
		IsFirstRelease: rel.IsFirstRelease(),
		CompareURL:     rel.CompareURL(),
		HistoryURL:     rel.HistoryURL(),
		Repository:     rel.Repo,
		Groups:         Groups(entries, categories, rel.Repo),
		Stats:          Statistics(rel.Bump, entries, categories),
	}
}

// RenderNotes renders the body of a GitHub Release.
func RenderNotes(rel Release, opts Options) (string, error) {
	return render(rel, opts, "notes", DefaultNotesTemplate)
}

// RenderEntry renders one version's section of CHANGELOG.md.
func RenderEntry(rel Release, opts Options) (string, error) {
	return render(rel, opts, "entry", DefaultEntryTemplate)
}

func render(rel Release, opts Options, name, fallback string) (string, error) {
	tmpl := opts.Template
	if tmpl == nil {
		var err error
		if tmpl, err = template.New(name).Parse(fallback); err != nil {
			// A malformed built-in template is a programming error.
			return "", fmt.Errorf("parsing the built-in %s template: %w", name, err)
		}
	}

	var out strings.Builder
	if err := tmpl.Execute(&out, NewData(rel, opts.categories())); err != nil {
		return "", fmt.Errorf("rendering the %s template: %w", name, err)
	}
	// Normalise the trailing whitespace a template inevitably leaves behind, so
	// callers can append to the output without guessing at the spacing.
	return strings.TrimRight(out.String(), "\n") + "\n", nil
}

// ParseTemplate loads a release-notes template from disk. The template is
// executed against Data.
func ParseTemplate(path string) (*template.Template, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the template %s: %w", path, err)
	}
	tmpl, err := template.New(filepath.Base(path)).Parse(string(source))
	if err != nil {
		return nil, fmt.Errorf("parsing the template %s: %w", path, err)
	}
	return tmpl, nil
}
