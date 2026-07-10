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

// labelGap is the space between the widest label in a table and its values.
const labelGap = 4

// status is the outcome of a single check, and selects the glyph and colour it
// is rendered with.
type status int

const (
	statusOK status = iota
	statusWarn
	statusFail
	statusInfo
)

// options configures a printer.
type options struct {
	color bool
	ascii bool
	width int
	level verbosity
}

// printer renders the report.
//
// Everything the user reads goes to stderr; only generated content — release
// notes, changelog entries — goes to stdout, so it can be redirected on its own
// while the report stays visible.
type printer struct {
	out   io.Writer
	err   io.Writer
	color bool
	width int
	level verbosity
	theme theme
}

func newPrinter(out, errw io.Writer, opts options) *printer {
	width := opts.width
	if width == 0 {
		width = fallbackWidth
	}
	return &printer{
		out:   out,
		err:   errw,
		color: opts.color,
		width: clampWidth(width),
		level: opts.level,
		theme: themeFor(opts.ascii),
	}
}

// paint wraps s in an escape sequence when colour is enabled.
func (p *printer) paint(code, s string) string {
	if !p.color {
		return s
	}
	return code + s + ansiReset
}

// glyph returns the marker and colour for a status.
func (p *printer) glyph(s status) (marker, color string) {
	switch s {
	case statusOK:
		return p.theme.success, ansiGreen
	case statusWarn:
		return p.theme.warning, ansiYellow
	case statusFail:
		return p.theme.failure, ansiRed
	default:
		return p.theme.info, ansiCyan
	}
}

// status prints one marked line, wrapping the message to the terminal width and
// indenting continuations under the text rather than under the glyph.
func (p *printer) status(s status, format string, a ...any) {
	marker, color := p.glyph(s)
	p.marked(p.paint(color, marker), fmt.Sprintf(format, a...))
}

func (p *printer) success(format string, a ...any) { p.status(statusOK, format, a...) }
func (p *printer) warn(format string, a ...any)    { p.status(statusWarn, format, a...) }
func (p *printer) fail(format string, a ...any)    { p.status(statusFail, format, a...) }
func (p *printer) info(format string, a ...any)    { p.status(statusInfo, format, a...) }

// bullet is an item in a plain list.
func (p *printer) bullet(format string, a ...any) {
	p.marked(p.paint(ansiDim, p.theme.bullet), fmt.Sprintf(format, a...))
}

// marked writes "<glyph> <text>", wrapping text into the remaining width.
func (p *printer) marked(glyph, text string) {
	indent := utf8.RuneCountInString(stripANSI(glyph)) + 1
	for i, line := range wrap(text, p.width-indent) {
		if i == 0 {
			fmt.Fprintf(p.err, "%s %s\n", glyph, line)
			continue
		}
		fmt.Fprintf(p.err, "%s%s\n", strings.Repeat(" ", indent), line)
	}
}

// note is an unadorned, dimmed line, indented to sit under a marked line.
func (p *printer) note(format string, a ...any) {
	for _, line := range wrap(fmt.Sprintf(format, a...), p.width-2) {
		fmt.Fprintf(p.err, "  %s\n", p.paint(ansiDim, line))
	}
}

// failure prints a multi-line error.
//
// Each line is wrapped on its own, and blank lines are preserved, so that the
// structure a release.Error builds — a summary, the offending paths, and a list
// of remedies — survives the journey to the terminal. Wrapping the message as
// one blob would run all of it into a single unreadable paragraph.
func (p *printer) failure(err error) {
	lines := strings.Split(err.Error(), "\n")

	glyph, color := p.glyph(statusFail)
	p.marked(p.paint(color, glyph), lines[0])

	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			p.blank()
			continue
		}
		// Preserve the leading indentation of a continuation line, such as the
		// two spaces before each offending path.
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		for _, wrapped := range wrap(line, p.width-len(indent)-2) {
			fmt.Fprintf(p.err, "  %s%s\n", indent, wrapped)
		}
	}
}

// plain writes a line with no decoration at all.
func (p *printer) plain(format string, a ...any) {
	fmt.Fprintf(p.err, "%s\n", fmt.Sprintf(format, a...))
}

// blank prints a single empty line. Commands rely on it rather than embedding
// newlines in messages, so that spacing stays consistent between them.
func (p *printer) blank() { fmt.Fprintln(p.err) }

// section introduces a block of the report: a blank line, a bold heading, and
// another blank line. Every screen is built from these, which is what makes the
// output scannable.
func (p *printer) section(title string) {
	p.blank()
	fmt.Fprintf(p.err, "%s\n\n", p.paint(ansiBold, title))
}

// rule draws a horizontal divider across the terminal.
func (p *printer) rule() {
	fmt.Fprintln(p.err, p.paint(ansiDim, strings.Repeat(p.theme.rule, p.width)))
}

// verbosef narrates a phase. It is silent unless --verbose was passed.
func (p *printer) verbosef(format string, a ...any) {
	if p.level >= levelVerbose {
		p.note(format, a...)
	}
}

// debugf reports internal detail. It is silent unless --debug was passed, so
// implementation details never leak into a normal run.
func (p *printer) debugf(format string, a ...any) {
	if p.level >= levelDebug {
		fmt.Fprintf(p.err, "%s %s\n", p.paint(ansiDim, "debug"), fmt.Sprintf(format, a...))
	}
}

// row is one line of a table: a label and its value.
type row struct {
	label string
	value string
	// bold highlights the value, for the line that matters most.
	bold bool
	// status, when set, prefixes the value with a glyph.
	status *status
}

// table prints label/value pairs in two aligned columns.
//
// The column width is derived from the longest label, so the layout adapts to
// whatever labels a caller passes, and never depends on the width of a value.
// A value too wide for the terminal is truncated rather than wrapped, because a
// wrapped value would break the column.
func (p *printer) table(rows []row) {
	labelWidth := 0
	for _, r := range rows {
		// Count runes, not bytes, so a non-ASCII label still aligns.
		if n := utf8.RuneCountInString(r.label); n > labelWidth {
			labelWidth = n
		}
	}
	labelWidth += labelGap

	for _, r := range rows {
		value := truncate(r.value, p.width-labelWidth, p.theme.ellipsis)
		if r.bold {
			value = p.paint(ansiBold, value)
		}
		if r.status != nil {
			marker, color := p.glyph(*r.status)
			value = p.paint(color, marker) + " " + value
		}
		pad := labelWidth - utf8.RuneCountInString(r.label)
		fmt.Fprintf(p.err, "%s%s%s\n", p.paint(ansiDim, r.label), strings.Repeat(" ", pad), value)
	}
}

// dryRunBanner announces, unmissably, that nothing will be changed.
func (p *printer) dryRunBanner() {
	p.rule()
	p.blank()
	p.plain("%s", p.paint(ansiYellow, p.paint(ansiBold, "DRY RUN")))
	p.blank()
	p.note("Nothing will be modified.")
	p.note("Nothing will be pushed.")
	p.note("Nothing will be published.")
	p.blank()
	p.rule()
}

// stars renders a rating out of five, filled then hollow.
func (p *printer) stars(filled int) string {
	filled = max(0, min(5, filled))
	return strings.Repeat(p.theme.star, filled) + strings.Repeat(p.theme.starDim, 5-filled)
}

// truncate shortens s to at most width columns, marking the cut with an
// ellipsis. A width too small to be useful disables truncation, because a
// mangled value is worse than an overlong one.
func truncate(s string, width int, ellipsis string) string {
	if width < 8 || utf8.RuneCountInString(s) <= width {
		return s
	}
	keep := width - utf8.RuneCountInString(ellipsis)
	runes := []rune(s)
	return string(runes[:keep]) + ellipsis
}

// wrap breaks text into lines of at most width columns, splitting on spaces. A
// word longer than the width is left alone rather than broken, because URLs and
// commands must stay copyable.
func wrap(text string, width int) []string {
	if width < 8 {
		return []string{text}
	}

	var lines []string
	var line strings.Builder

	for word := range strings.FieldsSeq(text) {
		switch {
		case line.Len() == 0:
			line.WriteString(word)
		case utf8.RuneCountInString(line.String())+1+utf8.RuneCountInString(word) <= width:
			line.WriteByte(' ')
			line.WriteString(word)
		default:
			lines = append(lines, line.String())
			line.Reset()
			line.WriteString(word)
		}
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// stripANSI removes escape sequences, so that a painted glyph can still be
// measured in columns.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // the 'm' itself
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
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
