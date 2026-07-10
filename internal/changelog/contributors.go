package changelog

import (
	"cmp"
	"slices"
	"strings"
)

// Contributor is one author and the number of commits they landed in a release.
type Contributor struct {
	Name    string
	Email   string
	Commits int
}

// Contributors summarises who worked on a release, most prolific first, then
// alphabetically so that the order is stable between runs.
//
// Authors are keyed on email rather than name, because the same person often
// commits under several spellings of their name. Commits whose author cannot be
// determined are skipped rather than reported as an empty contributor: a
// missing name is not worth failing a release over.
func Contributors(entries []Entry) []Contributor {
	byEmail := make(map[string]*Contributor)
	var order []string

	for _, e := range entries {
		email := strings.TrimSpace(strings.ToLower(e.Commit.AuthorEmail))
		name := strings.TrimSpace(e.Commit.AuthorName)

		key := email
		if key == "" {
			key = strings.ToLower(name)
		}
		if key == "" {
			continue // no author information at all
		}

		if existing, ok := byEmail[key]; ok {
			existing.Commits++
			continue
		}
		byEmail[key] = &Contributor{Name: name, Email: email, Commits: 1}
		order = append(order, key)
	}

	out := make([]Contributor, 0, len(order))
	for _, key := range order {
		out = append(out, *byEmail[key])
	}

	slices.SortFunc(out, func(a, b Contributor) int {
		if c := cmp.Compare(b.Commits, a.Commits); c != 0 {
			return c
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}
