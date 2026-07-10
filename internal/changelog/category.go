package changelog

import "slices"

// Keys for the two categories that are not driven by a commit type.
const (
	// BreakingKey names the callout that collects every breaking change,
	// whatever its type.
	BreakingKey = "breaking"
	// OtherKey names the catch-all for commits whose type no category claims,
	// including subjects that are not Conventional Commits at all.
	OtherKey = "other"
)

// Category is one heading in the rendered output.
//
// Adding a commit type to the release notes means adding a Category to
// DefaultCategories: nothing else in the package needs to change. A category
// whose Types contains "" is the catch-all, and also collects commits whose
// type is claimed by no other category.
type Category struct {
	// Key identifies the category to templates and statistics.
	Key string
	// Title is the heading text, without the icon.
	Title string
	// Icon is an optional emoji shown before the title.
	Icon string
	// Label is the short name used in the statistics block, where "Bug Fixes"
	// reads better as "Fixes".
	Label string
	// Types are the Conventional Commit types filed under this category.
	Types []string
	// Hidden keeps the category out of rendered output. Its commits are still
	// parsed and still counted in the statistics.
	Hidden bool
}

// Heading is the category's icon and title, as it appears in Markdown.
func (c Category) Heading() string {
	if c.Icon == "" {
		return c.Title
	}
	return c.Icon + " " + c.Title
}

// catchAll reports whether the category collects unclaimed commit types.
func (c Category) catchAll() bool { return slices.Contains(c.Types, "") }

// DefaultCategories is the built-in layout, in the order the sections appear.
//
// Every Conventional Commit type recognised by the specification has a home, so
// nothing is silently dropped. Housekeeping types are shown but ranked last;
// set Hidden on a category to suppress it instead.
func DefaultCategories() []Category {
	return []Category{
		{Key: "feat", Title: "Features", Icon: "🚀", Label: "Features", Types: []string{"feat"}},
		{Key: "fix", Title: "Bug Fixes", Icon: "🐛", Label: "Fixes", Types: []string{"fix"}},
		{Key: "perf", Title: "Performance", Icon: "⚡", Label: "Performance", Types: []string{"perf"}},
		{Key: "refactor", Title: "Refactoring", Icon: "♻️", Label: "Refactors", Types: []string{"refactor"}},
		{Key: "docs", Title: "Documentation", Icon: "📚", Label: "Documentation", Types: []string{"docs"}},
		{Key: "revert", Title: "Reverts", Icon: "⏪", Label: "Reverts", Types: []string{"revert"}},
		{Key: "test", Title: "Tests", Icon: "🧪", Label: "Tests", Types: []string{"test"}},
		{Key: "build", Title: "Build System", Icon: "📦", Label: "Build", Types: []string{"build"}},
		{Key: "ci", Title: "Continuous Integration", Icon: "🔧", Label: "CI", Types: []string{"ci"}},
		{Key: "style", Title: "Styles", Icon: "🎨", Label: "Style", Types: []string{"style"}},
		{Key: "chore", Title: "Chores", Icon: "🧹", Label: "Chores", Types: []string{"chore"}},
		{Key: OtherKey, Title: "Other Changes", Label: "Other", Types: []string{""}},
	}
}

// breakingCategory is the callout prepended to the output whenever a release
// contains a breaking change.
func breakingCategory() Category {
	return Category{Key: BreakingKey, Title: "Breaking Changes", Icon: "⚠️", Label: "Breaking"}
}

// Item is one rendered bullet: a commit, formatted for display.
type Item struct {
	// Title is the commit description, capitalised and stripped of its
	// Conventional Commit prefix and trailing full stop.
	Title string
	// Scope is the optional Conventional Commit scope.
	Scope string

	SHA      string
	ShortSHA string
	// URL is a permalink to the commit, or "" when the repository is unknown.
	URL string

	Breaking bool
	// BreakingNote is set only on items in the Breaking Changes callout, so
	// that the note is not repeated under the commit's own category.
	BreakingNote string

	// Text is the bullet body: the title, prefixed by a bold scope when there
	// is one.
	Text string
	// Link is the trailing commit reference, "([abc1234](url))" when the
	// repository is known and "(abc1234)" when it is not. It is empty for a
	// commit with no SHA.
	Link string
}

// Group is a category together with the items filed under it. Only non-empty
// groups are produced, so a template never has to test for emptiness.
type Group struct {
	Key     string
	Title   string
	Icon    string
	Heading string
	Items   []Item
}

// Groups classifies entries into the given categories, newest commit first
// within each group, and returns only the groups that have items.
//
// A Breaking Changes group is prepended when any entry is breaking. Those
// entries also remain under their own category: the callout exists so a
// breaking change cannot be missed, not to move it out of its section.
func Groups(entries []Entry, categories []Category, repo Repository) []Group {
	var groups []Group

	if breaking := breakingItems(entries, repo); len(breaking) > 0 {
		groups = append(groups, newGroup(breakingCategory(), breaking))
	}

	claimed := claimedTypes(categories)
	for _, category := range categories {
		if category.Hidden {
			continue
		}
		if items := itemsFor(entries, category, claimed, repo); len(items) > 0 {
			groups = append(groups, newGroup(category, items))
		}
	}
	return groups
}

func newGroup(c Category, items []Item) Group {
	return Group{Key: c.Key, Title: c.Title, Icon: c.Icon, Heading: c.Heading(), Items: items}
}

// claimedTypes is the set of commit types that some category names explicitly.
func claimedTypes(categories []Category) map[string]bool {
	claimed := make(map[string]bool)
	for _, c := range categories {
		for _, t := range c.Types {
			claimed[t] = true
		}
	}
	return claimed
}

// belongsTo reports whether an entry is filed under a category, taking the
// catch-all rule into account.
func belongsTo(e Entry, c Category, claimed map[string]bool) bool {
	if slices.Contains(c.Types, e.Type) {
		return true
	}
	return c.catchAll() && !claimed[e.Type]
}

func itemsFor(entries []Entry, c Category, claimed map[string]bool, repo Repository) []Item {
	var items []Item
	for _, e := range entries {
		if belongsTo(e, c, claimed) {
			items = append(items, newItem(e, repo, false))
		}
	}
	return dedupe(items)
}

func breakingItems(entries []Entry, repo Repository) []Item {
	var items []Item
	for _, e := range entries {
		if e.Breaking {
			items = append(items, newItem(e, repo, true))
		}
	}
	return dedupe(items)
}

// dedupe removes items that say the same thing, keeping the first — which, in
// git log order, is the newest.
//
// A cherry-pick onto a release branch, or a commit reverted and reapplied,
// produces two commits with identical scope and subject. Listing both tells the
// reader nothing and looks like a bug in the tool.
func dedupe(items []Item) []Item {
	if len(items) < 2 {
		return items
	}

	seen := make(map[string]bool, len(items))
	out := items[:0:0]
	for _, item := range items {
		key := item.Scope + "\x00" + item.Title
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

// newItem renders one entry. withNote is true only for the Breaking Changes
// callout, so the explanatory note appears exactly once.
func newItem(e Entry, repo Repository, withNote bool) Item {
	item := Item{
		Title:    e.Title(),
		Scope:    e.Scope,
		SHA:      e.Commit.SHA,
		ShortSHA: shortSHA(e.Commit.SHA),
		URL:      repo.CommitURL(e.Commit.SHA),
		Breaking: e.Breaking,
	}
	if withNote {
		item.BreakingNote = e.BreakingNote
	}

	item.Text = item.Title
	if item.Scope != "" {
		item.Text = "**" + item.Scope + ":** " + item.Title
	}

	switch {
	case item.ShortSHA == "":
	case item.URL == "":
		item.Link = "(" + item.ShortSHA + ")"
	default:
		item.Link = "([" + item.ShortSHA + "](" + item.URL + "))"
	}
	return item
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
