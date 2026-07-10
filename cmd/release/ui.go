package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// ANSI escapes, used only when the output is an interactive terminal.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// The status glyphs, used consistently by every command.
const (
	glyphSuccess = "✓"
	glyphWarning = "⚠"
	glyphError   = "✗"
	glyphInfo    = "ℹ"
	glyphBullet  = "•"
)

// ruleWidth is the width of a horizontal rule, chosen to sit comfortably inside
// an 80-column terminal.
const ruleWidth = 38

// labelGap is the space between the widest label in a table and its values.
const labelGap = 4

// printer writes progress to a terminal. Every message goes to stderr except
// generated content, which goes to stdout so it can be redirected into a file
// or piped without the progress chatter.
type printer struct {
	out   io.Writer
	err   io.Writer
	color bool
}

func newPrinter(out, errw io.Writer, color bool) *printer {
	return &printer{out: out, err: errw, color: color}
}

// paint wraps s in an escape sequence when colour is enabled.
func (p *printer) paint(code, s string) string {
	if !p.color {
		return s
	}
	return code + s + ansiReset
}

// success reports work that completed.
func (p *printer) success(format string, a ...any) {
	p.glyph(ansiGreen, glyphSuccess, format, a...)
}

// warn reports something the user should notice but which is not a failure.
func (p *printer) warn(format string, a ...any) {
	p.glyph(ansiYellow, glyphWarning, format, a...)
}

// fail reports a failure.
func (p *printer) fail(format string, a ...any) {
	p.glyph(ansiRed, glyphError, format, a...)
}

// info reports work about to happen, or context worth knowing.
func (p *printer) info(format string, a ...any) {
	p.glyph(ansiCyan, glyphInfo, format, a...)
}

// bullet is an item in a list, used for the dry-run action summary.
func (p *printer) bullet(format string, a ...any) {
	p.glyph(ansiDim, glyphBullet, format, a...)
}

func (p *printer) glyph(color, glyph, format string, a ...any) {
	fmt.Fprintf(p.err, "%s %s\n", p.paint(color, glyph), fmt.Sprintf(format, a...))
}

// note is an unadorned, dimmed line.
func (p *printer) note(format string, a ...any) {
	fmt.Fprintf(p.err, "%s\n", p.paint(ansiDim, fmt.Sprintf(format, a...)))
}

// blank prints a single empty line. Commands rely on it rather than embedding
// newlines in messages, so that spacing stays consistent between them.
func (p *printer) blank() { fmt.Fprintln(p.err) }

// heading introduces a block, and is always followed by a blank line.
func (p *printer) heading(format string, a ...any) {
	fmt.Fprintf(p.err, "%s\n\n", p.paint(ansiBold, fmt.Sprintf(format, a...)))
}

// rule draws a horizontal divider.
func (p *printer) rule() {
	fmt.Fprintln(p.err, p.paint(ansiDim, strings.Repeat("─", ruleWidth)))
}

// row is one line of a table: a label and its value.
type row struct {
	label string
	value string
	// bold highlights the value, for the line that matters most.
	bold bool
}

// table prints label/value pairs in two aligned columns.
//
// The column width is derived from the longest label, so the layout adapts to
// whatever labels a caller passes, and never depends on the width of a value:
// a long repository name simply runs on past the column.
func (p *printer) table(rows []row) {
	width := 0
	for _, r := range rows {
		// Count runes, not bytes, so a non-ASCII label still aligns.
		if n := utf8.RuneCountInString(r.label); n > width {
			width = n
		}
	}
	width += labelGap

	for _, r := range rows {
		value := r.value
		if r.bold {
			value = p.paint(ansiBold, value)
		}
		pad := width - utf8.RuneCountInString(r.label)
		fmt.Fprintf(p.err, "%s%s%s\n", p.paint(ansiDim, r.label), strings.Repeat(" ", pad), value)
	}
}

// dryRunBanner announces, unmissably, that nothing will be changed.
func (p *printer) dryRunBanner() {
	p.rule()
	p.blank()
	fmt.Fprintln(p.err, p.paint(ansiYellow, p.paint(ansiBold, "DRY RUN")))
	p.blank()
	p.note("No Git tags will be created.")
	p.note("No GitHub releases will be published.")
	p.note("No repository changes will be made.")
	p.blank()
	p.rule()
}

// confirm asks a yes/no question. Anything other than an explicit yes is a no.
func confirm(in io.Reader, out io.Writer, question string) (bool, error) {
	fmt.Fprintf(out, "%s [y/N] ", question)

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// isTerminal reports whether f is an interactive terminal, which decides both
// whether to colourise and whether it is safe to prompt.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// useColor honours the NO_COLOR convention (https://no-color.org) and the
// --no-color flag, and never colourises a redirected stream.
func useColor(disabled bool, f *os.File) bool {
	if disabled || os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTerminal(f)
}
