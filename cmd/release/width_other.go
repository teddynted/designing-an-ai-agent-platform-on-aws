//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package main

import "os"

// terminalWidth has no portable implementation on this platform, so the caller
// falls back to COLUMNS or the default width.
func terminalWidth(*os.File) (int, bool) { return 0, false }
