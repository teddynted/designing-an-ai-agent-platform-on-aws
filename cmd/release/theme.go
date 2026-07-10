package main

import (
	"os"
	"strings"
)

// verbosity selects how much diagnostic detail reaches the terminal. The
// user-facing report is identical at every level; only the logging around it
// changes.
type verbosity int

const (
	// levelNormal prints the report and nothing else.
	levelNormal verbosity = iota
	// levelVerbose narrates the phases as they run.
	levelVerbose
	// levelDebug adds internal detail: resolved configuration, tag counts,
	// timings of individual steps.
	levelDebug
)

// theme is the set of glyphs used to render status. Two are provided: one for
// terminals that can show Unicode, and an ASCII fallback for those that cannot.
//
// Every glyph is a single column wide, so a column of them stays aligned.
type theme struct {
	success  string
	warning  string
	failure  string
	info     string
	bullet   string
	star     string
	starDim  string
	rule     string
	ellipsis string
	// dash separates a check's name from its detail.
	dash string
}

func unicodeTheme() theme {
	return theme{
		success:  "✓",
		warning:  "⚠",
		failure:  "✗",
		info:     "ℹ",
		bullet:   "•",
		star:     "★",
		starDim:  "☆",
		rule:     "─",
		ellipsis: "…",
		dash:     "—",
	}
}

// asciiTheme uses only characters in the 7-bit range, so nothing here may be a
// typographic dash, ellipsis, or box-drawing character.
func asciiTheme() theme {
	return theme{
		success:  "+",
		warning:  "!",
		failure:  "x",
		info:     "i",
		bullet:   "-",
		star:     "*",
		starDim:  ".",
		rule:     "-",
		ellipsis: "...",
		dash:     "-",
	}
}

// useUnicode reports whether the terminal can be trusted with the Unicode
// glyphs. The --ascii flag always wins; otherwise the locale decides, because a
// terminal that is not in a UTF-8 locale will render them as mojibake.
func useUnicode(forceASCII bool) bool {
	if forceASCII || os.Getenv("RELEASE_ASCII") != "" {
		return false
	}
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if value := os.Getenv(name); value != "" {
			return strings.Contains(strings.ToUpper(value), "UTF-8") ||
				strings.Contains(strings.ToUpper(value), "UTF8")
		}
	}
	// No locale at all: assume a modern terminal, which is the common case in
	// CI, where LANG is often unset but the log viewer handles UTF-8 happily.
	return true
}

func themeFor(forceASCII bool) theme {
	if useUnicode(forceASCII) {
		return unicodeTheme()
	}
	return asciiTheme()
}
