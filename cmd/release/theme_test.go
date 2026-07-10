package main

import (
	"testing"
	"unicode/utf8"
)

// The ASCII theme exists so that a terminal without UTF-8 renders something
// readable. A single stray Unicode character defeats the point, so every glyph
// is checked, not just the obvious ones.
func TestASCIIThemeIsASCII(t *testing.T) {
	th := asciiTheme()
	glyphs := map[string]string{
		"success":  th.success,
		"warning":  th.warning,
		"failure":  th.failure,
		"info":     th.info,
		"bullet":   th.bullet,
		"star":     th.star,
		"starDim":  th.starDim,
		"rule":     th.rule,
		"ellipsis": th.ellipsis,
		"dash":     th.dash,
	}
	for name, glyph := range glyphs {
		if glyph == "" {
			t.Errorf("the ASCII theme has no %s glyph", name)
		}
		for _, r := range glyph {
			if r > utf8.RuneSelf {
				t.Errorf("ASCII theme %s = %q, which is not ASCII", name, glyph)
			}
		}
	}
}

// Every status glyph must be one column wide, or a column of them will not line
// up. The ellipsis and the dash are exempt: they sit inside text.
func TestGlyphsAreOneColumnWide(t *testing.T) {
	for name, th := range map[string]theme{"unicode": unicodeTheme(), "ascii": asciiTheme()} {
		for _, glyph := range []string{th.success, th.warning, th.failure, th.info, th.bullet, th.star, th.starDim, th.rule} {
			if n := utf8.RuneCountInString(glyph); n != 1 {
				t.Errorf("%s theme: glyph %q is %d runes wide, want 1", name, glyph, n)
			}
		}
	}
}

func TestUseUnicode(t *testing.T) {
	t.Run("--ascii always wins", func(t *testing.T) {
		t.Setenv("LANG", "en_GB.UTF-8")
		if useUnicode(true) {
			t.Error("--ascii should force the ASCII theme")
		}
	})

	t.Run("a UTF-8 locale enables Unicode", func(t *testing.T) {
		t.Setenv("LC_ALL", "")
		t.Setenv("LC_CTYPE", "")
		t.Setenv("LANG", "en_GB.UTF-8")
		if !useUnicode(false) {
			t.Error("a UTF-8 locale should enable Unicode")
		}
	})

	t.Run("a non-UTF-8 locale falls back", func(t *testing.T) {
		t.Setenv("LC_ALL", "C")
		if useUnicode(false) {
			t.Error("the C locale should fall back to ASCII")
		}
	})

	t.Run("LC_ALL outranks LANG", func(t *testing.T) {
		t.Setenv("LC_ALL", "C")
		t.Setenv("LANG", "en_GB.UTF-8")
		if useUnicode(false) {
			t.Error("LC_ALL should take precedence over LANG")
		}
	})

	t.Run("RELEASE_ASCII forces ASCII", func(t *testing.T) {
		t.Setenv("LANG", "en_GB.UTF-8")
		t.Setenv("RELEASE_ASCII", "1")
		if useUnicode(false) {
			t.Error("RELEASE_ASCII should force the ASCII theme")
		}
	})

	// CI often has no locale at all, but its log viewers handle UTF-8.
	t.Run("no locale assumes Unicode", func(t *testing.T) {
		t.Setenv("RELEASE_ASCII", "")
		t.Setenv("LC_ALL", "")
		t.Setenv("LC_CTYPE", "")
		t.Setenv("LANG", "")
		if !useUnicode(false) {
			t.Error("an unset locale should assume a modern terminal")
		}
	})
}

func TestThemeFor(t *testing.T) {
	if themeFor(true).success != asciiTheme().success {
		t.Error("themeFor(true) should return the ASCII theme")
	}
}
