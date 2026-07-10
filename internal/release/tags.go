package release

import (
	"context"
	"fmt"
	"slices"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// taggedVersion pairs a Git tag with the version it encodes.
type taggedVersion struct {
	tag     string
	version semver.Version
}

// versionSet is the repository's release tags, ordered by ascending precedence.
type versionSet []taggedVersion

// taggedVersions lists the tags that carry the configured prefix and parse as
// semantic versions, ordered oldest first.
//
// Tags that do not parse are ignored rather than fatal: repositories routinely
// carry unrelated tags, and one bad tag should not block every future release.
func (s *Service) taggedVersions(ctx context.Context) (versionSet, error) {
	pattern := s.cfg.TagPrefix + "*"
	if s.cfg.TagPrefix == "" {
		pattern = ""
	}

	names, err := s.git.Tags(ctx, pattern)
	if err != nil {
		return nil, fmt.Errorf("listing tags: %w", err)
	}

	var out versionSet
	for _, name := range names {
		trimmed, ok := trimPrefix(name, s.cfg.TagPrefix)
		if !ok {
			continue
		}
		v, err := semver.Parse(trimmed)
		if err != nil {
			continue
		}
		out = append(out, taggedVersion{tag: name, version: v})
	}

	slices.SortFunc(out, func(a, b taggedVersion) int { return semver.Compare(a.version, b.version) })
	return out, nil
}

// trimPrefix removes an exact prefix, reporting whether it was present. Unlike
// strings.TrimPrefix it rejects a non-matching tag rather than passing it
// through, so "release-1.0.0" is not mistaken for "1.0.0" when the prefix is
// "v".
func trimPrefix(tag, prefix string) (string, bool) {
	if prefix == "" {
		return tag, true
	}
	if len(tag) < len(prefix) || tag[:len(prefix)] != prefix {
		return "", false
	}
	return tag[len(prefix):], true
}

// latest returns the highest-precedence tag.
func (vs versionSet) latest() (taggedVersion, bool) {
	if len(vs) == 0 {
		return taggedVersion{}, false
	}
	return vs[len(vs)-1], true
}

// predecessorOf returns the highest tag ranked strictly below v. It is how a
// release finds the boundary of its commit range, and it is correct even when
// tags are created out of order, because the set is sorted by precedence rather
// than by creation time.
func (vs versionSet) predecessorOf(v semver.Version) (taggedVersion, bool) {
	for i := len(vs) - 1; i >= 0; i-- {
		if semver.Compare(vs[i].version, v) < 0 {
			return vs[i], true
		}
	}
	return taggedVersion{}, false
}
