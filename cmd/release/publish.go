package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/changelog"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/github"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
)

// resolveTag returns the tag a post-tag command should act on: the --tag flag,
// the tag GitHub Actions is running for, or the latest release tag.
func resolveTag(ctx context.Context, svc *release.Service, flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if ref := os.Getenv("GITHUB_REF_NAME"); ref != "" && os.Getenv("GITHUB_REF_TYPE") == "tag" {
		return ref, nil
	}

	latest, err := svc.LatestTag(ctx)
	if err != nil {
		return "", err
	}
	if latest == "" {
		return "", errors.New("no release tags exist; pass --tag explicitly")
	}
	return latest, nil
}

// notesCommand implements `release notes`: render the release notes for a tag.
func notesCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: go run ./cmd/release notes [flags]\n\nRender the release notes for a tag to stdout.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	var opts repoFlags
	opts.register(fs)
	tag := fs.String("tag", "", "tag to render notes for (default: the latest release tag)")
	out := fs.String("out", "", "write the notes to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	p := newPrinter(os.Stdout, os.Stderr, useColor(opts.noColor, os.Stderr))
	svc := release.New(opts.config())

	name, err := resolveTag(ctx, svc, *tag)
	if err != nil {
		return err
	}
	rel, err := svc.Snapshot(ctx, name)
	if err != nil {
		return err
	}
	notes := changelog.RenderNotes(rel, changelog.DefaultSections())

	if *out == "" {
		fmt.Fprint(p.out, notes)
		return nil
	}
	if err := os.WriteFile(*out, []byte(notes), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", *out, err)
	}
	p.ok("Wrote the notes for %s to %s", name, *out)
	return nil
}

// changelogCommand implements `release changelog`: render a CHANGELOG.md entry
// for a tag, and optionally insert it into the file.
func changelogCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("changelog", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: go run ./cmd/release changelog [flags]\n\nRender a CHANGELOG.md entry for a tag. Without --write the entry goes to stdout.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	var opts repoFlags
	opts.register(fs)
	tag := fs.String("tag", "", "tag to render an entry for (default: the latest release tag)")
	file := fs.String("file", "CHANGELOG.md", "changelog file to update")
	write := fs.Bool("write", false, "insert the entry into the changelog file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	p := newPrinter(os.Stdout, os.Stderr, useColor(opts.noColor, os.Stderr))
	svc := release.New(opts.config())

	name, err := resolveTag(ctx, svc, *tag)
	if err != nil {
		return err
	}
	rel, err := svc.Snapshot(ctx, name)
	if err != nil {
		return err
	}
	entry := changelog.RenderEntry(rel, changelog.DefaultSections())

	if !*write {
		fmt.Fprint(p.out, entry)
		return nil
	}

	path := *file
	if opts.dir != "" && !filepath.IsAbs(path) {
		path = filepath.Join(opts.dir, path)
	}

	// A missing changelog is not an error: the first release creates it.
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	updated, changed := changelog.Insert(existing, rel.Version.String(), entry)
	if !changed {
		p.ok("%s already documents %s, nothing to do", *file, name)
		return nil
	}
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	p.ok("Added %s to %s", name, *file)
	return nil
}

// publishCommand implements `release publish`: create or update the GitHub
// Release for a tag and attach its assets.
func publishCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: go run ./cmd/release publish [flags]\n\nCreate or update the GitHub Release for a tag. Requires GITHUB_TOKEN.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	var opts repoFlags
	opts.register(fs)
	tag := fs.String("tag", "", "tag to publish (default: $GITHUB_REF_NAME, else the latest release tag)")
	repo := fs.String("repo", "", "target repository as owner/name (default: derived from the git remote)")
	name := fs.String("name", "", "release title (default: the tag name)")
	draft := fs.Bool("draft", false, "create the release as a draft")
	dryRun := fs.Bool("dry-run", false, "print the notes that would be published without calling the API")
	apiURL := fs.String("api-url", "", "GitHub API base URL (default: $GITHUB_API_URL, else the public API)")
	uploadURL := fs.String("upload-url", "", "GitHub asset upload base URL (default: $GITHUB_UPLOAD_URL, else the public endpoint)")

	var assets stringSlice
	fs.Var(&assets, "asset", "file or glob to attach to the release; repeatable")

	if err := fs.Parse(args); err != nil {
		return err
	}

	p := newPrinter(os.Stdout, os.Stderr, useColor(opts.noColor, os.Stderr))
	svc := release.New(opts.config())

	tagName, err := resolveTag(ctx, svc, *tag)
	if err != nil {
		return err
	}
	rel, err := svc.Snapshot(ctx, tagName)
	if err != nil {
		return err
	}
	notes := changelog.RenderNotes(rel, changelog.DefaultSections())

	if *dryRun {
		p.warn("Dry run: nothing was published")
		p.blank()
		fmt.Fprint(p.out, notes)
		return nil
	}

	owner, repoName, err := resolveRepository(*repo, rel.Repo)
	if err != nil {
		return err
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return errors.New("GITHUB_TOKEN is not set; it is needed to create the GitHub Release")
	}

	client := github.NewClient(token,
		github.WithAPIURL(firstNonEmpty(*apiURL, os.Getenv("GITHUB_API_URL"))),
		github.WithUploadURL(firstNonEmpty(*uploadURL, os.Getenv("GITHUB_UPLOAD_URL"))),
		github.WithUserAgent("go-release-cli/"+version),
	)

	title := *name
	if title == "" {
		title = tagName
	}
	input := github.ReleaseInput{
		TagName:    tagName,
		Name:       title,
		Body:       notes,
		Draft:      *draft,
		Prerelease: rel.Version.IsPrerelease(),
	}

	// Re-running the workflow for a tag refreshes the release rather than
	// failing on a duplicate.
	published, err := upsertRelease(ctx, client, owner, repoName, input, p)
	if err != nil {
		return err
	}

	paths, err := expandAssets(assets)
	if err != nil {
		return err
	}
	if len(assets) > 0 && len(paths) == 0 {
		p.warn("No files matched %s; the release has no assets", strings.Join(assets, ", "))
	}
	if err := uploadAssets(ctx, client, owner, repoName, published.ID, paths, p); err != nil {
		return err
	}

	p.blank()
	p.ok("Release ready: %s", published.HTMLURL)
	return nil
}

// uploadAssets attaches each file to the release, replacing any asset of the
// same name so that re-running the workflow for a tag refreshes the downloads
// instead of failing. The existing assets are listed once rather than once per
// file.
func uploadAssets(ctx context.Context, client *github.Client, owner, repo string, releaseID int64, paths []string, p *printer) error {
	if len(paths) == 0 {
		return nil
	}

	existing, err := client.ListAssets(ctx, owner, repo, releaseID)
	if err != nil {
		return fmt.Errorf("listing the existing release assets: %w", err)
	}
	byName := make(map[string]int64, len(existing))
	for _, a := range existing {
		byName[a.Name] = a.ID
	}

	for _, path := range paths {
		name := filepath.Base(path)
		if id, taken := byName[name]; taken {
			if err := client.DeleteAsset(ctx, owner, repo, id); err != nil {
				return fmt.Errorf("replacing the existing asset %s: %w", name, err)
			}
		}
		if _, err := client.UploadAsset(ctx, owner, repo, releaseID, path); err != nil {
			return fmt.Errorf("uploading %s: %w", path, err)
		}
		p.ok("Uploaded %s", name)
	}
	return nil
}

// upsertRelease creates the release, or updates it when the tag already has one.
func upsertRelease(ctx context.Context, client *github.Client, owner, repo string, in github.ReleaseInput, p *printer) (*github.Release, error) {
	existing, err := client.GetReleaseByTag(ctx, owner, repo, in.TagName)
	switch {
	case err == nil:
		p.step("Updating the existing release for %s", in.TagName)
		return client.UpdateRelease(ctx, owner, repo, existing.ID, in)

	case errors.Is(err, github.ErrNotFound):
		p.step("Creating the release for %s", in.TagName)
		return client.CreateRelease(ctx, owner, repo, in)

	default:
		return nil, err
	}
}

// resolveRepository decides which GitHub repository to publish to: the --repo
// flag, the GITHUB_REPOSITORY variable set by Actions, or the git remote.
func resolveRepository(flagValue string, fromRemote changelog.Repository) (owner, name string, err error) {
	value := flagValue
	if value == "" {
		value = os.Getenv("GITHUB_REPOSITORY")
	}
	if value != "" {
		owner, name, ok := strings.Cut(value, "/")
		if !ok || owner == "" || name == "" {
			return "", "", fmt.Errorf("repository %q is not in owner/name form", value)
		}
		return owner, name, nil
	}

	if fromRemote.Owner == "" || fromRemote.Name == "" {
		return "", "", errors.New("cannot determine the target repository; pass --repo owner/name")
	}
	return fromRemote.Owner, fromRemote.Name, nil
}

// expandAssets resolves the asset globs into a deduplicated, ordered file list.
func expandAssets(patterns []string) ([]string, error) {
	var paths []string
	seen := make(map[string]bool)

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid asset pattern %q: %w", pattern, err)
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				return nil, err
			}
			if info.IsDir() || seen[match] {
				continue
			}
			seen[match] = true
			paths = append(paths, match)
		}
	}
	return paths, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
