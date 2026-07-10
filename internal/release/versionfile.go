package release

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// The VERSION file, and the arithmetic on it.
//
// VERSION at the repository root holds the bare current version — 0.2.0, no v,
// one line, trailing newline. It is the answer to "what version is this working
// tree", available to any tool that can read a file, without git and without
// this package.
//
// Writing it is deliberately narrow: Write refuses to move the version backwards
// unless forced. A release pipeline that rewinds VERSION because a stale tag was
// pushed is a class of bug that costs a day to diagnose and a second to prevent.

// VersionFilename is the file's name at the repository root.
const VersionFilename = "VERSION"

// VersionFile reads, validates, and increments the project version.
type VersionFile struct {
	Path string
}

// NewVersionFile locates VERSION under root.
func NewVersionFile(root string) *VersionFile {
	return &VersionFile{Path: filepath.Join(root, VersionFilename)}
}

// Exists reports whether the file is present.
func (v *VersionFile) Exists() bool {
	_, err := os.Stat(v.Path)
	return err == nil
}

// Read returns the current project version.
func (v *VersionFile) Read() (version.Version, error) {
	raw, err := os.ReadFile(v.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return version.Version{}, fmt.Errorf("%s does not exist", v.Path)
	}
	if err != nil {
		return version.Version{}, fmt.Errorf("could not read %s: %w", v.Path, err)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return version.Version{}, fmt.Errorf("%s is empty", v.Path)
	}
	parsed, err := version.Parse(text)
	if err != nil {
		return version.Version{}, fmt.Errorf("%s: %w", v.Path, err)
	}
	return parsed, nil
}

// Write persists next, refusing to go backwards unless allowDowngrade is set.
func (v *VersionFile) Write(next version.Version, allowDowngrade bool) error {
	if !allowDowngrade && v.Exists() {
		current, err := v.Read()
		if err != nil {
			return err
		}
		if next.Less(current) {
			return fmt.Errorf(
				"refusing to downgrade %s from %s to %s; pass --allow-downgrade if this is deliberate",
				v.Path, current, next,
			)
		}
	}
	if err := os.WriteFile(v.Path, []byte(next.String()+"\n"), 0o644); err != nil {
		return fmt.Errorf("could not write %s: %w", v.Path, err)
	}
	return nil
}

// Bump increments the current version by part and, when write is set, persists
// it. It returns the new version either way, so a dry run can report it.
func (v *VersionFile) Bump(part version.Part, write bool) (version.Version, error) {
	current, err := v.Read()
	if err != nil {
		return version.Version{}, err
	}
	next, err := current.Bump(part)
	if err != nil {
		return version.Version{}, err
	}
	if write {
		if err := v.Write(next, false); err != nil {
			return version.Version{}, err
		}
	}
	return next, nil
}
