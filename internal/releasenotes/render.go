package releasenotes

import (
	"fmt"
	"strings"
)

// Rendering the body a reader sees on the releases page.
//
// Three rules shape it.
//
// Empty sections are omitted. A release with no bug fixes says nothing about bug
// fixes; a "Bug Fixes" heading followed by nothing is worse than no heading.
//
// Commit hashes are not the content. A reader deciding whether to upgrade wants
// titles, not forty hexadecimal characters. The raw commit list is still there,
// folded into a <details> block, because sometimes it is exactly what you want.
//
// Nothing is paraphrased. Every title below was typed by a person.

// Render produces the GitHub-flavoured markdown for a release body.
func Render(notes Notes) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s\n", notes.Version.Tag())

	renderHighlights(&b, notes)

	for _, section := range notes.PopulatedSections() {
		fmt.Fprintf(&b, "\n## %s\n\n", section.Heading())
		for _, entry := range notes.Entries(section) {
			b.WriteString(bullet(entry, section))
		}
	}

	if notes.IsEmpty() {
		b.WriteString("\nThis release carries no user-facing changes.\n")
	}

	renderStatistics(&b, notes)
	renderContributors(&b, notes)
	renderCommits(&b, notes)
	renderChangelogLink(&b, notes)

	return b.String()
}

func renderHighlights(b *strings.Builder, notes Notes) {
	b.WriteString("\n## 🚀 Highlights\n\n")
	b.WriteString(notes.Summary)
	b.WriteString("\n")

	if len(notes.Highlights) > 0 {
		b.WriteString("\n")
		for _, highlight := range notes.Highlights {
			fmt.Fprintf(b, "- %s\n", highlight)
		}
	}
}

// bullet renders one entry. The breaking marker is redundant inside the Breaking
// Changes section, so it is omitted there.
func bullet(entry Entry, section Section) string {
	title := entry.Title
	if entry.Breaking && section != Breaking {
		title = "**Breaking:** " + title
	}
	if entry.Number > 0 {
		return fmt.Sprintf("- %s (#%d)\n", title, entry.Number)
	}
	return fmt.Sprintf("- %s\n", title)
}

func renderStatistics(b *strings.Builder, notes Notes) {
	s := notes.Statistics
	b.WriteString("\n## 📊 Release Statistics\n\n")
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(b, "| **Commits** | %d |\n", s.Commits)
	fmt.Fprintf(b, "| **Contributors** | %d |\n", s.Contributors)
	fmt.Fprintf(b, "| **Files changed** | %d |\n", s.FilesChanged)
	fmt.Fprintf(b, "| **Lines added** | %d |\n", s.Insertions)
	fmt.Fprintf(b, "| **Lines removed** | %d |\n", s.Deletions)
}

func renderContributors(b *strings.Builder, notes Notes) {
	if len(notes.Contributors) == 0 {
		return
	}
	b.WriteString("\n## 🙌 Contributors\n\n")

	if len(notes.Contributors) == 1 {
		fmt.Fprintf(b, "This release was written by %s.\n", notes.Contributors[0].Mention())
		return
	}

	b.WriteString("Thanks to:\n\n")
	for _, contributor := range notes.Contributors {
		fmt.Fprintf(b, "- %s\n", contributor.Mention())
	}
}

// renderCommits folds the raw commit list away. Hashes are for the reader who
// went looking; they are not what a release is about.
func renderCommits(b *strings.Builder, notes Notes) {
	if len(notes.Commits) == 0 {
		return
	}
	b.WriteString("\n<details>\n<summary>All commits in this release</summary>\n\n")
	for _, commit := range notes.Commits {
		fmt.Fprintf(b, "- `%s` %s\n", commit.ShortSHA(), commit.Subject)
	}
	b.WriteString("\n</details>\n")
}

func renderChangelogLink(b *strings.Builder, notes Notes) {
	if notes.Repository == "" {
		return
	}
	b.WriteString("\n## 🔗 Full Changelog\n\n")

	if notes.IsInitial() {
		// There is no predecessor to compare against; the tag is the whole
		// history.
		fmt.Fprintf(b, "https://github.com/%s/commits/%s\n", notes.Repository, notes.Version.Tag())
		return
	}
	fmt.Fprintf(b, "https://github.com/%s/compare/%s...%s\n",
		notes.Repository, notes.PreviousTag, notes.Version.Tag())
}

// plural formats a count with the right noun.
func plural(n int, singular, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, many)
}
