package changelog

import (
	"strings"
	"testing"
)

func TestInsertIntoEmptyFile(t *testing.T) {
	out, changed := Insert(nil, "1.0.0", "## [1.0.0] - 2026-07-10\n\n### Features\n\n- first\n")
	if !changed {
		t.Fatal("inserting into an empty file should change it")
	}
	got := string(out)
	if !strings.HasPrefix(got, "# Changelog") {
		t.Errorf("a new file should get the standard header:\n%s", got)
	}
	if !strings.Contains(got, "## [1.0.0]") {
		t.Errorf("the entry is missing:\n%s", got)
	}
}

func TestInsertPrependsNewestFirst(t *testing.T) {
	existing := Header + "\n## [1.0.0] - 2026-01-01\n\n### Features\n\n- first\n"
	out, changed := Insert([]byte(existing), "1.1.0", "## [1.1.0] - 2026-07-10\n\n### Features\n\n- second\n")
	if !changed {
		t.Fatal("a new version should change the file")
	}
	got := string(out)

	newest, oldest := strings.Index(got, "## [1.1.0]"), strings.Index(got, "## [1.0.0]")
	if newest == -1 || oldest == -1 {
		t.Fatalf("both versions should be present:\n%s", got)
	}
	if newest > oldest {
		t.Errorf("1.1.0 should precede 1.0.0:\n%s", got)
	}
	if !strings.HasPrefix(got, "# Changelog") {
		t.Errorf("the preamble should be preserved:\n%s", got)
	}
}

// Re-running the release workflow for a tag must not duplicate its entry.
func TestInsertIsIdempotent(t *testing.T) {
	entry := "## [1.1.0] - 2026-07-10\n\n### Features\n\n- second\n"
	first, _ := Insert([]byte(Header), "1.1.0", entry)
	second, changed := Insert(first, "1.1.0", entry)
	if changed {
		t.Error("inserting the same version twice should report no change")
	}
	if string(first) != string(second) {
		t.Error("inserting the same version twice should leave the file untouched")
	}
}

func TestInsertPreambleOnly(t *testing.T) {
	out, changed := Insert([]byte("# Changelog\n\nSome preamble.\n"), "1.0.0", "## [1.0.0] - 2026-07-10\n")
	if !changed {
		t.Fatal("should have changed")
	}
	got := string(out)
	if !strings.HasPrefix(got, "# Changelog\n\nSome preamble.\n\n## [1.0.0]") {
		t.Errorf("the entry should follow the preamble:\n%q", got)
	}
}

// The seeded CHANGELOG.md in the repository must be recognised as a preamble,
// not mistaken for an existing entry.
func TestInsertIntoSeededHeader(t *testing.T) {
	out, changed := Insert([]byte(Header), "0.1.0", "## [0.1.0] - 2026-07-10\n\n### Features\n\n- first\n")
	if !changed {
		t.Fatal("the seeded header should accept a first entry")
	}
	got := string(out)
	if !strings.HasPrefix(got, Header) {
		t.Errorf("the header should be preserved verbatim:\n%s", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("no blank-line runs should be introduced:\n%q", got)
	}
}
