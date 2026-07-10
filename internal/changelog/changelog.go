// Package changelog renders release notes as Keep a Changelog markdown and
// maintains CHANGELOG.md.
//
// Rendering lives here rather than on release.Notes so that the same notes can
// become a changelog entry and a GitHub release body without either format
// leaking into the domain. The two differ on purpose: the changelog entry leads
// with a version heading and omits the summary, because a changelog is read as a
// list; the release body leads with the summary and ends with a compare link,
// because it is read on its own.
//
// Insert is idempotent. Running a release twice must not produce two entries for
// the same version, and a release pipeline is retried more often than anyone
// plans for.
package changelog

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// Filename is the changelog's name at the repository root.
const Filename = "CHANGELOG.md"

// Header is the preamble every generated changelog carries.
const Header = `# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
`

// versionHeading matches "## [0.2.0] - 2026-07-10", capturing the version.
var versionHeading = regexp.MustCompile(`(?m)^## \[([^\]]+)\]`)

// Entry renders one release as a changelog section, without the compare link.
//
//	## [0.2.0] - 2026-07-10
//
//	### Added
//	- Add the roadmap registry (aaaaaaa)
func Entry(notes release.Notes) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## [%s] - %s\n", notes.Version, notes.Date.Format(release.DateFormat))

	sections := notes.PopulatedSections()
	if len(sections) == 0 {
		// An empty release is not an error — a re-tag, for instance — but the
		// changelog must say something rather than trail off into a heading.
		b.WriteString("\nNo user-facing changes.\n")
		return b.String()
	}

	for _, section := range sections {
		fmt.Fprintf(&b, "\n### %s\n\n", section.Category)
		for _, entry := range section.Entries {
			fmt.Fprintf(&b, "- %s\n", entry)
		}
	}
	return b.String()
}

// ReleaseBody renders notes as a GitHub release body: a summary paragraph, the
// sections, and a compare link back to the previous release.
func ReleaseBody(notes release.Notes, repository string) string {
	var b strings.Builder

	if notes.Summary != "" {
		b.WriteString(notes.Summary)
		b.WriteString("\n")
	}

	for _, section := range notes.PopulatedSections() {
		fmt.Fprintf(&b, "\n### %s\n\n", section.Category)
		for _, entry := range section.Entries {
			fmt.Fprintf(&b, "- %s\n", entry)
		}
	}

	if notes.IsEmpty() {
		b.WriteString("\nNo user-facing changes.\n")
	}

	if link := compareLink(notes, repository); link != "" {
		fmt.Fprintf(&b, "\n%s\n", link)
	}
	return b.String()
}

// compareLink points at the diff this release represents. The first release has
// no predecessor and therefore no compare link.
func compareLink(notes release.Notes, repository string) string {
	if repository == "" || notes.Comparison == nil || notes.Comparison.IsInitial() {
		return ""
	}
	url := fmt.Sprintf("https://github.com/%s/compare/%s", repository, notes.Comparison.Range())
	return fmt.Sprintf("**Full changelog:** [%s](%s)", notes.Comparison.Range(), url)
}

// Render builds a whole changelog from newest to oldest.
func Render(entries []release.Notes) string {
	var b strings.Builder
	b.WriteString(Header)
	for _, notes := range entries {
		b.WriteString("\n")
		b.WriteString(Entry(notes))
	}
	return b.String()
}

// Contains reports whether the changelog already documents this version.
func Contains(changelog string, v version.Version) bool {
	for _, match := range versionHeading.FindAllStringSubmatch(changelog, -1) {
		if existing, err := version.Parse(match[1]); err == nil && existing.Equal(v) {
			return true
		}
	}
	return false
}

// Insert places a new entry above the newest existing one, returning the updated
// changelog.
//
// It is idempotent: a version already present is left untouched, so re-running a
// release does not duplicate its entry.
func Insert(changelog string, notes release.Notes) string {
	if strings.TrimSpace(changelog) == "" {
		return Render([]release.Notes{notes})
	}
	if Contains(changelog, notes.Version) {
		return changelog
	}

	entry := Entry(notes)

	// Insert immediately above the first version heading, which is the newest
	// release. Everything before it is the preamble.
	if location := versionHeading.FindStringIndex(changelog); location != nil {
		return changelog[:location[0]] + entry + "\n" + changelog[location[0]:]
	}

	// No releases yet: append below the preamble.
	return strings.TrimRight(changelog, "\n") + "\n\n" + entry
}

// File maintains CHANGELOG.md on disk.
type File struct {
	Path string
}

// NewFile locates CHANGELOG.md under root.
func NewFile(root string) *File {
	return &File{Path: filepath.Join(root, Filename)}
}

// Read returns the current changelog, or the bare header when none exists.
func (f *File) Read() (string, error) {
	raw, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return Header, nil
	}
	if err != nil {
		return "", fmt.Errorf("could not read %s: %w", f.Path, err)
	}
	return string(raw), nil
}

// Insert adds an entry for notes and writes the file back.
//
// It reports whether the file changed, so a dry run and a no-op re-release can
// both be reported honestly.
func (f *File) Insert(notes release.Notes) (changed bool, err error) {
	current, err := f.Read()
	if err != nil {
		return false, err
	}
	updated := Insert(current, notes)
	if updated == current {
		return false, nil
	}
	if err := os.WriteFile(f.Path, []byte(updated), 0o644); err != nil {
		return false, fmt.Errorf("could not write %s: %w", f.Path, err)
	}
	return true, nil
}
