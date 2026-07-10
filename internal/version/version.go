// Package version implements Semantic Versioning 2.0.0 as a value type.
//
// Two rules from the specification are easy to get wrong, and both are
// implemented deliberately here rather than falling out of struct equality:
//
//   - Build metadata is ignored when determining precedence (spec §10).
//     1.0.0+build.1 and 1.0.0+build.2 have equal precedence. Equal reports them
//     as equal, and Compare returns 0. This is the spec's meaning of "same
//     version", not an oversight. It is also why Version must not be compared
//     with ==, which would consider the build fields.
//
//   - A pre-release sorts below its own release (spec §11.3).
//     1.0.0-alpha < 1.0.0. Encoded by treating an absent pre-release as sorting
//     above every pre-release identifier.
//
// Within a pre-release, numeric identifiers compare numerically, alphanumeric
// identifiers compare lexically in ASCII order, numeric always sorts below
// alphanumeric, and a smaller set of identifiers sorts below a larger set when
// all preceding identifiers are equal (spec §11.4).
package version

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The regexp published in the SemVer 2.0.0 spec FAQ, with an optional leading
// "v" so it accepts both the tag form (v1.2.3) and the bare form (1.2.3).
var semver = regexp.MustCompile(
	`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
		`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`,
)

var numeric = regexp.MustCompile(`^(0|[1-9]\d*)$`)

// Part names the component of a version to increment.
type Part string

const (
	Major Part = "major"
	Minor Part = "minor"
	Patch Part = "patch"
)

// ParsePart validates a bump level supplied as text, such as a CLI argument.
func ParsePart(text string) (Part, error) {
	switch p := Part(strings.ToLower(strings.TrimSpace(text))); p {
	case Major, Minor, Patch:
		return p, nil
	default:
		return "", fmt.Errorf("cannot bump %q: expected %q, %q, or %q", text, Major, Minor, Patch)
	}
}

// Version is a parsed SemVer 2.0.0 version.
//
// The zero Version (0.0.0) is valid but rarely meaningful. Construct with Parse.
// Compare with Equal, Less, or Compare — never with ==.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease []string
	Build      []string
}

// Parse reads 1.2.3, v1.2.3, 1.2.3-rc.1, or 1.2.3-rc.1+build.7.
//
// It rejects the common near-misses: 1.2, 01.2.3 (leading zero), and 1.2.3-
// (empty pre-release).
func Parse(text string) (Version, error) {
	match := semver.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return Version{}, fmt.Errorf("not a valid SemVer 2.0.0 version: %q", text)
	}
	// The regexp has already constrained these to digit runs without leading
	// zeroes, so the only way Atoi fails is integer overflow.
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return Version{}, fmt.Errorf("major version out of range in %q", text)
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return Version{}, fmt.Errorf("minor version out of range in %q", text)
	}
	patch, err := strconv.Atoi(match[3])
	if err != nil {
		return Version{}, fmt.Errorf("patch version out of range in %q", text)
	}
	return Version{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		Prerelease: split(match[4]),
		Build:      split(match[5]),
	}, nil
}

// MustParse is Parse for versions known at compile time. It panics on failure.
func MustParse(text string) Version {
	v, err := Parse(text)
	if err != nil {
		panic(err)
	}
	return v
}

// IsValid reports whether text would Parse.
func IsValid(text string) bool {
	_, err := Parse(text)
	return err == nil
}

func split(field string) []string {
	if field == "" {
		return nil
	}
	return strings.Split(field, ".")
}

// Each bump drops any pre-release and build metadata: 1.2.3-rc.1 bumped at the
// patch level is 1.2.4, not 1.2.4-rc.1. Carrying a pre-release across a bump
// would assert something the caller did not say.

// BumpMajor returns the next breaking version: 1.2.3 -> 2.0.0.
func (v Version) BumpMajor() Version { return Version{Major: v.Major + 1} }

// BumpMinor returns the next feature version: 1.2.3 -> 1.3.0.
func (v Version) BumpMinor() Version { return Version{Major: v.Major, Minor: v.Minor + 1} }

// BumpPatch returns the next fix version: 1.2.3 -> 1.2.4.
func (v Version) BumpPatch() Version {
	return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch + 1}
}

// Bump increments the component named by part.
func (v Version) Bump(part Part) (Version, error) {
	switch part {
	case Major:
		return v.BumpMajor(), nil
	case Minor:
		return v.BumpMinor(), nil
	case Patch:
		return v.BumpPatch(), nil
	default:
		return Version{}, fmt.Errorf("cannot bump %q: expected %q, %q, or %q", part, Major, Minor, Patch)
	}
}

// IsPrerelease reports whether this version carries a pre-release identifier.
func (v Version) IsPrerelease() bool { return len(v.Prerelease) > 0 }

// IsInitialDevelopment reports whether major is 0, meaning the public API may
// change at any time (spec §4).
func (v Version) IsInitialDevelopment() bool { return v.Major == 0 }

// Tag renders the git tag form this project uses: v0.2.0.
func (v Version) Tag() string { return "v" + v.String() }

// String renders the canonical SemVer form, without a leading v.
func (v Version) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Prerelease) > 0 {
		b.WriteByte('-')
		b.WriteString(strings.Join(v.Prerelease, "."))
	}
	if len(v.Build) > 0 {
		b.WriteByte('+')
		b.WriteString(strings.Join(v.Build, "."))
	}
	return b.String()
}

// Compare returns -1, 0, or +1 as v sorts below, equal to, or above other,
// following the precedence rules in spec §11. Build metadata is excluded.
func (v Version) Compare(other Version) int {
	if c := cmpInt(v.Major, other.Major); c != 0 {
		return c
	}
	if c := cmpInt(v.Minor, other.Minor); c != 0 {
		return c
	}
	if c := cmpInt(v.Patch, other.Patch); c != 0 {
		return c
	}
	return comparePrerelease(v.Prerelease, other.Prerelease)
}

// Less reports whether v has lower precedence than other.
func (v Version) Less(other Version) bool { return v.Compare(other) < 0 }

// Equal reports whether v and other have the same precedence. Build metadata is
// ignored, so 1.0.0+a equals 1.0.0+b.
func (v Version) Equal(other Version) bool { return v.Compare(other) == 0 }

// comparePrerelease implements spec §11.3 and §11.4. An empty identifier list
// means "not a pre-release", which outranks every pre-release.
func comparePrerelease(left, right []string) int {
	switch {
	case len(left) == 0 && len(right) == 0:
		return 0
	case len(left) == 0:
		return 1 // a release outranks its own pre-release
	case len(right) == 0:
		return -1
	}
	for i := 0; i < len(left) && i < len(right); i++ {
		if c := compareIdentifier(left[i], right[i]); c != 0 {
			return c
		}
	}
	// All shared identifiers are equal: the shorter set sorts below (§11.4.4).
	return cmpInt(len(left), len(right))
}

// compareIdentifier orders one dot-separated pre-release identifier. Numeric
// identifiers compare numerically and always sort below alphanumeric ones.
func compareIdentifier(left, right string) int {
	leftNum, rightNum := numeric.MatchString(left), numeric.MatchString(right)
	switch {
	case leftNum && rightNum:
		// Both matched the numeric pattern, so both are digit runs without a
		// leading zero, and both fit unless absurdly long.
		l, errL := strconv.Atoi(left)
		r, errR := strconv.Atoi(right)
		if errL != nil || errR != nil {
			return strings.Compare(left, right) // overflow: fall back to ASCII
		}
		return cmpInt(l, r)
	case leftNum:
		return -1
	case rightNum:
		return 1
	default:
		return strings.Compare(left, right)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// CompareStrings is the three-way comparison over unparsed versions, for
// callers such as a shell or a CI step that want a cmp-style integer.
func CompareStrings(left, right string) (int, error) {
	l, err := Parse(left)
	if err != nil {
		return 0, err
	}
	r, err := Parse(right)
	if err != nil {
		return 0, err
	}
	return l.Compare(r), nil
}

// Sort orders versions ascending by precedence, in place.
func Sort(versions []Version) {
	slices.SortFunc(versions, Version.Compare)
}
