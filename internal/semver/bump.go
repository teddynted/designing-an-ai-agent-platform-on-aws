package semver

import (
	"fmt"
	"strconv"
	"strings"
)

// Bump identifies which component of a version a release increments.
type Bump uint8

const (
	// BumpPatch is for backwards-compatible bug fixes.
	BumpPatch Bump = iota + 1
	// BumpMinor is for backwards-compatible functionality.
	BumpMinor
	// BumpMajor is for incompatible API changes.
	BumpMajor
)

// ParseBump maps a CLI argument such as "minor" onto a Bump.
func ParseBump(s string) (Bump, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "patch":
		return BumpPatch, nil
	case "minor":
		return BumpMinor, nil
	case "major":
		return BumpMajor, nil
	default:
		return 0, fmt.Errorf("unknown bump %q: expected major, minor, or patch", s)
	}
}

// String returns the lower-case name of the bump.
func (b Bump) String() string {
	switch b {
	case BumpPatch:
		return "patch"
	case BumpMinor:
		return "minor"
	case BumpMajor:
		return "major"
	default:
		return "unknown"
	}
}

// BumpBetween reports which component was incremented to get from one version
// to the next. ok is false when to does not rank above from, which happens for
// the first release, where there is nothing to compare against.
//
// It is the inverse of Bump, and lets an already-published tag describe itself.
func BumpBetween(from, to Version) (b Bump, ok bool) {
	// Components are only meaningful once to is known to outrank from.
	// Otherwise 2.0.0 to 1.2.3 would look like a minor bump, because its minor
	// component happens to be larger.
	if Compare(from, to) >= 0 {
		return 0, false
	}

	switch {
	case to.Major > from.Major:
		return BumpMajor, true
	case to.Minor > from.Minor:
		return BumpMinor, true
	case to.Patch > from.Patch:
		return BumpPatch, true
	default:
		// The core is unchanged and only the pre-release advanced, as in
		// 1.3.0-rc.0 to 1.3.0-rc.1, or 1.3.0-rc.1 to 1.3.0.
		return BumpPatch, true
	}
}

// Bump returns the next stable version.
//
// When v is a pre-release that already leads to the requested version, the
// pre-release identifiers are simply dropped rather than the core being
// incremented again: 1.2.3-rc.1 patched is 1.2.3, not 1.2.4. This matches the
// behaviour developers expect from a release candidate graduating to a release.
// Build metadata never survives a bump.
func (v Version) Bump(b Bump) Version {
	next := v.Core()
	switch b {
	case BumpMajor:
		if v.IsPrerelease() && v.Minor == 0 && v.Patch == 0 {
			return next
		}
		next.Major++
		next.Minor, next.Patch = 0, 0
	case BumpMinor:
		if v.IsPrerelease() && v.Patch == 0 {
			return next
		}
		next.Minor++
		next.Patch = 0
	case BumpPatch:
		if v.IsPrerelease() {
			return next
		}
		next.Patch++
	}
	return next
}

// BumpPrerelease returns the next pre-release version in the series named id
// (for example "rc" or "beta").
//
// When v is already a pre-release of the version that b would produce, the
// trailing counter advances: 1.3.0-rc.1 minor-bumped in the "rc" series becomes
// 1.3.0-rc.2. Otherwise the core is bumped and a fresh series is started, so
// 1.2.3 minor-bumped becomes 1.3.0-rc.0.
func (v Version) BumpPrerelease(b Bump, id string) Version {
	target := v.Bump(b)

	// v.Bump(b) equals the core exactly when v is a pre-release of the version
	// the caller is asking for, i.e. when the bump only drops identifiers.
	if v.IsPrerelease() && target == v.Core() {
		if next, ok := nextInSeries(v.Prerelease, id); ok {
			out := v.Core()
			out.Prerelease = next
			return out
		}
	}
	target.Prerelease = id + ".0"
	return target
}

// nextInSeries advances the counter of a pre-release belonging to series id.
// ok is false when pre belongs to a different series, which means the caller
// should start a new one.
func nextInSeries(pre, id string) (string, bool) {
	if pre == id {
		return id + ".0", true
	}
	if !strings.HasPrefix(pre, id+".") {
		return "", false
	}
	parts := strings.Split(pre[len(id)+1:], ".")
	last := parts[len(parts)-1]
	if !isNumeric(last) {
		return pre + ".0", true
	}
	n, err := strconv.ParseUint(last, 10, 64)
	if err != nil {
		return "", false
	}
	parts[len(parts)-1] = strconv.FormatUint(n+1, 10)
	return id + "." + strings.Join(parts, "."), true
}
