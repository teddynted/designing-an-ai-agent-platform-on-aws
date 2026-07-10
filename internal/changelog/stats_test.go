package changelog

import "testing"

func countFor(stats Stats, key string) int {
	for _, c := range stats.Counts {
		if c.Key == key {
			return c.N
		}
	}
	return 0
}

func TestStatistics(t *testing.T) {
	entries := ParseAll([]Commit{
		{SHA: "a", Subject: "feat: one"},
		{SHA: "b", Subject: "feat: two"},
		{SHA: "c", Subject: "fix: three"},
		{SHA: "d", Subject: "docs: four"},
		{SHA: "e", Subject: "refactor: five"},
		{SHA: "f", Subject: "not conventional"},
		{SHA: "g", Subject: "feat!: breaking", Body: "BREAKING CHANGE: yes"},
	})
	stats := Statistics("minor", entries, DefaultCategories())

	if stats.Bump != "minor" {
		t.Errorf("Bump = %q", stats.Bump)
	}
	if stats.Commits != 7 {
		t.Errorf("Commits = %d, want 7", stats.Commits)
	}
	if stats.Breaking != 1 {
		t.Errorf("Breaking = %d, want 1", stats.Breaking)
	}

	for key, want := range map[string]int{
		"feat": 3, "fix": 1, "docs": 1, "refactor": 1, OtherKey: 1,
	} {
		if got := countFor(stats, key); got != want {
			t.Errorf("count[%s] = %d, want %d", key, got, want)
		}
	}
}

// The statistics must never disagree with the rendered notes, so they are
// derived from the same classification.
func TestStatisticsMatchesGroups(t *testing.T) {
	entries := ParseAll([]Commit{
		{SHA: "a", Subject: "feat: one"},
		{SHA: "b", Subject: "chore: two"},
		{SHA: "c", Subject: "mystery"},
	})
	categories := DefaultCategories()

	stats := Statistics("patch", entries, categories)
	for _, g := range Groups(entries, categories, testRepo) {
		if g.Key == BreakingKey {
			continue
		}
		if got := countFor(stats, g.Key); got != len(g.Items) {
			t.Errorf("category %q: statistics say %d, notes show %d", g.Key, got, len(g.Items))
		}
	}
}

// Empty categories are omitted, not reported as zero.
func TestStatisticsOmitsEmptyCategories(t *testing.T) {
	stats := Statistics("patch", ParseAll([]Commit{{SHA: "a", Subject: "fix: one"}}), DefaultCategories())
	if len(stats.Counts) != 1 || stats.Counts[0].Key != "fix" {
		t.Errorf("Counts = %+v, want only fix", stats.Counts)
	}
	if stats.Counts[0].Label != "Fixes" {
		t.Errorf("Label = %q, want the short statistics label", stats.Counts[0].Label)
	}
}

func TestStatisticsCountsInCategoryOrder(t *testing.T) {
	entries := ParseAll([]Commit{
		{SHA: "a", Subject: "fix: b"},
		{SHA: "b", Subject: "feat: a"},
	})
	stats := Statistics("minor", entries, DefaultCategories())
	if len(stats.Counts) != 2 || stats.Counts[0].Key != "feat" || stats.Counts[1].Key != "fix" {
		t.Errorf("Counts = %+v, want feat before fix", stats.Counts)
	}
}

func TestStatisticsEmptyRelease(t *testing.T) {
	stats := Statistics("patch", nil, DefaultCategories())
	if stats.Commits != 0 || stats.Breaking != 0 || len(stats.Counts) != 0 {
		t.Errorf("Statistics(nil) = %+v", stats)
	}
}
