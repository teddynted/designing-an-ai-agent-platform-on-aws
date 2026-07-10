package changelog

import "testing"

func TestContributors(t *testing.T) {
	entries := ParseAll([]Commit{
		{SHA: "a", Subject: "feat: one", AuthorName: "Teddy Kekana", AuthorEmail: "teddy@example.com"},
		{SHA: "b", Subject: "fix: two", AuthorName: "Ada Lovelace", AuthorEmail: "ada@example.com"},
		{SHA: "c", Subject: "docs: three", AuthorName: "Teddy Kekana", AuthorEmail: "teddy@example.com"},
		{SHA: "d", Subject: "test: four", AuthorName: "Teddy Kekana", AuthorEmail: "teddy@example.com"},
	})
	got := Contributors(entries)

	if len(got) != 2 {
		t.Fatalf("Contributors() = %+v, want 2", got)
	}
	// Most prolific first.
	if got[0].Name != "Teddy Kekana" || got[0].Commits != 3 {
		t.Errorf("first contributor = %+v", got[0])
	}
	if got[1].Name != "Ada Lovelace" || got[1].Commits != 1 {
		t.Errorf("second contributor = %+v", got[1])
	}
}

// The same person often commits under several spellings of their name, so
// authors are keyed on email.
func TestContributorsMergeByEmail(t *testing.T) {
	entries := ParseAll([]Commit{
		{SHA: "a", Subject: "feat: one", AuthorName: "Teddy Kekana", AuthorEmail: "teddy@example.com"},
		{SHA: "b", Subject: "fix: two", AuthorName: "teddynted", AuthorEmail: "Teddy@Example.com"},
	})
	got := Contributors(entries)

	if len(got) != 1 || got[0].Commits != 2 {
		t.Fatalf("Contributors() = %+v, want one author with 2 commits", got)
	}
	// The first spelling encountered wins.
	if got[0].Name != "Teddy Kekana" {
		t.Errorf("Name = %q", got[0].Name)
	}
}

// Ties break alphabetically, so the order is stable between runs.
func TestContributorsStableOrder(t *testing.T) {
	entries := ParseAll([]Commit{
		{SHA: "a", Subject: "feat: one", AuthorName: "Zoe", AuthorEmail: "z@example.com"},
		{SHA: "b", Subject: "fix: two", AuthorName: "Ada", AuthorEmail: "a@example.com"},
		{SHA: "c", Subject: "docs: three", AuthorName: "Mia", AuthorEmail: "m@example.com"},
	})
	got := Contributors(entries)

	want := []string{"Ada", "Mia", "Zoe"}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("Contributors()[%d] = %q, want %q", i, got[i].Name, name)
		}
	}
}

// Author information is often missing from a synthesised commit. That must not
// fail a release, and must not produce a blank contributor.
func TestContributorsFailsGracefully(t *testing.T) {
	entries := ParseAll([]Commit{
		{SHA: "a", Subject: "feat: no author at all"},
		{SHA: "b", Subject: "fix: named but no email", AuthorName: "Ada Lovelace"},
	})
	got := Contributors(entries)

	if len(got) != 1 || got[0].Name != "Ada Lovelace" {
		t.Errorf("Contributors() = %+v, want only the named author", got)
	}
	if n := len(Contributors(nil)); n != 0 {
		t.Errorf("Contributors(nil) returned %d contributors", n)
	}
}
