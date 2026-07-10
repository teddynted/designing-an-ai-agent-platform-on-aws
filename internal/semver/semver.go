// Package semver implements the Semantic Versioning 2.0.0 specification:
// parsing, precedence comparison, and the increment rules used to calculate the
// next release version.
//
// See https://semver.org/spec/v2.0.0.html. The package depends only on the
// standard library and has no knowledge of Git, GitHub, or the release process.
package semver

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrInvalid is wrapped by every error Parse returns, so callers can classify a
// failure with errors.Is without matching on message text.
var ErrInvalid = errors.New("invalid semantic version")

// Version is a parsed semantic version.
//
// The zero value is 0.0.0, a valid version. It is the implicit starting point
// for a repository that has no release tags yet.
type Version struct {
	Major uint64
	Minor uint64
	Patch uint64

	// Prerelease holds the dot-separated identifiers following '-', without the
	// leading '-'. For example "rc.1". Empty for a stable release.
	Prerelease string

	// Build holds the dot-separated identifiers following '+', without the
	// leading '+'. Build metadata is ignored when determining precedence.
	Build string
}

// Parse parses a semantic version. A single leading "v" is tolerated so that
// Git tag names such as "v1.4.0" can be passed through unchanged.
func Parse(s string) (Version, error) {
	in := strings.TrimSpace(s)
	rest := strings.TrimPrefix(in, "v")

	var v Version

	// Build metadata is stripped first: it sits after the pre-release and may
	// itself contain '-', which would otherwise confuse the pre-release split.
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		v.Build, rest = rest[i+1:], rest[:i]
		if err := validateIdentifiers(v.Build, true); err != nil {
			return Version{}, fmt.Errorf("%w %q: build metadata: %w", ErrInvalid, in, err)
		}
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		v.Prerelease, rest = rest[i+1:], rest[:i]
		if err := validateIdentifiers(v.Prerelease, false); err != nil {
			return Version{}, fmt.Errorf("%w %q: pre-release: %w", ErrInvalid, in, err)
		}
	}

	core := strings.Split(rest, ".")
	if len(core) != 3 {
		return Version{}, fmt.Errorf("%w %q: expected MAJOR.MINOR.PATCH", ErrInvalid, in)
	}
	names := [3]string{"major", "minor", "patch"}
	var nums [3]uint64
	for i, part := range core {
		n, err := parseNumeric(part)
		if err != nil {
			return Version{}, fmt.Errorf("%w %q: %s version: %w", ErrInvalid, in, names[i], err)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// MustParse is Parse for versions known to be valid at compile time. It panics
// on failure and is intended for tests and package-level variables.
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// String renders the version in its canonical form, without a "v" prefix.
func (v Version) String() string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(v.Major, 10))
	b.WriteByte('.')
	b.WriteString(strconv.FormatUint(v.Minor, 10))
	b.WriteByte('.')
	b.WriteString(strconv.FormatUint(v.Patch, 10))
	if v.Prerelease != "" {
		b.WriteByte('-')
		b.WriteString(v.Prerelease)
	}
	if v.Build != "" {
		b.WriteByte('+')
		b.WriteString(v.Build)
	}
	return b.String()
}

// Tag renders the version as a Git tag name, e.g. Tag("v") == "v1.4.0".
func (v Version) Tag(prefix string) string { return prefix + v.String() }

// IsPrerelease reports whether the version carries pre-release identifiers and
// therefore denotes an unstable release.
func (v Version) IsPrerelease() bool { return v.Prerelease != "" }

// Core returns the version stripped of pre-release and build metadata.
func (v Version) Core() Version {
	return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch}
}

// Compare orders a before b (-1), after b (+1), or equal (0), following the
// precedence rules in clause 11 of the specification. Build metadata is ignored.
func Compare(a, b Version) int {
	if c := cmp.Compare(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Patch, b.Patch); c != 0 {
		return c
	}
	return comparePrerelease(a.Prerelease, b.Prerelease)
}

// Compare orders v relative to o. See the package-level Compare.
func (v Version) Compare(o Version) int { return Compare(v, o) }

// Less reports whether v has lower precedence than o.
func (v Version) Less(o Version) bool { return Compare(v, o) < 0 }

// Equal reports whether v and o have the same precedence. Two versions that
// differ only in build metadata are equal.
func (v Version) Equal(o Version) bool { return Compare(v, o) == 0 }

// Sort orders versions by ascending precedence, in place.
func Sort(vs []Version) { slices.SortFunc(vs, Compare) }

// Latest returns the highest-precedence version in vs. ok is false when vs is
// empty.
func Latest(vs []Version) (v Version, ok bool) {
	if len(vs) == 0 {
		return Version{}, false
	}
	return slices.MaxFunc(vs, Compare), true
}

// comparePrerelease applies clause 11.3-11.4: a version without a pre-release
// outranks one with, and otherwise identifiers are compared left to right.
func comparePrerelease(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdentifier(as[i], bs[i]); c != 0 {
			return c
		}
	}
	// A larger set of identifiers wins when all preceding ones are equal.
	return cmp.Compare(len(as), len(bs))
}

// compareIdentifier applies clause 11.4.1-11.4.3: numeric identifiers compare
// numerically and always rank below alphanumeric ones, which compare in ASCII
// order.
func compareIdentifier(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		// Leading zeros are rejected at parse time, so a longer digit string is
		// always the larger number. Comparing this way cannot overflow.
		if c := cmp.Compare(len(a), len(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func parseNumeric(s string) (uint64, error) {
	switch {
	case s == "":
		return 0, errors.New("is empty")
	case !isNumeric(s):
		return 0, fmt.Errorf("%q is not a number", s)
	case len(s) > 1 && s[0] == '0':
		return 0, fmt.Errorf("%q has a leading zero", s)
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is out of range", s)
	}
	return n, nil
}

// validateIdentifiers checks a dot-separated identifier set. Numeric
// identifiers may only carry leading zeros in build metadata.
func validateIdentifiers(set string, allowLeadingZeros bool) error {
	for _, id := range strings.Split(set, ".") {
		if id == "" {
			return errors.New("contains an empty identifier")
		}
		for i := 0; i < len(id); i++ {
			if !isIdentifierChar(id[i]) {
				return fmt.Errorf("identifier %q contains invalid character %q", id, string(id[i]))
			}
		}
		if !allowLeadingZeros && isNumeric(id) && len(id) > 1 && id[0] == '0' {
			return fmt.Errorf("numeric identifier %q has a leading zero", id)
		}
	}
	return nil
}

func isIdentifierChar(c byte) bool {
	return c == '-' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
