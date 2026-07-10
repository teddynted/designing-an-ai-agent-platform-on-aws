package changelog

import "testing"

func TestParseConventionalSubjects(t *testing.T) {
	tests := []struct {
		subject  string
		wantType string
		scope    string
		want     string
		breaking bool
	}{
		{"feat: add release command", "feat", "", "add release command", false},
		{"fix(semver): reject leading zeros", "fix", "semver", "reject leading zeros", false},
		{"feat(api)!: drop v1 endpoints", "feat", "api", "drop v1 endpoints", true},
		{"FEAT: upper case type", "feat", "", "upper case type", false},
		{"refactor(git)!:\tuse a tab separator", "refactor", "git", "use a tab separator", true},

		// Not Conventional Commits: kept, but unclassified.
		{"Merge branch 'main'", "", "", "Merge branch 'main'", false},
		{"update readme", "", "", "update readme", false},
		{"feat:missing space", "", "", "feat:missing space", false},
		{"feat(): empty scope", "", "", "feat(): empty scope", false},
		{"feat:", "", "", "feat:", false},
	}
	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			e := Parse(Commit{Subject: tt.subject})
			if e.Type != tt.wantType || e.Scope != tt.scope || e.Subject != tt.want || e.Breaking != tt.breaking {
				t.Errorf("Parse(%q) = type=%q scope=%q subject=%q breaking=%v; want type=%q scope=%q subject=%q breaking=%v",
					tt.subject, e.Type, e.Scope, e.Subject, e.Breaking, tt.wantType, tt.scope, tt.want, tt.breaking)
			}
		})
	}
}

func TestParseBreakingFooter(t *testing.T) {
	for _, prefix := range []string{"BREAKING CHANGE:", "BREAKING-CHANGE:"} {
		body := "Some context.\n\n" + prefix + " the --force flag\nis gone entirely.\n\nRefs: #12"
		e := Parse(Commit{Subject: "fix: tidy flags", Body: body})
		if !e.Breaking {
			t.Fatalf("%s footer should mark the entry breaking", prefix)
		}
		if want := "the --force flag is gone entirely."; e.BreakingNote != want {
			t.Errorf("BreakingNote = %q, want %q", e.BreakingNote, want)
		}
	}

	if e := Parse(Commit{Subject: "fix: unrelated", Body: "BREAKING CHANGE: on the first line"}); !e.Breaking {
		t.Error("a footer on the first body line should still count")
	}
	if e := Parse(Commit{Subject: "fix: unrelated", Body: "no footer here"}); e.Breaking {
		t.Error("a body without a footer must not be breaking")
	}
	// The footer must start a line: prose that merely mentions it is not a footer.
	if e := Parse(Commit{Subject: "fix: unrelated", Body: "this is not a BREAKING CHANGE: really"}); e.Breaking {
		t.Error("a mid-line mention must not be treated as a footer")
	}
}

func TestParseAllPreservesOrder(t *testing.T) {
	entries := ParseAll([]Commit{{Subject: "feat: one"}, {Subject: "fix: two"}})
	if len(entries) != 2 || entries[0].Type != "feat" || entries[1].Type != "fix" {
		t.Errorf("ParseAll = %+v", entries)
	}
	if got := ParseAll(nil); len(got) != 0 {
		t.Errorf("ParseAll(nil) = %v, want empty", got)
	}
}

func TestTitle(t *testing.T) {
	tests := []struct{ in, want string }{
		// Capitalised, trailing full stop removed.
		{"add semantic versioning", "Add semantic versioning"},
		{"add semantic versioning.", "Add semantic versioning"},
		{"  add semantic versioning  ", "Add semantic versioning"},

		// Already capitalised, or not a letter: left alone.
		{"Add semantic versioning", "Add semantic versioning"},
		{"2FA support", "2FA support"},
		{"", ""},

		// An ellipsis is not a trailing full stop, so it survives.
		{"handle the rest...", "Handle the rest..."},

		// Only the trailing stop goes; internal punctuation stays.
		{"support v1.2.3 tags", "Support v1.2.3 tags"},
		{"fix e.g. handling.", "Fix e.g. handling"},

		// Identifiers that start lower-case on purpose are not corrupted.
		{"gRPC transport added", "gRPC transport added"},
		{"iOS build fixed", "iOS build fixed"},

		// A single lower-case letter still capitalises.
		{"a", "A"},

		// Non-ASCII first rune.
		{"ändere die Konfiguration", "Ändere die Konfiguration"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := Title(tt.in); got != tt.want {
				t.Errorf("Title(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The Conventional Commit prefix is removed by Parse, and Title tidies what is
// left. Together they turn a commit subject into a release-note line.
func TestEntryTitleStripsPrefixAndTidies(t *testing.T) {
	e := Parse(Commit{Subject: "feat(cli): add semantic versioning."})
	if got, want := e.Title(), "Add semantic versioning"; got != want {
		t.Errorf("Title() = %q, want %q", got, want)
	}
	if e.Subject != "add semantic versioning." {
		t.Errorf("Subject should stay as written, got %q", e.Subject)
	}
}
