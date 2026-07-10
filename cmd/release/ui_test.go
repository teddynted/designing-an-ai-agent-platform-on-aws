package main

import (
	"strings"
	"testing"
)

// newTestPrinter captures output, with colour disabled so assertions compare
// plain text.
func newTestPrinter() (*printer, *strings.Builder, *strings.Builder) {
	var out, errw strings.Builder
	return newPrinter(&out, &errw, false), &out, &errw
}

func TestTableAlignsOnTheLongestLabel(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.table([]row{
		{label: "Repository", value: "teddynted/designing-an-ai-agent-platform-on-aws"},
		{label: "Branch", value: "main"},
		{label: "Release Date", value: "2026-07-10"},
	})

	lines := strings.Split(strings.TrimRight(errw.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	// "Release Date" is 12 runes; every value starts at column 12 + labelGap.
	want := len("Release Date") + labelGap
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
		{label: "Repository", value: strings.Repeat("x", 120)},
		{label: "Branch", value: "main"},
	})

	shortBranch := strings.Split(strings.TrimSpace(shortOut.String()), "\n")[1]
	longBranch := strings.Split(strings.TrimSpace(longOut.String()), "\n")[1]
	if shortBranch != longBranch {
		t.Errorf("a long value changed the layout:\n%q\n%q", shortBranch, longBranch)
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

func TestGlyphs(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.success("done")
	p.warn("careful")
	p.fail("broken")
	p.info("noted")
	p.bullet("item")

	got := errw.String()
	for glyph, message := range map[string]string{
		glyphSuccess: "done",
		glyphWarning: "careful",
		glyphError:   "broken",
		glyphInfo:    "noted",
		glyphBullet:  "item",
	} {
		if !strings.Contains(got, glyph+" "+message) {
			t.Errorf("expected %q followed by %q\n---\n%s", glyph, message, got)
		}
	}
}

// Generated content goes to stdout; progress goes to stderr, so notes can be
// piped without the chatter.
func TestProgressGoesToStderr(t *testing.T) {
	p, out, errw := newTestPrinter()
	p.success("progress")

	if out.String() != "" {
		t.Errorf("progress leaked to stdout: %q", out.String())
	}
	if !strings.Contains(errw.String(), "progress") {
		t.Errorf("progress missing from stderr: %q", errw.String())
	}
}

func TestColorIsOptional(t *testing.T) {
	var out, errw strings.Builder
	colored := newPrinter(&out, &errw, true)
	colored.success("hello")
	if !strings.Contains(errw.String(), ansiGreen) {
		t.Error("a colour-enabled printer should emit escapes")
	}

	p, _, plain := newTestPrinter()
	p.success("hello")
	if strings.Contains(plain.String(), "\x1b[") {
		t.Errorf("a colour-disabled printer must emit no escapes: %q", plain.String())
	}
}

func TestDryRunBannerIsUnmissable(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.dryRunBanner()

	got := errw.String()
	for _, want := range []string{
		"DRY RUN",
		"No Git tags will be created.",
		"No GitHub releases will be published.",
		"No repository changes will be made.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner missing %q\n---\n%s", want, got)
		}
	}
	if n := strings.Count(got, strings.Repeat("─", ruleWidth)); n != 2 {
		t.Errorf("the banner should be fenced by two rules, found %d\n---\n%s", n, got)
	}
}

func TestHeadingIsFollowedByABlankLine(t *testing.T) {
	p, _, errw := newTestPrinter()
	p.heading("Release plan")
	if got := errw.String(); got != "Release plan\n\n" {
		t.Errorf("heading() = %q", got)
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
