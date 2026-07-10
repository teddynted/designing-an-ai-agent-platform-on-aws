package changelog

// Count is the number of commits filed under one category.
type Count struct {
	// Key matches the Category the count belongs to.
	Key string
	// Label is the short name for display, such as "Fixes".
	Label string
	// N is the number of commits.
	N int
}

// Stats summarises what a release contains. It is derived from the categorised
// commits, so it can never disagree with the rendered notes.
type Stats struct {
	// Bump is the increment being applied: "major", "minor", or "patch". It is
	// empty when the statistics describe an existing tag rather than a plan.
	Bump string
	// Commits is the total number of commits in the release.
	Commits int
	// Breaking is how many of them are breaking changes.
	Breaking int
	// Counts holds one entry per category that has at least one commit, in
	// category order. Empty categories are omitted rather than shown as zero.
	Counts []Count
}

// Statistics counts entries per category, including categories that are hidden
// from the rendered notes: a chore is still work that went into the release.
func Statistics(bump string, entries []Entry, categories []Category) Stats {
	stats := Stats{Bump: bump, Commits: len(entries)}

	for _, e := range entries {
		if e.Breaking {
			stats.Breaking++
		}
	}

	claimed := claimedTypes(categories)
	for _, category := range categories {
		n := 0
		for _, e := range entries {
			if belongsTo(e, category, claimed) {
				n++
			}
		}
		if n > 0 {
			stats.Counts = append(stats.Counts, Count{Key: category.Key, Label: category.Label, N: n})
		}
	}
	return stats
}
