package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
)

// newTestPrinter captures output, with colour disabled and a fixed width so
// assertions compare plain text and stable layout.
func newTestPrinter() (*printer, *strings.Builder, *strings.Builder) {
	var out, errw strings.Builder
	p := newPrinter(&out, &errw, options{width: 80})
	return p, &out, &errw
}

func TestTableAlignsOnTheLongestLabel(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.table([]row{
		{label: "Current Version", value: "v1.2.3"},
		{label: "Branch", value: "main"},
		{label: "Increment Type", value: "Minor"},
	})

	lines := strings.Split(strings.TrimRight(errw.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	// "Current Version" is 15 runes; every value starts at 15 + labelGap.
	want := len("Current Version") + labelGap
	for _, line := range lines {
		at := strings.Index(line, strings.TrimLeft(line[want:], " "))
		if at != want {
			t.Errorf("value starts at column %d, want %d: %q", at, want, line)
		}
	}
}

// Alignment depends on the labels, never on how long a value happens to be.
func TestTableIgnoresValueLength(t *testing.T) {
	short, _, shortOut := newTestPrinter()
	short.table([]row{{label: "Repository", value: "a/b"}, {label: "Branch", value: "main"}})

	long, _, longOut := newTestPrinter()
	long.table([]row{
		{label: "Repository", value: strings.Repeat("x", 60)},
		{label: "Branch", value: "main"},
	})

	shortBranch := strings.Split(strings.TrimSpace(shortOut.String()), "\n")[1]
	longBranch := strings.Split(strings.TrimSpace(longOut.String()), "\n")[1]
	if shortBranch != longBranch {
		t.Errorf("a long value changed the layout:\n%q\n%q", shortBranch, longBranch)
	}
}

// A value wider than the terminal is truncated, because wrapping it would break
// the column.
func TestTableTruncatesOverlongValues(t *testing.T) {
	var out, errw strings.Builder
	p := newPrinter(&out, &errw, options{width: 40})
	p.table([]row{{label: "Repository", value: strings.Repeat("x", 100)}})

	line := strings.TrimRight(errw.String(), "\n")
	if len([]rune(line)) > 40 {
		t.Errorf("line is %d columns wide, want at most 40: %q", len([]rune(line)), line)
	}
	if !strings.HasSuffix(line, "…") {
		t.Errorf("a truncated value should end in an ellipsis: %q", line)
	}
}

// A label containing non-ASCII text must still align: padding counts runes.
func TestTableAlignsMultiByteLabels(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.table([]row{{label: "Ähnlich", value: "1"}, {label: "B", value: "2"}})

	lines := strings.Split(strings.TrimSpace(errw.String()), "\n")
	first := strings.Index(lines[0], "1")
	second := strings.Index(lines[1], "2")
	// Byte offsets differ because "Ä" is two bytes; rune columns must match.
	if len([]rune(lines[0][:first])) != len([]rune(lines[1][:second])) {
		t.Errorf("multi-byte label broke the alignment:\n%q\n%q", lines[0], lines[1])
	}
}

func TestTableEmpty(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.table(nil)
	if errw.String() != "" {
		t.Errorf("an empty table should print nothing, got %q", errw.String())
	}
}

func TestStatusGlyphs(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.success("done")
	p.warn("careful")
	p.fail("broken")
	p.info("noted")
	p.bullet("item")

	th := unicodeTheme()
	got := errw.String()
	for glyph, message := range map[string]string{
		th.success: "done",
		th.warning: "careful",
		th.failure: "broken",
		th.info:    "noted",
		th.bullet:  "item",
	} {
		if !strings.Contains(got, glyph+" "+message) {
			t.Errorf("expected %q followed by %q\n---\n%s", glyph, message, got)
		}
	}
}

// A long status message wraps under its text, not under its glyph, so the
// column of glyphs stays readable.
func TestStatusWrapsUnderTheText(t *testing.T) {
	var out, errw strings.Builder
	p := newPrinter(&out, &errw, options{width: 40})
	p.success("%s", strings.Repeat("word ", 15))

	lines := strings.Split(strings.TrimRight(errw.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected the message to wrap, got %q", errw.String())
	}
	if !strings.HasPrefix(lines[0], unicodeTheme().success+" ") {
		t.Errorf("the first line should carry the glyph: %q", lines[0])
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("continuation should be indented under the text: %q", line)
		}
	}
	for _, line := range lines {
		if len([]rune(line)) > 40 {
			t.Errorf("line exceeds the width: %q", line)
		}
	}
}

// Generated content goes to stdout; the report goes to stderr, so notes can be
// piped without the report.
func TestReportGoesToStderr(t *testing.T) {
	p, out, errw := newTestPrinter()
	p.success("progress")

	if out.String() != "" {
		t.Errorf("the report leaked to stdout: %q", out.String())
	}
	if !strings.Contains(errw.String(), "progress") {
		t.Errorf("the report is missing from stderr: %q", errw.String())
	}
}

func TestColorIsOptional(t *testing.T) {
	var out, errw strings.Builder
	newPrinter(&out, &errw, options{color: true, width: 80}).success("hello")
	if !strings.Contains(errw.String(), ansiGreen) {
		t.Error("a colour-enabled printer should emit escapes")
	}

	p, _, plain := newTestPrinter()
	p.success("hello")
	if strings.Contains(plain.String(), "\x1b[") {
		t.Errorf("a colour-disabled printer must emit no escapes: %q", plain.String())
	}
}

// Colour must not disturb the layout: a painted glyph is still one column.
func TestColorDoesNotBreakWrapping(t *testing.T) {
	var out, errw strings.Builder
	newPrinter(&out, &errw, options{color: true, width: 40}).success("%s", strings.Repeat("word ", 15))

	for _, line := range strings.Split(strings.TrimRight(errw.String(), "\n"), "\n")[1:] {
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("continuation should be indented by two columns: %q", line)
		}
	}
}

func TestVerbosity(t *testing.T) {
	for _, tt := range []struct {
		level       verbosity
		wantVerbose bool
		wantDebug   bool
	}{
		{levelNormal, false, false},
		{levelVerbose, true, false},
		{levelDebug, true, true},
	} {
		var out, errw strings.Builder
		p := newPrinter(&out, &errw, options{width: 80, level: tt.level})
		p.verbosef("narration")
		p.debugf("internals")

		got := errw.String()
		if strings.Contains(got, "narration") != tt.wantVerbose {
			t.Errorf("level %d: verbosef visible = %v, want %v", tt.level, !tt.wantVerbose, tt.wantVerbose)
		}
		if strings.Contains(got, "internals") != tt.wantDebug {
			t.Errorf("level %d: debugf visible = %v, want %v", tt.level, !tt.wantDebug, tt.wantDebug)
		}
	}
}

// Implementation detail must never reach a normal run.
func TestNormalRunHidesDiagnostics(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.debugf("remote=origin tag-prefix=%q", "v")
	p.verbosef("calculating the next version")
	if errw.String() != "" {
		t.Errorf("a normal run printed diagnostics: %q", errw.String())
	}
}

func TestDryRunBannerIsUnmissable(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.dryRunBanner()

	got := errw.String()
	for _, want := range []string{
		"DRY RUN",
		"Nothing will be modified.",
		"Nothing will be pushed.",
		"Nothing will be published.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner missing %q\n---\n%s", want, got)
		}
	}
	if n := strings.Count(got, strings.Repeat("─", 80)); n != 2 {
		t.Errorf("the banner should be fenced by two rules, found %d\n---\n%s", n, got)
	}
}

func TestSectionIsPrecededByABlankLine(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.section("Release Plan")
	if got := errw.String(); got != "\nRelease Plan\n\n" {
		t.Errorf("section() = %q", got)
	}
}

func TestStars(t *testing.T) {
	p, _, _ := newTestPrinter()
	if got := p.stars(5); got != "★★★★★" {
		t.Errorf("stars(5) = %q", got)
	}
	if got := p.stars(3); got != "★★★☆☆" {
		t.Errorf("stars(3) = %q", got)
	}
	// Out-of-range ratings are clamped rather than panicking.
	if got := p.stars(9); got != "★★★★★" {
		t.Errorf("stars(9) = %q", got)
	}
	if got := p.stars(-1); got != "☆☆☆☆☆" {
		t.Errorf("stars(-1) = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10, "…"); got != "hello" {
		t.Errorf("a short string should be untouched, got %q", got)
	}
	if got := truncate("hello world", 8, "…"); got != "hello w…" {
		t.Errorf("truncate() = %q", got)
	}
	// Too narrow to truncate usefully: a mangled value is worse than a long one.
	if got := truncate("hello world", 3, "…"); got != "hello world" {
		t.Errorf("truncate() at a tiny width = %q", got)
	}
	// Runes, not bytes: each of these is two bytes wide but one column.
	if got := truncate("äöüäöüäöü", 8, "…"); len([]rune(got)) != 8 {
		t.Errorf("truncate() = %q, %d runes, want 8", got, len([]rune(got)))
	}
}

func TestWrap(t *testing.T) {
	got := wrap("the quick brown fox jumps", 10)
	for _, line := range got {
		if len(line) > 10 {
			t.Errorf("line %q exceeds 10 columns", line)
		}
	}
	if strings.Join(got, " ") != "the quick brown fox jumps" {
		t.Errorf("wrap() lost or reordered words: %v", got)
	}
}

// A URL must stay copyable, so an overlong word is never broken.
func TestWrapDoesNotBreakLongWords(t *testing.T) {
	url := "https://github.com/teddynted/designing-an-ai-agent-platform-on-aws/compare/v0.0.0...v0.1.0"
	got := wrap(url, 40)
	if len(got) != 1 || got[0] != url {
		t.Errorf("wrap() broke a long word: %v", got)
	}
}

func TestWrapEmpty(t *testing.T) {
	if got := wrap("", 40); len(got) != 1 || got[0] != "" {
		t.Errorf("wrap(\"\") = %v", got)
	}
}

func TestStripANSI(t *testing.T) {
	if got := stripANSI(ansiGreen + "✓" + ansiReset); got != "✓" {
		t.Errorf("stripANSI() = %q", got)
	}
	if got := stripANSI("plain"); got != "plain" {
		t.Errorf("stripANSI() = %q", got)
	}
}

func TestConfirm(t *testing.T) {
	for input, want := range map[string]bool{
		"y\n": true, "Y\n": true, "yes\n": true, "YES\n": true,
		"n\n": false, "\n": false, "": false, "maybe\n": false,
	} {
		var out strings.Builder
		got, err := confirm(strings.NewReader(input), &out, "Proceed?")
		if err != nil {
			t.Fatalf("confirm(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("confirm(%q) = %v, want %v", input, got, want)
		}
		if !strings.Contains(out.String(), "Proceed? [y/N]") {
			t.Errorf("confirm should print the question, got %q", out.String())
		}
	}
}

func TestUseColorHonoursNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if useColor(false, nil) {
		t.Error("NO_COLOR should disable colour")
	}
}

// A release.Error lays itself out across several lines: a summary, the offending
// paths, and a list of remedies. Wrapping the whole message as one blob would
// run all of that into an unreadable paragraph.
func TestFailurePreservesStructure(t *testing.T) {
	err := &release.Error{
		Cause:     release.ErrDirtyWorkTree,
		What:      "The working tree has uncommitted changes.",
		Why:       "A release must describe a commit, so the tree has to be clean:\n\n" + "  M go.mod\n  ?? scratch.txt",
		Solutions: []string{"commit the changes", "set them aside: git stash"},
	}

	var out, errw strings.Builder
	newPrinter(&out, &errw, options{width: 76}).failure(err)

	lines := strings.Split(strings.TrimRight(errw.String(), "\n"), "\n")
	if !strings.HasPrefix(lines[0], unicodeTheme().failure+" The working tree") {
		t.Errorf("the summary should carry the failure glyph: %q", lines[0])
	}

	got := errw.String()
	for _, want := range []string{
		"\n    M go.mod\n",
		"\n    ?? scratch.txt\n",
		"\n  Possible solutions:\n",
		"\n  • commit the changes\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("failure() lost the line %q:\n%s", want, got)
		}
	}
	// Blank lines survive, so the remedies do not run into the paths.
	if !strings.Contains(got, "\n\n") {
		t.Errorf("failure() collapsed the blank lines:\n%s", got)
	}
}

// A long single-line error still wraps, so it never runs off a narrow terminal.
func TestFailureWrapsLongLines(t *testing.T) {
	err := errors.New(strings.Repeat("word ", 30))

	var out, errw strings.Builder
	newPrinter(&out, &errw, options{width: 40}).failure(err)

	for _, line := range strings.Split(strings.TrimRight(errw.String(), "\n"), "\n") {
		if len([]rune(line)) > 40 {
			t.Errorf("line exceeds the width: %q", line)
		}
	}
}
