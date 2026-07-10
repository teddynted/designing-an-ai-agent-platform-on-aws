package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ANSI escapes, used only when the output is an interactive terminal.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

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

// step reports work about to happen.
func (p *printer) step(format string, a ...any) {
	fmt.Fprintf(p.err, "%s %s\n", p.paint(ansiCyan, "→"), fmt.Sprintf(format, a...))
}

// ok reports work that succeeded.
func (p *printer) ok(format string, a ...any) {
	fmt.Fprintf(p.err, "%s %s\n", p.paint(ansiGreen, "✓"), fmt.Sprintf(format, a...))
}

// warn reports something the user should notice but which is not a failure.
func (p *printer) warn(format string, a ...any) {
	fmt.Fprintf(p.err, "%s %s\n", p.paint(ansiYellow, "!"), fmt.Sprintf(format, a...))
}

// info is an unadorned note.
func (p *printer) info(format string, a ...any) {
	fmt.Fprintf(p.err, "  %s\n", p.paint(ansiDim, fmt.Sprintf(format, a...)))
}

func (p *printer) blank() { fmt.Fprintln(p.err) }

// field prints one aligned label/value pair of a summary block.
func (p *printer) field(label, format string, a ...any) {
	fmt.Fprintf(p.err, "  %-12s %s\n", p.paint(ansiDim, label), fmt.Sprintf(format, a...))
}

// heading prints a bold line.
func (p *printer) heading(format string, a ...any) {
	fmt.Fprintf(p.err, "%s\n", p.paint(ansiBold, fmt.Sprintf(format, a...)))
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
