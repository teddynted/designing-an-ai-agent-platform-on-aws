package changelog

import (
	"strings"
	"testing"
)

// statsFor builds statistics the way a real release does, from commits.
func statsFor(subjects ...string) Stats {
	commits := make([]Commit, 0, len(subjects))
	for i, s := range subjects {
		commits = append(commits, Commit{SHA: string(rune('a' + i)), Subject: s})
	}
	return Statistics("minor", ParseAll(commits), DefaultCategories())
}

func TestSummary(t *testing.T) {
	tests := []struct {
		name     string
		subjects []string
		want     string
	}{
		{
			name:     "one of each",
			subjects: []string{"feat: a", "fix: b", "docs: c"},
			want:     "This release introduces 1 new feature, fixes 1 bug, and documents 1 change.",
		},
		{
			name:     "plurals",
			subjects: []string{"feat: a", "feat: b", "fix: c", "fix: d"},
			want:     "This release introduces 2 new features and fixes 2 bugs.",
		},
		{
			name:     "a single clause needs no conjunction",
			subjects: []string{"feat: a"},
			want:     "This release introduces 1 new feature.",
		},
		{
			name:     "housekeeping only",
			subjects: []string{"chore: a", "ci: b"},
			want:     "This release contains 2 changes.",
		},
		{
			name:     "one housekeeping commit",
			subjects: []string{"chore: a"},
			want:     "This release contains 1 change.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Summary(statsFor(tt.subjects...)); got != tt.want {
				t.Errorf("Summary() = %q\nwant             %q", got, tt.want)
			}
		})
	}
}

func TestSummaryWarnsAboutBreakingChanges(t *testing.T) {
	stats := statsFor("feat!: a", "fix: b")
	got := Summary(stats)

	if !strings.Contains(got, "1 breaking change") {
		t.Errorf("Summary() = %q, want a breaking-change warning", got)
	}
	if !strings.HasSuffix(got, "review the notes before upgrading.") {
		t.Errorf("Summary() = %q, want the upgrade warning last", got)
	}
}

func TestSummaryEmptyRelease(t *testing.T) {
	if got := Summary(Stats{}); got != "" {
		t.Errorf("Summary() of an empty release = %q, want empty so the caller can omit it", got)
	}
}

// The summary is counted, never paraphrased: no commit text may reach it. That
// is the whole reason it can be trusted.
func TestSummaryNeverQuotesCommitText(t *testing.T) {
	got := Summary(statsFor("feat: add a completely distinctive phrase", "fix: another distinctive phrase"))
	for _, phrase := range []string{"distinctive", "add", "another"} {
		if strings.Contains(got, phrase) {
			t.Errorf("Summary() leaked commit text %q: %s", phrase, got)
		}
	}
}

func TestQuantity(t *testing.T) {
	if got := quantity(1, "bug", "bugs"); got != "1 bug" {
		t.Errorf("quantity(1) = %q", got)
	}
	if got := quantity(2, "bug", "bugs"); got != "2 bugs" {
		t.Errorf("quantity(2) = %q", got)
	}
}

func TestList(t *testing.T) {
	for _, tt := range []struct {
		in   []string
		want string
	}{
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a and b"},
		{[]string{"a", "b", "c"}, "a, b, and c"},
	} {
		if got := list(tt.in); got != tt.want {
			t.Errorf("list(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
