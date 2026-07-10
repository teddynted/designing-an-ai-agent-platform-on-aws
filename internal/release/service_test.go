package release_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/version"
)

// A test fake is a plain struct with the right methods; it inherits from nothing
// and imports nothing. That is what the structural ports buy.

type fakeRepo struct {
	tags        []git.Tag
	comparison  git.Comparison
	compareErr  error
	compareArgs []string
	created     []string
}

func (f *fakeRepo) ListTags() ([]git.Tag, error) { return f.tags, nil }

func (f *fakeRepo) LatestTag() (*git.Tag, error) {
	if len(f.tags) == 0 {
		return nil, nil
	}
	return &f.tags[len(f.tags)-1], nil
}

func (f *fakeRepo) CurrentTag() (*git.Tag, error) { return nil, nil }

func (f *fakeRepo) PreviousTag(target *version.Version) (*git.Tag, error) {
	ceiling := target
	if ceiling == nil {
		if len(f.tags) == 0 {
			return nil, nil
		}
		ceiling = &f.tags[len(f.tags)-1].Version
	}
	for i := len(f.tags) - 1; i >= 0; i-- {
		if f.tags[i].Version.Less(*ceiling) {
			return &f.tags[i], nil
		}
	}
	return nil, nil
}

func (f *fakeRepo) TagExists(name string) (bool, error) {
	for _, tag := range f.tags {
		if tag.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRepo) CreateTag(name, message, ref string) (git.Tag, error) {
	f.created = append(f.created, name)
	return git.ParseTag(name)
}

func (f *fakeRepo) CommitsBetween(base, head string) ([]git.Commit, error) {
	return f.comparison.Commits, nil
}

func (f *fakeRepo) Compare(base, head string, headVersion *version.Version) (git.Comparison, error) {
	f.compareArgs = append(f.compareArgs, base+".."+head)
	if f.compareErr != nil {
		return git.Comparison{}, f.compareErr
	}
	return f.comparison, nil
}

type fakeHost struct {
	comparison git.Comparison
	compareErr error
	compares   int
}

func (f *fakeHost) ListReleases(int) ([]release.Release, error)   { return nil, nil }
func (f *fakeHost) LatestRelease() (*release.Release, error)      { return nil, nil }
func (f *fakeHost) ReleaseByTag(string) (*release.Release, error) { return nil, nil }
func (f *fakeHost) DeleteRelease(string) error                    { return nil }

func (f *fakeHost) CreateRelease(release.CreateOptions) (release.Release, error) {
	return release.Release{}, nil
}

func (f *fakeHost) UpdateRelease(string, release.UpdateOptions) (release.Release, error) {
	return release.Release{}, nil
}

func (f *fakeHost) Compare(base, head string) (git.Comparison, error) {
	f.compares++
	if f.compareErr != nil {
		return git.Comparison{}, f.compareErr
	}
	return f.comparison, nil
}

func tags(names ...string) []git.Tag {
	var out []git.Tag
	for _, name := range names {
		tag, err := git.ParseTag(name)
		if err != nil {
			panic(err)
		}
		out = append(out, tag)
	}
	return out
}

func hostComparison() git.Comparison {
	base := version.MustParse("0.1.0")
	return git.Comparison{
		Head:    version.MustParse("0.2.0"),
		Base:    &base,
		Commits: []git.Commit{{SHA: "host123", Subject: "feat: from the host"}},
	}
}

func localComparison() git.Comparison {
	base := version.MustParse("0.1.0")
	return git.Comparison{
		Head:    version.MustParse("0.2.0"),
		Base:    &base,
		Commits: []git.Commit{{SHA: "local45", Subject: "feat: from local git"}},
	}
}

func TestComparisonServicePrefersTheHost(t *testing.T) {
	repo := &fakeRepo{tags: tags("v0.1.0"), comparison: localComparison()}
	host := &fakeHost{comparison: hostComparison()}
	service := release.NewComparisonService(repo, host, true, nil)

	got, err := service.Compare("v0.1.0", "v0.2.0", nil)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}
	if got.Commits[0].SHA != "host123" {
		t.Errorf("expected the host's comparison, got %v", got.Commits)
	}
	if len(repo.compareArgs) != 0 {
		t.Error("local git should not have been consulted")
	}
}

// A comparison from a shallow clone can be missing commits, so the fallback is
// permitted but never silent.
func TestComparisonServiceFallsBackWhenAllowed(t *testing.T) {
	repo := &fakeRepo{tags: tags("v0.1.0"), comparison: localComparison()}
	host := &fakeHost{compareErr: errors.New("502 bad gateway")}
	service := release.NewComparisonService(repo, host, true, nil)

	got, err := service.Compare("v0.1.0", "v0.2.0", nil)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}
	if got.Commits[0].SHA != "local45" {
		t.Errorf("expected the local comparison, got %v", got.Commits)
	}
}

func TestComparisonServicePropagatesWhenFallbackForbidden(t *testing.T) {
	repo := &fakeRepo{tags: tags("v0.1.0"), comparison: localComparison()}
	host := &fakeHost{compareErr: errors.New("502 bad gateway")}
	service := release.NewComparisonService(repo, host, false, nil)

	if _, err := service.Compare("v0.1.0", "v0.2.0", nil); err == nil {
		t.Error("with AllowFallback false, a host failure must propagate")
	}
	if len(repo.compareArgs) != 0 {
		t.Error("local git should not have been consulted")
	}
}

// The first release has no predecessor, and only local git can express a diff
// against the empty tree, so the host is not asked.
func TestComparisonServiceSkipsHostForTheFirstRelease(t *testing.T) {
	repo := &fakeRepo{comparison: git.Comparison{Head: version.MustParse("0.1.0")}}
	host := &fakeHost{comparison: hostComparison()}
	service := release.NewComparisonService(repo, host, true, nil)

	got, err := service.Compare("", "v0.1.0", nil)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}
	if host.compares != 0 {
		t.Error("the host cannot compare against the empty tree and should not be asked")
	}
	if !got.IsInitial() {
		t.Error("expected the initial comparison")
	}
}

// Generating notes before the tag is pushed diffs HEAD, which the forge has
// never seen. Asking it anyway spends a request to earn a fallback warning on
// every local release.
func TestComparisonServiceDoesNotAskTheHostAboutUnpushedRefs(t *testing.T) {
	repo := &fakeRepo{tags: tags("v0.1.0"), comparison: localComparison()}
	host := &fakeHost{comparison: hostComparison()}
	service := release.NewComparisonService(repo, host, true, nil)

	head := version.MustParse("0.2.0")
	got, err := service.Compare("v0.1.0", "HEAD", &head)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}
	if host.compares != 0 {
		t.Error("the host cannot resolve HEAD and should not be asked")
	}
	if got.Commits[0].SHA != "local45" {
		t.Errorf("expected the local comparison, got %v", got.Commits)
	}
}

// The comparison describes the release being cut, whatever ref was diffed.
func TestNotesNormaliseTheComparisonHead(t *testing.T) {
	base := version.MustParse("0.1.0")
	comparison := git.Comparison{
		Head:    version.MustParse("0.0.0"), // as a diff of HEAD would leave it
		Base:    &base,
		Commits: []git.Commit{{SHA: "aaaaaaa0", Subject: "feat: a thing"}},
	}
	repo := &fakeRepo{tags: tags("v0.1.0"), comparison: comparison}
	comparisons := release.NewComparisonService(repo, nil, true, nil)
	service := release.NewNotesService(comparisons, release.FixedClock{When: release.MustDate("2026-07-10")})

	notes, err := service.Generate(version.MustParse("0.2.0"), "HEAD")
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if got, want := notes.Comparison.Range(), "v0.1.0...v0.2.0"; got != want {
		t.Errorf("Range() = %q, want %q", got, want)
	}
}

func TestComparisonServiceWithoutAHost(t *testing.T) {
	repo := &fakeRepo{tags: tags("v0.1.0"), comparison: localComparison()}
	service := release.NewComparisonService(repo, nil, true, nil)

	got, err := service.Compare("v0.1.0", "v0.2.0", nil)
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}
	if got.Commits[0].SHA != "local45" {
		t.Errorf("expected the local comparison, got %v", got.Commits)
	}
}

func TestCompareReleaseResolvesThePredecessor(t *testing.T) {
	repo := &fakeRepo{tags: tags("v0.1.0", "v0.2.0"), comparison: localComparison()}
	service := release.NewComparisonService(repo, nil, true, nil)

	// v0.3.0 does not exist yet; its predecessor is still v0.2.0.
	if _, err := service.CompareRelease(version.MustParse("0.3.0"), "HEAD"); err != nil {
		t.Fatalf("CompareRelease returned error: %v", err)
	}
	if got, want := repo.compareArgs[0], "v0.2.0..HEAD"; got != want {
		t.Errorf("compared %q, want %q", got, want)
	}
}

func TestCompareReleaseFirstReleaseHasNoBase(t *testing.T) {
	repo := &fakeRepo{comparison: git.Comparison{Head: version.MustParse("0.1.0")}}
	service := release.NewComparisonService(repo, nil, true, nil)

	if _, err := service.CompareRelease(version.MustParse("0.1.0"), "HEAD"); err != nil {
		t.Fatalf("CompareRelease returned error: %v", err)
	}
	if got, want := repo.compareArgs[0], "..HEAD"; got != want {
		t.Errorf("compared %q, want %q (an empty base)", got, want)
	}
}

func TestCompareReleaseDefaultsHeadToTheTag(t *testing.T) {
	repo := &fakeRepo{tags: tags("v0.1.0"), comparison: localComparison()}
	service := release.NewComparisonService(repo, nil, true, nil)

	if _, err := service.CompareRelease(version.MustParse("0.2.0"), ""); err != nil {
		t.Fatalf("CompareRelease returned error: %v", err)
	}
	if got, want := repo.compareArgs[0], "v0.1.0..v0.2.0"; got != want {
		t.Errorf("compared %q, want %q", got, want)
	}
}

func TestNotesServiceGenerate(t *testing.T) {
	base := version.MustParse("0.1.0")
	comparison := git.Comparison{
		Head: version.MustParse("0.2.0"),
		Base: &base,
		Commits: []git.Commit{
			{SHA: "aaaaaaa0", Subject: "feat: add the roadmap"},
			{SHA: "bbbbbbb0", Subject: "fix: correct the parser"},
			{SHA: "ccccccc0", Subject: "Merge pull request #1 from x"}, // dropped
			{SHA: "ddddddd0", Subject: "chore: tidy"},                  // dropped
			{SHA: "eeeeeee0", Subject: "feat!: drop v1"},               // breaking -> Changed
		},
		Files: []git.FileChange{
			{Path: "a.go", Kind: git.Added, Insertions: 10},
			{Path: "b.go", Kind: git.Modified, Insertions: 5, Deletions: 3},
		},
	}
	repo := &fakeRepo{tags: tags("v0.1.0"), comparison: comparison}
	comparisons := release.NewComparisonService(repo, nil, true, nil)

	when := release.MustDate("2026-07-10")
	notes, err := release.NewNotesService(comparisons, release.FixedClock{When: when}).
		Generate(version.MustParse("0.2.0"), "HEAD")
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}

	// The clock is injected, so the date is a fact the test can assert.
	if !notes.Date.Equal(when) {
		t.Errorf("Date = %v, want %v", notes.Date, when)
	}
	if notes.IsEmpty() {
		t.Fatal("notes should not be empty")
	}

	if got := notes.Entries(release.Added); len(got) != 1 || got[0] != "Add the roadmap (aaaaaaa)" {
		t.Errorf("Added = %v", got)
	}
	if got := notes.Entries(release.Fixed); len(got) != 1 || got[0] != "Correct the parser (bbbbbbb)" {
		t.Errorf("Fixed = %v", got)
	}
	if got := notes.Entries(release.Changed); len(got) != 1 || got[0] != "**Breaking:** Drop v1 (eeeeeee)" {
		t.Errorf("Changed = %v", got)
	}
	if got := notes.Entries(release.Security); len(got) != 0 {
		t.Errorf("Security should be empty, got %v", got)
	}

	// Sections render in Keep a Changelog order, not map order.
	sections := notes.PopulatedSections()
	if len(sections) != 3 {
		t.Fatalf("expected 3 populated sections, got %d", len(sections))
	}
	want := []release.Category{release.Added, release.Changed, release.Fixed}
	for i, category := range want {
		if sections[i].Category != category {
			t.Errorf("sections[%d] = %s, want %s", i, sections[i].Category, category)
		}
	}
}

func TestNotesServiceSummary(t *testing.T) {
	base := version.MustParse("0.1.0")
	tests := []struct {
		name       string
		comparison git.Comparison
		want       string
	}{
		{
			name: "ordinary release",
			comparison: git.Comparison{
				Head:    version.MustParse("0.2.0"),
				Base:    &base,
				Commits: []git.Commit{{Subject: "feat: a"}, {Subject: "feat: b"}},
				Files:   []git.FileChange{{Path: "a", Kind: git.Added, Insertions: 10, Deletions: 2}},
			},
			want: "2 commits across 1 file, with 10 insertions and 2 deletions.",
		},
		{
			name: "initial release",
			comparison: git.Comparison{
				Head:    version.MustParse("0.1.0"),
				Commits: []git.Commit{{Subject: "feat: a"}},
				Files:   []git.FileChange{{Path: "a", Kind: git.Added}, {Path: "b", Kind: git.Added}},
			},
			want: "Initial release. 1 commit across 2 files.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{comparison: tc.comparison}
			comparisons := release.NewComparisonService(repo, nil, true, nil)
			service := release.NewNotesService(comparisons, release.FixedClock{When: release.MustDate("2026-07-10")})

			notes := service.FromComparison(tc.comparison.Head, tc.comparison)
			if notes.Summary != tc.want {
				t.Errorf("Summary = %q, want %q", notes.Summary, tc.want)
			}
		})
	}
}

// An empty release is not an error, but the caller should be able to notice.
func TestNotesIsEmpty(t *testing.T) {
	comparison := git.Comparison{
		Head:    version.MustParse("0.2.0"),
		Commits: []git.Commit{{Subject: "chore: nothing worth announcing"}},
	}
	repo := &fakeRepo{comparison: comparison}
	comparisons := release.NewComparisonService(repo, nil, true, nil)
	notes := release.NewNotesService(comparisons, release.FixedClock{When: time.Now()}).
		FromComparison(version.MustParse("0.2.0"), comparison)

	if !notes.IsEmpty() {
		t.Error("a release of only housekeeping commits has no entries")
	}
}

func TestReleaseValidate(t *testing.T) {
	released := release.Release{Version: version.MustParse("0.1.0"), Status: release.Released}
	if err := released.Validate(); err == nil {
		t.Error("a released version with no date cannot be ordered on the roadmap")
	}
	released.Date = release.MustDate("2026-07-10")
	if err := released.Validate(); err != nil {
		t.Errorf("a dated release should validate: %v", err)
	}
	planned := release.Release{Version: version.MustParse("0.3.0"), Status: release.Planned}
	if err := planned.Validate(); err != nil {
		t.Errorf("a planned release needs no date: %v", err)
	}
}

func TestReleasedOnIsATransitionNotAMutation(t *testing.T) {
	planned := release.Release{Version: version.MustParse("0.2.0"), Title: "T", Status: release.Planned}
	when := release.MustDate("2026-07-10")
	shipped := planned.ReleasedOn(when)

	if planned.Status != release.Planned || !planned.Date.IsZero() {
		t.Error("ReleasedOn must not mutate the receiver")
	}
	if !shipped.IsReleased() || !shipped.Date.Equal(when) {
		t.Errorf("shipped = %+v", shipped)
	}
}

func TestParseStatus(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    release.Status
		wantErr bool
	}{
		{"planned", release.Planned, false},
		{"in-progress", release.InProgress, false},
		{"RELEASED", release.Released, false},
		{" planned ", release.Planned, false},
		{"shipped", "", true},
	} {
		got, err := release.ParseStatus(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseStatus(%q) should fail", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseStatus(%q) = %q, %v", tc.in, got, err)
		}
	}
}

func TestVersionFileRoundTrip(t *testing.T) {
	root := t.TempDir()
	file := release.NewVersionFile(root)

	if file.Exists() {
		t.Fatal("VERSION should not exist yet")
	}
	if _, err := file.Read(); err == nil {
		t.Error("reading a missing VERSION should fail")
	}

	if err := file.Write(version.MustParse("0.1.0"), false); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	got, err := file.Read()
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if got.String() != "0.1.0" {
		t.Errorf("Read() = %s", got)
	}

	// One line, bare version, trailing newline: readable by any tool.
	raw, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "0.1.0\n" {
		t.Errorf("VERSION contents = %q, want %q", raw, "0.1.0\n")
	}
}

// A pipeline that rewinds VERSION because a stale tag was pushed is a class of
// bug that costs a day to diagnose and a second to prevent.
func TestVersionFileRefusesToDowngrade(t *testing.T) {
	root := t.TempDir()
	file := release.NewVersionFile(root)
	if err := file.Write(version.MustParse("0.2.0"), false); err != nil {
		t.Fatal(err)
	}

	if err := file.Write(version.MustParse("0.1.0"), false); err == nil {
		t.Error("writing an older version should be refused")
	}
	current, _ := file.Read()
	if current.String() != "0.2.0" {
		t.Errorf("VERSION was modified despite the refusal: %s", current)
	}

	// The same version is not a downgrade.
	if err := file.Write(version.MustParse("0.2.0"), false); err != nil {
		t.Errorf("rewriting the same version should be allowed: %v", err)
	}
	// And the guard can be lifted deliberately.
	if err := file.Write(version.MustParse("0.1.0"), true); err != nil {
		t.Errorf("an explicit downgrade should be allowed: %v", err)
	}
}

func TestVersionFileRejectsJunk(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "VERSION")
	file := release.NewVersionFile(root)

	for _, contents := range []string{"", "   \n", "not-a-version\n", "1.2\n"} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Read(); err == nil {
			t.Errorf("Read() on %q should fail", contents)
		}
	}
}

func TestVersionFileBump(t *testing.T) {
	root := t.TempDir()
	file := release.NewVersionFile(root)
	if err := file.Write(version.MustParse("0.1.0"), false); err != nil {
		t.Fatal(err)
	}

	// A dry run reports the next version without persisting it.
	next, err := file.Bump(version.Minor, false)
	if err != nil {
		t.Fatalf("Bump returned error: %v", err)
	}
	if next.String() != "0.2.0" {
		t.Errorf("Bump() = %s, want 0.2.0", next)
	}
	if current, _ := file.Read(); current.String() != "0.1.0" {
		t.Errorf("a dry-run bump must not write: VERSION is %s", current)
	}

	if _, err := file.Bump(version.Minor, true); err != nil {
		t.Fatalf("Bump returned error: %v", err)
	}
	if current, _ := file.Read(); current.String() != "0.2.0" {
		t.Errorf("VERSION = %s, want 0.2.0", current)
	}
}
