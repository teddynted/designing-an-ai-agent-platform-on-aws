package main

import (
	"os"
	"strconv"
)

// Terminal widths outside this range are ignored: below the minimum the report
// cannot be laid out at all, and above the maximum long lines become hard to
// read. 80 is the fallback when the width cannot be discovered.
const (
	minWidth      = 40
	maxWidth      = 100
	fallbackWidth = 80
)

// detectWidth returns the usable width of the terminal attached to f.
//
// COLUMNS wins when set, because a user who exports it is overriding on
// purpose. Otherwise the width comes from the terminal itself, and failing that
// from the fallback, so output is always laid out to something sensible even
// when redirected to a file.
func detectWidth(f *os.File) int {
	if columns := os.Getenv("COLUMNS"); columns != "" {
		if n, err := strconv.Atoi(columns); err == nil {
			return clampWidth(n)
		}
	}
	if f != nil {
		if n, ok := terminalWidth(f); ok {
			return clampWidth(n)
		}
	}
	return fallbackWidth
}

func clampWidth(n int) int {
	switch {
	case n < minWidth:
		return minWidth
	case n > maxWidth:
		return maxWidth
	default:
		return n
	}
}
