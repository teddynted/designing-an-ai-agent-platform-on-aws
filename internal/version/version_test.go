package version_test

import (
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want version.Version
	}{
		{"bare", "1.2.3", version.Version{Major: 1, Minor: 2, Patch: 3}},
		{"v prefix", "v1.2.3", version.Version{Major: 1, Minor: 2, Patch: 3}},
		{"surrounding space", "  v1.2.3\n", version.Version{Major: 1, Minor: 2, Patch: 3}},
		{"zeroes", "0.0.0", version.Version{}},
		{"prerelease", "1.2.3-rc.1", version.Version{Major: 1, Minor: 2, Patch: 3, Prerelease: []string{"rc", "1"}}},
		{"build", "1.2.3+build.7", version.Version{Major: 1, Minor: 2, Patch: 3, Build: []string{"build", "7"}}},
		{
			"prerelease and build",
			"1.2.3-rc.1+build.7",
			version.Version{Major: 1, Minor: 2, Patch: 3, Prerelease: []string{"rc", "1"}, Build: []string{"build", "7"}},
		},
		{"hyphen in prerelease", "1.0.0-x-y-z.0", version.Version{Major: 1, Prerelease: []string{"x-y-z", "0"}}},
		{"large minor", "1.10.0", version.Version{Major: 1, Minor: 10}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := version.Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tc.in, err)
			}
			if got.String() != tc.want.String() {
				t.Errorf("Parse(%q) = %s, want %s", tc.in, got, tc.want)
			}
			if !got.Equal(tc.want) {
				t.Errorf("Parse(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	// The near-misses. Each of these has bitten someone.
	for _, in := range []string{
		"",
		"1.2",
		"1",
		"01.2.3",      // leading zero in major
		"1.02.3",      // leading zero in minor
		"1.2.03",      // leading zero in patch
		"1.2.3-",      // empty pre-release
		"1.2.3+",      // empty build
		"1.2.3-rc.01", // leading zero in a numeric pre-release identifier
		"1.2.3.4",
		"v",
		"vv1.2.3",
		"1.2.3-rc..1", // empty identifier
		"latest",
		"-1.2.3",
	} {
		t.Run(in, func(t *testing.T) {
			if _, err := version.Parse(in); err == nil {
				t.Errorf("Parse(%q) succeeded; want an error", in)
			}
			if version.IsValid(in) {
				t.Errorf("IsValid(%q) = true; want false", in)
			}
		})
	}
}

func TestString(t *testing.T) {
	for _, in := range []string{"1.2.3", "1.2.3-rc.1", "1.2.3+build.7", "1.2.3-rc.1+build.7", "0.0.0"} {
		t.Run(in, func(t *testing.T) {
			v := version.MustParse(in)
			if got := v.String(); got != in {
				t.Errorf("round trip of %q gave %q", in, got)
			}
			if got, want := v.Tag(), "v"+in; got != want {
				t.Errorf("Tag() = %q, want %q", got, want)
			}
		})
	}
}

func TestBump(t *testing.T) {
	tests := []struct {
		name string
		in   string
		part version.Part
		want string
	}{
		{"major", "1.2.3", version.Major, "2.0.0"},
		{"minor", "1.2.3", version.Minor, "1.3.0"},
		{"patch", "1.2.3", version.Patch, "1.2.4"},
		{"major from zero", "0.1.0", version.Major, "1.0.0"},
		{"patch rolls into nine", "1.2.9", version.Patch, "1.2.10"},

		// A bump drops pre-release and build metadata rather than carrying it.
		{"drops prerelease", "1.2.3-rc.1", version.Patch, "1.2.4"},
		{"drops build", "1.2.3+build.7", version.Minor, "1.3.0"},
		{"drops both", "1.2.3-rc.1+build.7", version.Major, "2.0.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := version.MustParse(tc.in).Bump(tc.part)
			if err != nil {
				t.Fatalf("Bump(%q) returned error: %v", tc.part, err)
			}
			if got.String() != tc.want {
				t.Errorf("%s bumped by %s = %s, want %s", tc.in, tc.part, got, tc.want)
			}
		})
	}
}

func TestBumpRejectsUnknownPart(t *testing.T) {
	if _, err := version.MustParse("1.0.0").Bump(version.Part("epoch")); err == nil {
		t.Error("Bump(\"epoch\") succeeded; want an error")
	}
}

func TestParsePart(t *testing.T) {
	tests := []struct {
		in      string
		want    version.Part
		wantErr bool
	}{
		{"major", version.Major, false},
		{"minor", version.Minor, false},
		{"patch", version.Patch, false},
		{"PATCH", version.Patch, false},
		{"  Minor  ", version.Minor, false},
		{"epoch", "", true},
		{"", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := version.ParsePart(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ParsePart(%q) succeeded; want an error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePart(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParsePart(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPrecedence walks the exact ordering the specification gives in §11.
func TestPrecedence(t *testing.T) {
	// Spec §11.2 and §11.4: this chain is strictly ascending.
	ascending := []string{
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
		"2.1.0",
		"2.1.1",
	}
	for i := 0; i < len(ascending)-1; i++ {
		lower, higher := version.MustParse(ascending[i]), version.MustParse(ascending[i+1])
		if !lower.Less(higher) {
			t.Errorf("%s should sort below %s", lower, higher)
		}
		if higher.Less(lower) {
			t.Errorf("%s should not sort below %s", higher, lower)
		}
		if lower.Equal(higher) {
			t.Errorf("%s should not equal %s", lower, higher)
		}
	}
}

func TestPrecedenceRules(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		want  int
	}{
		// §11.3: a pre-release sorts below its own release.
		{"prerelease below release", "1.0.0-alpha", "1.0.0", -1},
		{"release above prerelease", "1.0.0", "1.0.0-alpha", 1},

		// §11.4.1: numeric identifiers compare numerically, not lexically.
		{"numeric identifiers compare numerically", "1.0.0-beta.2", "1.0.0-beta.11", -1},

		// §11.4.3: numeric sorts below alphanumeric.
		{"numeric below alphanumeric", "1.0.0-alpha.1", "1.0.0-alpha.beta", -1},

		// §11.4.4: a smaller set of identifiers sorts below a larger one.
		{"fewer identifiers sort below", "1.0.0-alpha", "1.0.0-alpha.1", -1},

		// §10: build metadata is excluded from precedence entirely.
		{"build ignored", "1.0.0+build.1", "1.0.0+build.2", 0},
		{"build ignored against none", "1.0.0", "1.0.0+build.99", 0},
		{"build ignored with prerelease", "1.0.0-rc.1+a", "1.0.0-rc.1+z", 0},

		{"identical", "1.2.3", "1.2.3", 0},
		{"major dominates", "2.0.0", "1.99.99", 1},
		{"minor dominates patch", "1.2.0", "1.1.99", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			left, right := version.MustParse(tc.left), version.MustParse(tc.right)
			if got := left.Compare(right); got != tc.want {
				t.Errorf("Compare(%s, %s) = %d, want %d", tc.left, tc.right, got, tc.want)
			}
			// Compare must be antisymmetric.
			if got, want := right.Compare(left), -tc.want; got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d (antisymmetry)", tc.right, tc.left, got, want)
			}
			if got := left.Equal(right); got != (tc.want == 0) {
				t.Errorf("Equal(%s, %s) = %v, want %v", tc.left, tc.right, got, tc.want == 0)
			}
		})
	}
}

// Build metadata is ignored for precedence, so two versions differing only in
// build are Equal even though == would report them as different structs.
func TestBuildMetadataIgnoredButPreserved(t *testing.T) {
	a, b := version.MustParse("1.0.0+build.1"), version.MustParse("1.0.0+build.2")
	if !a.Equal(b) {
		t.Error("versions differing only in build metadata should have equal precedence")
	}
	if a.String() == b.String() {
		t.Error("build metadata should still round-trip through String")
	}
}

func TestCompareStrings(t *testing.T) {
	got, err := version.CompareStrings("v1.0.0", "1.0.1")
	if err != nil {
		t.Fatalf("CompareStrings returned error: %v", err)
	}
	if got != -1 {
		t.Errorf("CompareStrings(v1.0.0, 1.0.1) = %d, want -1", got)
	}
	if _, err := version.CompareStrings("nonsense", "1.0.0"); err == nil {
		t.Error("CompareStrings with an invalid left version should fail")
	}
	if _, err := version.CompareStrings("1.0.0", "nonsense"); err == nil {
		t.Error("CompareStrings with an invalid right version should fail")
	}
}

func TestSort(t *testing.T) {
	versions := []version.Version{
		version.MustParse("1.0.0"),
		version.MustParse("0.1.0"),
		version.MustParse("1.0.0-rc.1"),
		version.MustParse("2.0.0"),
		version.MustParse("0.9.9"),
	}
	version.Sort(versions)

	want := []string{"0.1.0", "0.9.9", "1.0.0-rc.1", "1.0.0", "2.0.0"}
	for i, w := range want {
		if versions[i].String() != w {
			t.Errorf("Sort()[%d] = %s, want %s", i, versions[i], w)
		}
	}
}

func TestInterrogation(t *testing.T) {
	tests := []struct {
		in                 string
		prerelease         bool
		initialDevelopment bool
	}{
		{"1.0.0", false, false},
		{"0.1.0", false, true},
		{"1.0.0-rc.1", true, false},
		{"0.1.0-alpha", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			v := version.MustParse(tc.in)
			if got := v.IsPrerelease(); got != tc.prerelease {
				t.Errorf("IsPrerelease() = %v, want %v", got, tc.prerelease)
			}
			if got := v.IsInitialDevelopment(); got != tc.initialDevelopment {
				t.Errorf("IsInitialDevelopment() = %v, want %v", got, tc.initialDevelopment)
			}
		})
	}
}

func TestMustParsePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustParse on an invalid version should panic")
		}
	}()
	version.MustParse("not-a-version")
}
