package changelog

import (
	"slices"
	"testing"
)

var testRepo = Repository{Host: "github.com", Owner: "teddynted", Name: "repo"}

// Every Conventional Commit type named by the specification must land in a
// category, otherwise a commit would silently vanish from the notes.
func TestDefaultCategoriesCoverEveryType(t *testing.T) {
	claimed := claimedTypes(DefaultCategories())
	for _, commitType := range []string{
		"feat", "fix", "docs", "refactor", "perf",
		"test", "build", "ci", "chore", "style", "revert",
	} {
		if !claimed[commitType] {
			t.Errorf("no category claims %q", commitType)
		}
	}
	// The catch-all exists, and it is the "other" category.
	if !claimed[""] {
		t.Error("no category acts as the catch-all")
	}
}

func TestDefaultCategoriesHaveUniqueKeys(t *testing.T) {
	var keys []string
	for _, c := range DefaultCategories() {
		if slices.Contains(keys, c.Key) {
			t.Errorf("duplicate category key %q", c.Key)
		}
		if c.Title == "" || c.Label == "" {
			t.Errorf("category %q needs both a Title and a Label", c.Key)
		}
		keys = append(keys, c.Key)
	}
}

func TestHeading(t *testing.T) {
	if got := (Category{Title: "Features", Icon: "🚀"}).Heading(); got != "🚀 Features" {
		t.Errorf("Heading() = %q", got)
	}
	if got := (Category{Title: "Other Changes"}).Heading(); got != "Other Changes" {
		t.Errorf("Heading() without an icon = %q", got)
	}
}

func TestGroupsClassifiesAndOrders(t *testing.T) {
	entries := ParseAll([]Commit{
		{SHA: "aaaaaaabbbb", Subject: "fix: correct a thing"},
		{SHA: "cccccccdddd", Subject: "feat: add a thing"},
		{SHA: "eeeeeeefff0", Subject: "docs: explain a thing"},
		{SHA: "11111112222", Subject: "wip on something"},
		{SHA: "33333334444", Subject: "unknowntype: still something"},
	})
	groups := Groups(entries, DefaultCategories(), testRepo)

	// Category order, not commit order, decides the sections.
	var titles []string
	for _, g := range groups {
		titles = append(titles, g.Title)
	}
	want := []string{"Features", "Bug Fixes", "Documentation", "Other Changes"}
	if !slices.Equal(titles, want) {
		t.Fatalf("group titles = %v, want %v", titles, want)
	}

	// Both the non-conventional subject and the unrecognised type fall through
	// to the catch-all.
	other := groups[len(groups)-1]
	if len(other.Items) != 2 {
		t.Errorf("Other Changes has %d items, want 2: %+v", len(other.Items), other.Items)
	}
}

func TestGroupsSkipsEmptyCategories(t *testing.T) {
	groups := Groups(ParseAll([]Commit{{SHA: "a", Subject: "feat: only this"}}), DefaultCategories(), testRepo)
	if len(groups) != 1 || groups[0].Key != "feat" {
		t.Errorf("groups = %+v, want only the Features group", groups)
	}
}

func TestGroupsRespectsHidden(t *testing.T) {
	categories := DefaultCategories()
	for i := range categories {
		if categories[i].Key == "chore" {
			categories[i].Hidden = true
		}
	}
	entries := ParseAll([]Commit{{SHA: "a", Subject: "chore: tidy"}})

	if groups := Groups(entries, categories, testRepo); len(groups) != 0 {
		t.Errorf("a hidden category should produce no group, got %+v", groups)
	}
	// ...but its commits are still counted.
	if stats := Statistics("", entries, categories); stats.Commits != 1 {
		t.Errorf("a hidden category's commits should still be counted, got %+v", stats)
	}
}

// A breaking change is called out at the top and also left under its own
// category, so it can be neither missed nor lost.
func TestGroupsBreakingChangesAppearTwice(t *testing.T) {
	entries := ParseAll([]Commit{
		{SHA: "aaaaaaabbbb", Subject: "feat(api)!: drop v1", Body: "BREAKING CHANGE: use v2"},
	})
	groups := Groups(entries, DefaultCategories(), testRepo)

	if len(groups) != 2 {
		t.Fatalf("want a Breaking group and a Features group, got %d", len(groups))
	}
	if groups[0].Key != BreakingKey {
		t.Errorf("the breaking callout should come first, got %q", groups[0].Key)
	}
	if groups[1].Key != "feat" {
		t.Errorf("the commit should remain under its own category, got %q", groups[1].Key)
	}

	// The explanatory note belongs to the callout only, so it is not repeated.
	if groups[0].Items[0].BreakingNote != "use v2" {
		t.Errorf("callout note = %q", groups[0].Items[0].BreakingNote)
	}
	if groups[1].Items[0].BreakingNote != "" {
		t.Errorf("the note should not repeat under Features, got %q", groups[1].Items[0].BreakingNote)
	}
	if !groups[1].Items[0].Breaking {
		t.Error("the item should still be flagged as breaking")
	}
}

func TestGroupsNoBreakingCalloutWhenNoneBreak(t *testing.T) {
	groups := Groups(ParseAll([]Commit{{SHA: "a", Subject: "feat: safe"}}), DefaultCategories(), testRepo)
	for _, g := range groups {
		if g.Key == BreakingKey {
			t.Error("a release with no breaking change should have no callout")
		}
	}
}

func TestItemRendering(t *testing.T) {
	entries := ParseAll([]Commit{{SHA: "aaaaaaabbbbbbb", Subject: "feat(cli): add --dry-run."}})

	linked := Groups(entries, DefaultCategories(), testRepo)[0].Items[0]
	if linked.Title != "Add --dry-run" {
		t.Errorf("Title = %q", linked.Title)
	}
	if linked.Text != "**cli:** Add --dry-run" {
		t.Errorf("Text = %q", linked.Text)
	}
	if linked.ShortSHA != "aaaaaaa" {
		t.Errorf("ShortSHA = %q", linked.ShortSHA)
	}
	if want := "([aaaaaaa](https://github.com/teddynted/repo/commit/aaaaaaabbbbbbb))"; linked.Link != want {
		t.Errorf("Link = %q, want %q", linked.Link, want)
	}

	// Without a known repository the SHA is still shown, just not linked.
	plain := Groups(entries, DefaultCategories(), Repository{})[0].Items[0]
	if plain.Link != "(aaaaaaa)" {
		t.Errorf("unlinked Link = %q", plain.Link)
	}
	if plain.URL != "" {
		t.Errorf("unlinked URL = %q, want empty", plain.URL)
	}
}

func TestItemWithoutSHAHasNoLink(t *testing.T) {
	item := Groups(ParseAll([]Commit{{Subject: "feat: no sha"}}), DefaultCategories(), testRepo)[0].Items[0]
	if item.Link != "" {
		t.Errorf("Link = %q, want empty", item.Link)
	}
}

func TestItemWithoutScopeHasPlainText(t *testing.T) {
	item := Groups(ParseAll([]Commit{{SHA: "a", Subject: "feat: no scope"}}), DefaultCategories(), testRepo)[0].Items[0]
	if item.Text != "No scope" {
		t.Errorf("Text = %q", item.Text)
	}
}

// Adding a category should be a data change and nothing else.
func TestCustomCategory(t *testing.T) {
	categories := []Category{
		{Key: "security", Title: "Security", Icon: "🔒", Label: "Security", Types: []string{"sec", "security"}},
		{Key: OtherKey, Title: "Other Changes", Label: "Other", Types: []string{""}},
	}
	entries := ParseAll([]Commit{
		{SHA: "a", Subject: "sec: patch the parser"},
		{SHA: "b", Subject: "feat: unclaimed now"},
	})
	groups := Groups(entries, categories, testRepo)

	if len(groups) != 2 || groups[0].Heading != "🔒 Security" {
		t.Fatalf("groups = %+v", groups)
	}
	// "feat" is claimed by no category here, so it falls to the catch-all.
	if groups[1].Key != OtherKey || len(groups[1].Items) != 1 {
		t.Errorf("unclaimed types should fall through to the catch-all, got %+v", groups[1])
	}
}

// Without a catch-all, an unclaimed commit is dropped. That is the caller's
// choice, but it should be a deliberate one.
func TestNoCatchAllDropsUnclaimedTypes(t *testing.T) {
	categories := []Category{{Key: "feat", Title: "Features", Label: "Features", Types: []string{"feat"}}}
	groups := Groups(ParseAll([]Commit{{SHA: "a", Subject: "chore: tidy"}}), categories, testRepo)
	if len(groups) != 0 {
		t.Errorf("groups = %+v, want none", groups)
	}
}
