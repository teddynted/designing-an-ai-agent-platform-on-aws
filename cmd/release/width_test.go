package main

import (
	"os"
	"testing"
)

func TestClampWidth(t *testing.T) {
	for in, want := range map[int]int{
		0:    minWidth,
		10:   minWidth,
		39:   minWidth,
		40:   40,
		76:   76,
		100:  100,
		500:  maxWidth,
		-100: minWidth,
	} {
		if got := clampWidth(in); got != want {
			t.Errorf("clampWidth(%d) = %d, want %d", in, got, want)
		}
	}
}

// A user who exports COLUMNS is overriding on purpose, so it outranks the
// terminal.
func TestDetectWidthPrefersColumns(t *testing.T) {
	t.Setenv("COLUMNS", "72")
	if got := detectWidth(os.Stdout); got != 72 {
		t.Errorf("detectWidth() = %d, want 72", got)
	}
}

func TestDetectWidthClampsColumns(t *testing.T) {
	t.Setenv("COLUMNS", "5000")
	if got := detectWidth(os.Stdout); got != maxWidth {
		t.Errorf("detectWidth() = %d, want %d", got, maxWidth)
	}
}

func TestDetectWidthIgnoresGarbageColumns(t *testing.T) {
	t.Setenv("COLUMNS", "wide please")
	// Falls through to the terminal, and then to the fallback. Under `go test`
	// stdout is not a terminal, so the fallback wins.
	if got := detectWidth(os.Stdout); got != fallbackWidth {
		t.Errorf("detectWidth() = %d, want the fallback %d", got, fallbackWidth)
	}
}

// A redirected stream has no width, and neither does a nil file.
func TestDetectWidthFallsBack(t *testing.T) {
	t.Setenv("COLUMNS", "")
	if got := detectWidth(nil); got != fallbackWidth {
		t.Errorf("detectWidth(nil) = %d, want %d", got, fallbackWidth)
	}
}
