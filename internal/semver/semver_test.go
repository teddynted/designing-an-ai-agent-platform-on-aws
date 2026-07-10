package semver

import (
	"errors"
	"testing"
)

func TestParseValid(t *testing.T) {
	tests := []struct {
		in   string
		want Version
	}{
		{"0.0.0", Version{}},
		{"1.2.3", Version{Major: 1, Minor: 2, Patch: 3}},
		{"v1.2.3", Version{Major: 1, Minor: 2, Patch: 3}},
		{" v1.2.3 ", Version{Major: 1, Minor: 2, Patch: 3}},
		{"10.20.30", Version{Major: 10, Minor: 20, Patch: 30}},
		{"1.2.3-rc.1", Version{Major: 1, Minor: 2, Patch: 3, Prerelease: "rc.1"}},
		{"1.0.0-alpha", Version{Major: 1, Prerelease: "alpha"}},
		{"1.0.0-0.3.7", Version{Major: 1, Prerelease: "0.3.7"}},
		{"1.0.0-x-y-z.--", Version{Major: 1, Prerelease: "x-y-z.--"}},
		{"1.0.0+20130313144700", Version{Major: 1, Build: "20130313144700"}},
		{"1.0.0-beta+exp.sha.5114f85", Version{Major: 1, Prerelease: "beta", Build: "exp.sha.5114f85"}},
		// Build metadata may contain a hyphen; it must not be read as a pre-release.
		{"1.0.0+21AF26D3---117B344092BD", Version{Major: 1, Build: "21AF26D3---117B344092BD"}},
		// Leading zeros are legal in build metadata only.
		{"1.0.0+0.007", Version{Major: 1, Build: "0.007"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	invalid := []string{
		"",
		"v",
		"1",
		"1.2",
		"1.2.3.4",
		"1.2.x",
		"-1.2.3",
		"01.2.3",      // leading zero in major
		"1.02.3",      // leading zero in minor
		"1.2.03",      // leading zero in patch
		"1.2.3-",      // empty pre-release
		"1.2.3-01",    // leading zero in numeric pre-release identifier
		"1.2.3-rc..1", // empty identifier
		"1.2.3-rc.1+", // empty build metadata
		"1.2.3-rc_1",  // invalid character
		"1.2.3+build_1",
		"99999999999999999999.0.0", // out of uint64 range
	}
	for _, in := range invalid {
		t.Run(in, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Fatalf("Parse(%q) succeeded, want error", in)
			} else if !errors.Is(err, ErrInvalid) {
				t.Errorf("Parse(%q) error %v does not wrap ErrInvalid", in, err)
			}
		})
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, in := range []string{"0.0.0", "1.2.3", "1.2.3-rc.1", "1.0.0-beta+exp.sha.5114f85", "1.0.0+build.1"} {
		if got := MustParse(in).String(); got != in {
			t.Errorf("MustParse(%q).String() = %q", in, got)
		}
	}
}

func TestTag(t *testing.T) {
	if got := MustParse("1.4.0").Tag("v"); got != "v1.4.0" {
		t.Errorf("Tag(\"v\") = %q, want v1.4.0", got)
	}
	if got := MustParse("1.4.0").Tag(""); got != "1.4.0" {
		t.Errorf("Tag(\"\") = %q, want 1.4.0", got)
	}
}

// TestPrecedenceChain walks the exact ordering given in clause 11 of the
// specification.
func TestPrecedenceChain(t *testing.T) {
	chain := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
		"1.1.0",
		"2.0.0",
	}
	for i := 0; i < len(chain)-1; i++ {
		lo, hi := MustParse(chain[i]), MustParse(chain[i+1])
		if Compare(lo, hi) != -1 {
			t.Errorf("Compare(%s, %s) = %d, want -1", lo, hi, Compare(lo, hi))
		}
		if Compare(hi, lo) != 1 {
			t.Errorf("Compare(%s, %s) = %d, want 1", hi, lo, Compare(hi, lo))
		}
		if Compare(lo, lo) != 0 {
			t.Errorf("Compare(%s, %s) != 0", lo, lo)
		}
	}
}

func TestCompareIgnoresBuildMetadata(t *testing.T) {
	a, b := MustParse("1.0.0+build.1"), MustParse("1.0.0+build.2")
	if Compare(a, b) != 0 {
		t.Errorf("Compare(%s, %s) = %d, want 0", a, b, Compare(a, b))
	}
	if !a.Equal(b) {
		t.Errorf("%s should be Equal to %s", a, b)
	}
}

func TestCompareLongNumericIdentifiers(t *testing.T) {
	// Numeric identifiers may exceed uint64; comparison must not overflow.
	lo := MustParse("1.0.0-99999999999999999999998")
	hi := MustParse("1.0.0-99999999999999999999999")
	if Compare(lo, hi) != -1 {
		t.Errorf("Compare(%s, %s) = %d, want -1", lo, hi, Compare(lo, hi))
	}
}

func TestSortAndLatest(t *testing.T) {
	vs := []Version{
		MustParse("1.0.0"),
		MustParse("0.9.9"),
		MustParse("2.0.0-rc.1"),
		MustParse("2.0.0"),
		MustParse("1.10.0"),
	}
	Sort(vs)
	want := []string{"0.9.9", "1.0.0", "1.10.0", "2.0.0-rc.1", "2.0.0"}
	for i, w := range want {
		if vs[i].String() != w {
			t.Errorf("Sort()[%d] = %s, want %s", i, vs[i], w)
		}
	}

	latest, ok := Latest(vs)
	if !ok || latest.String() != "2.0.0" {
		t.Errorf("Latest() = %s, %v; want 2.0.0, true", latest, ok)
	}
	if _, ok := Latest(nil); ok {
		t.Error("Latest(nil) should report ok=false")
	}
}

func TestBump(t *testing.T) {
	tests := []struct {
		from string
		bump Bump
		want string
	}{
		// A repository with no tags starts from the zero value.
		{"0.0.0", BumpPatch, "0.0.1"},
		{"0.0.0", BumpMinor, "0.1.0"},
		{"0.0.0", BumpMajor, "1.0.0"},

		{"1.2.3", BumpPatch, "1.2.4"},
		{"1.2.3", BumpMinor, "1.3.0"},
		{"1.2.3", BumpMajor, "2.0.0"},

		// Build metadata never survives a bump.
		{"1.2.3+build.7", BumpPatch, "1.2.4"},

		// A pre-release graduates to the release it was a candidate for.
		{"1.2.3-rc.1", BumpPatch, "1.2.3"},
		{"1.3.0-rc.1", BumpMinor, "1.3.0"},
		{"2.0.0-rc.1", BumpMajor, "2.0.0"},

		// ...but only when the pre-release actually targets that component.
		{"1.2.3-rc.1", BumpMinor, "1.3.0"},
		{"1.2.3-rc.1", BumpMajor, "2.0.0"},
		{"1.3.0-rc.1", BumpMajor, "2.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.from+"/"+tt.bump.String(), func(t *testing.T) {
			if got := MustParse(tt.from).Bump(tt.bump); got.String() != tt.want {
				t.Errorf("MustParse(%q).Bump(%s) = %s, want %s", tt.from, tt.bump, got, tt.want)
			}
		})
	}
}

func TestBumpPrerelease(t *testing.T) {
	tests := []struct {
		from string
		bump Bump
		id   string
		want string
	}{
		{"1.2.3", BumpPatch, "rc", "1.2.4-rc.0"},
		{"1.2.3", BumpMinor, "rc", "1.3.0-rc.0"},
		{"1.2.3", BumpMajor, "rc", "2.0.0-rc.0"},

		// Advancing within an existing series.
		{"1.3.0-rc.0", BumpMinor, "rc", "1.3.0-rc.1"},
		{"1.3.0-rc.9", BumpMinor, "rc", "1.3.0-rc.10"},
		{"1.3.0-rc", BumpMinor, "rc", "1.3.0-rc.0"},

		// Switching series keeps the core version.
		{"1.3.0-beta.2", BumpMinor, "rc", "1.3.0-rc.0"},

		// A different bump level starts a new core and a new series.
		{"1.3.0-rc.1", BumpMajor, "rc", "2.0.0-rc.0"},
	}
	for _, tt := range tests {
		t.Run(tt.from+"/"+tt.bump.String()+"/"+tt.id, func(t *testing.T) {
			got := MustParse(tt.from).BumpPrerelease(tt.bump, tt.id)
			if got.String() != tt.want {
				t.Errorf("MustParse(%q).BumpPrerelease(%s, %q) = %s, want %s", tt.from, tt.bump, tt.id, got, tt.want)
			}
		})
	}
}

// A bumped version must always sort after the version it came from, otherwise
// the tool could produce a tag that Git and package managers consider older.
func TestBumpAlwaysIncreasesPrecedence(t *testing.T) {
	froms := []string{"0.0.0", "1.2.3", "1.2.3-rc.1", "1.3.0-rc.1", "2.0.0-rc.1", "1.0.0+build.1"}
	for _, from := range froms {
		v := MustParse(from)
		for _, b := range []Bump{BumpPatch, BumpMinor, BumpMajor} {
			if next := v.Bump(b); !v.Less(next) {
				t.Errorf("%s.Bump(%s) = %s, which does not increase precedence", v, b, next)
			}
			if next := v.BumpPrerelease(b, "rc"); !v.Less(next) {
				t.Errorf("%s.BumpPrerelease(%s, rc) = %s, which does not increase precedence", v, b, next)
			}
		}
	}
}

func TestParseBump(t *testing.T) {
	for in, want := range map[string]Bump{
		"patch": BumpPatch, "minor": BumpMinor, "major": BumpMajor,
		"PATCH": BumpPatch, " minor ": BumpMinor,
	} {
		got, err := ParseBump(in)
		if err != nil {
			t.Fatalf("ParseBump(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseBump(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseBump("nope"); err == nil {
		t.Error("ParseBump(\"nope\") should fail")
	}
}
