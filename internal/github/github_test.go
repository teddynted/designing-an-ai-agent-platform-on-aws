package github_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/git"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/github"
	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/release"
)

// call records one request the client made.
type call struct {
	method  string
	url     string
	body    string
	headers map[string]string
}

// fakeTransport answers requests from a table keyed by "METHOD path-suffix", and
// records every call. Nothing here touches the network.
type fakeTransport struct {
	responses map[string]github.Response
	calls     []call
	err       error
}

func (f *fakeTransport) Do(method, url string, body []byte, headers map[string]string) (github.Response, error) {
	f.calls = append(f.calls, call{method: method, url: url, body: string(body), headers: headers})
	if f.err != nil {
		return github.Response{}, f.err
	}
	for key, response := range f.responses {
		wantMethod, suffix, _ := strings.Cut(key, " ")
		if method == wantMethod && strings.Contains(url, suffix) {
			return response, nil
		}
	}
	return github.Response{Status: 404, Body: []byte(`{"message":"Not Found"}`)}, nil
}

func ok(body string) github.Response {
	return github.Response{Status: 200, Body: []byte(body)}
}

func newReleases(t *testing.T, transport *fakeTransport) *github.Releases {
	t.Helper()
	client, err := github.NewClient("teddynted/platform", "tok", github.WithTransport(transport))
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	return github.NewReleases(client)
}

func TestNewClientRejectsBadRepository(t *testing.T) {
	for _, repository := range []string{"platform", "", "  "} {
		if _, err := github.NewClient(repository, ""); err == nil {
			t.Errorf("NewClient(%q) should fail", repository)
		}
	}
	if _, err := github.NewClient("teddynted/platform", ""); err != nil {
		t.Errorf("NewClient on owner/name should succeed: %v", err)
	}
}

func TestRequestCarriesAuthAndAPIVersion(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"GET releases/latest": ok(`{"tag_name":"v0.1.0","name":"First","published_at":"2026-07-09T10:00:00Z"}`),
	}}
	if _, err := newReleases(t, transport).LatestRelease(); err != nil {
		t.Fatalf("LatestRelease returned error: %v", err)
	}

	headers := transport.calls[0].headers
	if got, want := headers["Authorization"], "Bearer tok"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got, want := headers["X-GitHub-Api-Version"], github.APIVersion; got != want {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", got, want)
	}
	if got, want := headers["Accept"], "application/vnd.github+json"; got != want {
		t.Errorf("Accept = %q, want %q", got, want)
	}
	if got, want := transport.calls[0].url, "https://api.github.com/repos/teddynted/platform/releases/latest"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

func TestUnauthenticatedClientSendsNoAuthorization(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"GET releases/latest": ok(`{"tag_name":"v0.1.0","published_at":"2026-07-09T10:00:00Z"}`),
	}}
	client, err := github.NewClient("teddynted/platform", "", github.WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := github.NewReleases(client).LatestRelease(); err != nil {
		t.Fatal(err)
	}
	if _, present := transport.calls[0].headers["Authorization"]; present {
		t.Error("an unauthenticated client must not send an Authorization header")
	}
}

func TestWithBaseURL(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"GET releases/latest": ok(`{"tag_name":"v0.1.0","published_at":"2026-07-09T10:00:00Z"}`),
	}}
	client, err := github.NewClient("o/n", "t",
		github.WithTransport(transport),
		github.WithBaseURL("https://ghe.example.com/api/v3/"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := github.NewReleases(client).LatestRelease(); err != nil {
		t.Fatal(err)
	}
	if got, want := transport.calls[0].url, "https://ghe.example.com/api/v3/repos/o/n/releases/latest"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

// The distinction that sends people hunting for a permissions bug that does not
// exist: a rate-limited 403 is a throttle, not a rejection.
func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name           string
		response       github.Response
		wantIsNotFound bool
		wantSubstring  string
	}{
		{
			name:           "not found",
			response:       github.Response{Status: 404, Body: []byte(`{"message":"Not Found"}`)},
			wantIsNotFound: true,
			wantSubstring:  "not found",
		},
		{
			name: "rate limited",
			response: github.Response{
				Status:  403,
				Headers: map[string]string{"x-ratelimit-remaining": "0", "x-ratelimit-reset": "1750000000"},
			},
			wantSubstring: "rate limit exhausted",
		},
		{
			name:          "forbidden for real",
			response:      github.Response{Status: 403, Headers: map[string]string{"x-ratelimit-remaining": "57"}},
			wantSubstring: "rejected the credentials",
		},
		{
			name:          "unauthorised",
			response:      github.Response{Status: 401},
			wantSubstring: "rejected the credentials",
		},
		{
			name:          "server error surfaces GitHub's message",
			response:      github.Response{Status: 500, Body: []byte(`{"message":"Server Error"}`)},
			wantSubstring: "Server Error",
		},
		{
			name:          "unprocessable",
			response:      github.Response{Status: 422, Body: []byte(`{"message":"Validation Failed"}`)},
			wantSubstring: "Validation Failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := &fakeTransport{responses: map[string]github.Response{"GET compare/": tc.response}}
			_, err := newReleases(t, transport).Compare("v0.1.0", "v0.2.0")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantSubstring)
			}
			if got := errors.Is(err, github.ErrNotFound); got != tc.wantIsNotFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %v, want %v", got, tc.wantIsNotFound)
			}
		})
	}
}

// A rate-limit 403 must not be mistaken for an auth failure.
func TestRateLimitIsNotAnAuthFailure(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"GET compare/": {Status: 403, Headers: map[string]string{"x-ratelimit-remaining": "0"}},
	}}
	_, err := newReleases(t, transport).Compare("v0.1.0", "v0.2.0")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "rejected the credentials") {
		t.Errorf("a throttle was reported as a permissions failure: %v", err)
	}
	if !strings.Contains(err.Error(), "throttle, not a permissions failure") {
		t.Errorf("error should say it is a throttle: %v", err)
	}
}

func TestTransportErrorPropagates(t *testing.T) {
	transport := &fakeTransport{err: errors.New("dial tcp: no route to host")}
	if _, err := newReleases(t, transport).LatestRelease(); err == nil {
		t.Error("a transport failure should surface as an error")
	}
}

func TestLatestReleaseAbsentIsNilNotError(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{}} // everything 404s
	got, err := newReleases(t, transport).LatestRelease()
	if err != nil {
		t.Fatalf("a repository with no releases is not an error: %v", err)
	}
	if got != nil {
		t.Errorf("LatestRelease() = %v, want nil", got)
	}
}

func TestReleaseByTagAbsentIsNilNotError(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{}}
	got, err := newReleases(t, transport).ReleaseByTag("v9.9.9")
	if err != nil {
		t.Fatalf("a missing release is not an error: %v", err)
	}
	if got != nil {
		t.Errorf("ReleaseByTag() = %v, want nil", got)
	}
}

func TestToReleaseMapping(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantTitle  string
		wantStatus release.Status
		wantDated  bool
	}{
		{
			name:       "published",
			payload:    `{"tag_name":"v0.1.0","name":"First light","published_at":"2026-07-09T10:00:00Z"}`,
			wantTitle:  "First light",
			wantStatus: release.Released,
			wantDated:  true,
		},
		{
			// GitHub permits a null name; the tag is the honest fallback.
			name:       "null name falls back to the tag",
			payload:    `{"tag_name":"v0.1.0","name":null,"published_at":"2026-07-09T10:00:00Z"}`,
			wantTitle:  "v0.1.0",
			wantStatus: release.Released,
			wantDated:  true,
		},
		{
			// A draft is in progress, and carries no date — a released version
			// without a date would fail Validate.
			name:       "draft is in progress",
			payload:    `{"tag_name":"v0.2.0","name":"Next","draft":true,"published_at":"2026-07-09T10:00:00Z"}`,
			wantTitle:  "Next",
			wantStatus: release.InProgress,
			wantDated:  false,
		},
		{
			name:       "unpublished is in progress",
			payload:    `{"tag_name":"v0.2.0","name":"Next","published_at":null}`,
			wantTitle:  "Next",
			wantStatus: release.InProgress,
			wantDated:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := &fakeTransport{responses: map[string]github.Response{"GET releases/tags/": ok(tc.payload)}}
			got, err := newReleases(t, transport).ReleaseByTag("v0.1.0")
			if err != nil {
				t.Fatalf("ReleaseByTag returned error: %v", err)
			}
			if got == nil {
				t.Fatal("ReleaseByTag returned nil")
			}
			if got.Title != tc.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, tc.wantTitle)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Date.IsZero() == tc.wantDated {
				t.Errorf("Date = %v, wantDated = %v", got.Date, tc.wantDated)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("mapped release should be internally consistent: %v", err)
			}
		})
	}
}

func TestListReleasesSkipsNonSemVerTagsAndSortsDescending(t *testing.T) {
	body := `[
	  {"tag_name":"v0.1.0","published_at":"2026-07-01T00:00:00Z"},
	  {"tag_name":"backup-before-rewrite","published_at":"2026-07-02T00:00:00Z"},
	  {"tag_name":"v0.10.0","published_at":"2026-07-03T00:00:00Z"},
	  {"tag_name":"v0.2.0","published_at":"2026-07-04T00:00:00Z"}
	]`
	transport := &fakeTransport{responses: map[string]github.Response{"GET releases": ok(body)}}

	got, err := newReleases(t, transport).ListReleases(10)
	if err != nil {
		t.Fatalf("ListReleases returned error: %v", err)
	}
	want := []string{"v0.10.0", "v0.2.0", "v0.1.0"} // newest version first, by precedence
	if len(got) != len(want) {
		t.Fatalf("ListReleases returned %d releases, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Tag() != w {
			t.Errorf("releases[%d] = %s, want %s", i, got[i].Tag(), w)
		}
	}
}

// pagingTransport serves a fixed number of releases across pages, honouring the
// page and per_page query parameters the client sends.
type pagingTransport struct {
	total int
	calls []string
}

func (p *pagingTransport) Do(method, target string, _ []byte, _ map[string]string) (github.Response, error) {
	p.calls = append(p.calls, target)

	parsed, err := url.Parse(target)
	if err != nil {
		return github.Response{}, err
	}
	page, _ := strconv.Atoi(parsed.Query().Get("page"))
	perPage, _ := strconv.Atoi(parsed.Query().Get("per_page"))
	if page < 1 {
		page = 1
	}

	start := (page - 1) * perPage
	items := []map[string]any{}
	for i := start; i < start+perPage && i < p.total; i++ {
		items = append(items, map[string]any{
			"tag_name":     fmt.Sprintf("v1.0.%d", i),
			"published_at": "2026-07-01T00:00:00Z",
		})
	}
	body, _ := json.Marshal(items)
	return github.Response{Status: 200, Body: body}, nil
}

func TestListReleasesPaginates(t *testing.T) {
	// 150 releases: a full page of 100, then a short page of 50, then stop.
	transport := &pagingTransport{total: 150}
	client, err := github.NewClient("o/n", "t", github.WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}

	got, err := github.NewReleases(client).ListReleases(150)
	if err != nil {
		t.Fatalf("ListReleases returned error: %v", err)
	}
	if len(got) != 150 {
		t.Errorf("ListReleases returned %d releases, want 150", len(got))
	}
	if len(transport.calls) != 2 {
		t.Errorf("expected 2 page requests, got %d: %v", len(transport.calls), transport.calls)
	}
	if !strings.Contains(transport.calls[0], "per_page=100") {
		t.Errorf("first page should ask for the maximum: %s", transport.calls[0])
	}
}

// A short first page means there is no second one, so the client must not ask.
func TestListReleasesStopsOnShortPage(t *testing.T) {
	transport := &pagingTransport{total: 3}
	client, _ := github.NewClient("o/n", "t", github.WithTransport(transport))

	got, err := github.NewReleases(client).ListReleases(100)
	if err != nil {
		t.Fatalf("ListReleases returned error: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d releases, want 3", len(got))
	}
	if len(transport.calls) != 1 {
		t.Errorf("a short page should end pagination; calls: %v", transport.calls)
	}
}

// The limit must be honoured even when more releases exist.
func TestListReleasesRespectsLimit(t *testing.T) {
	transport := &pagingTransport{total: 500}
	client, _ := github.NewClient("o/n", "t", github.WithTransport(transport))

	got, err := github.NewReleases(client).ListReleases(5)
	if err != nil {
		t.Fatalf("ListReleases returned error: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("got %d releases, want 5", len(got))
	}
	if !strings.Contains(transport.calls[0], "per_page=5") {
		t.Errorf("client should not over-fetch: %s", transport.calls[0])
	}
}

func TestCreateReleaseSendsTheRightBody(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"POST releases": ok(`{"tag_name":"v0.2.0","name":"Release management","published_at":"2026-07-10T00:00:00Z"}`),
	}}
	got, err := newReleases(t, transport).CreateRelease(release.CreateOptions{
		Tag:    "v0.2.0",
		Title:  "Release management",
		Body:   "Notes here",
		Target: "main",
	})
	if err != nil {
		t.Fatalf("CreateRelease returned error: %v", err)
	}
	if got.Tag() != "v0.2.0" {
		t.Errorf("created %s", got.Tag())
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(transport.calls[0].body), &sent); err != nil {
		t.Fatalf("request body was not JSON: %v", err)
	}
	if sent["tag_name"] != "v0.2.0" || sent["name"] != "Release management" || sent["body"] != "Notes here" {
		t.Errorf("unexpected request body: %v", sent)
	}
	if sent["target_commitish"] != "main" {
		t.Errorf("target_commitish = %v, want main", sent["target_commitish"])
	}
	if sent["prerelease"] != false {
		t.Errorf("prerelease = %v, want false", sent["prerelease"])
	}
}

// Pushing v1.0.0-rc.1 must not silently announce itself as a stable release.
func TestCreateReleaseInfersPrereleaseFromTheVersion(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"POST releases": ok(`{"tag_name":"v1.0.0-rc.1","name":"RC","draft":true}`),
	}}
	if _, err := newReleases(t, transport).CreateRelease(release.CreateOptions{
		Tag:        "v1.0.0-rc.1",
		Title:      "RC",
		Prerelease: false, // caller did not ask, but the version says so
	}); err != nil {
		t.Fatalf("CreateRelease returned error: %v", err)
	}

	var sent map[string]any
	json.Unmarshal([]byte(transport.calls[0].body), &sent)
	if sent["prerelease"] != true {
		t.Errorf("prerelease = %v, want true for a v1.0.0-rc.1 tag", sent["prerelease"])
	}
}

func TestCreateReleaseRejectsNonSemVerTag(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{}}
	if _, err := newReleases(t, transport).CreateRelease(release.CreateOptions{Tag: "nightly"}); err == nil {
		t.Error("CreateRelease on a non-SemVer tag should fail")
	}
	if len(transport.calls) != 0 {
		t.Error("CreateRelease should validate before calling GitHub")
	}
}

func TestUpdateReleaseSendsOnlyTheGivenFields(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"GET releases/tags/": ok(`{"id":42,"tag_name":"v0.2.0","published_at":"2026-07-10T00:00:00Z"}`),
		"PATCH releases/42":  ok(`{"id":42,"tag_name":"v0.2.0","name":"Renamed","published_at":"2026-07-10T00:00:00Z"}`),
	}}
	body := "Rewritten notes"
	if _, err := newReleases(t, transport).UpdateRelease("v0.2.0", release.UpdateOptions{Body: &body}); err != nil {
		t.Fatalf("UpdateRelease returned error: %v", err)
	}

	patch := transport.calls[len(transport.calls)-1]
	var sent map[string]any
	if err := json.Unmarshal([]byte(patch.body), &sent); err != nil {
		t.Fatalf("PATCH body was not JSON: %v", err)
	}
	if sent["body"] != "Rewritten notes" {
		t.Errorf("body = %v", sent["body"])
	}
	// Only the fields the caller supplied may be sent; a nil field is not "clear it".
	if _, present := sent["name"]; present {
		t.Errorf("UpdateRelease sent a name it was not given: %v", sent)
	}
	if _, present := sent["draft"]; present {
		t.Errorf("UpdateRelease sent a draft flag it was not given: %v", sent)
	}
}

func TestUpdateReleaseRejectsEmptyUpdate(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{}}
	if _, err := newReleases(t, transport).UpdateRelease("v0.2.0", release.UpdateOptions{}); err == nil {
		t.Error("an update with nothing to update should fail")
	}
	if len(transport.calls) != 0 {
		t.Error("an empty update should not call GitHub")
	}
}

func TestDeleteReleaseResolvesTheIDFirst(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"GET releases/tags/": ok(`{"id":42,"tag_name":"v0.2.0","published_at":"2026-07-10T00:00:00Z"}`),
		"DELETE releases/42": {Status: 204},
	}}
	if err := newReleases(t, transport).DeleteRelease("v0.2.0"); err != nil {
		t.Fatalf("DeleteRelease returned error: %v", err)
	}
	last := transport.calls[len(transport.calls)-1]
	if last.method != "DELETE" || !strings.HasSuffix(last.url, "/releases/42") {
		t.Errorf("last call = %s %s", last.method, last.url)
	}
}

// Re-running a release workflow after a transient failure must not fail because
// the release was created on the first attempt.
func TestUpsertReleaseUpdatesWhenPresent(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"GET releases/tags/": ok(`{"id":42,"tag_name":"v0.2.0","published_at":"2026-07-10T00:00:00Z"}`),
		"PATCH releases/42":  ok(`{"id":42,"tag_name":"v0.2.0","name":"Updated","published_at":"2026-07-10T00:00:00Z"}`),
	}}
	got, err := newReleases(t, transport).UpsertRelease(release.CreateOptions{
		Tag: "v0.2.0", Title: "Updated", Body: "Notes",
	})
	if err != nil {
		t.Fatalf("UpsertRelease returned error: %v", err)
	}
	if got.Title != "Updated" {
		t.Errorf("Title = %q", got.Title)
	}
	for _, c := range transport.calls {
		if c.method == "POST" {
			t.Error("UpsertRelease created a release that already existed")
		}
	}
}

func TestUpsertReleaseCreatesWhenAbsent(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"POST releases": ok(`{"tag_name":"v0.2.0","name":"New","published_at":"2026-07-10T00:00:00Z"}`),
		// GET releases/tags/ is absent, so it 404s: no such release yet.
	}}
	if _, err := newReleases(t, transport).UpsertRelease(release.CreateOptions{
		Tag: "v0.2.0", Title: "New", Body: "Notes",
	}); err != nil {
		t.Fatalf("UpsertRelease returned error: %v", err)
	}
	sawPost := false
	for _, c := range transport.calls {
		if c.method == "POST" {
			sawPost = true
		}
	}
	if !sawPost {
		t.Error("UpsertRelease should create a release that does not exist")
	}
}

func TestCompareMapsFilesAndReversesCommits(t *testing.T) {
	body := `{
	  "files": [
	    {"filename":"docs/new.md","status":"added","additions":40,"deletions":0},
	    {"filename":"README.md","status":"modified","additions":10,"deletions":2},
	    {"filename":"internal/new.go","status":"renamed","additions":1,"deletions":1,"previous_filename":"pkg/old.go"},
	    {"filename":"copied.go","status":"copied","additions":3,"deletions":0},
	    {"filename":"gone.md","status":"removed","additions":0,"deletions":9}
	  ],
	  "commits": [
	    {"sha":"oldest","commit":{"message":"Add docs\n\nbody","author":{"name":"Teddy","date":"2026-07-09T10:00:00Z"}}},
	    {"sha":"newest","commit":{"message":"Fix parser","author":{"name":"Teddy","date":"2026-07-10T10:00:00Z"}}}
	  ]
	}`
	transport := &fakeTransport{responses: map[string]github.Response{"GET compare/": ok(body)}}

	got, err := newReleases(t, transport).Compare("v0.1.0", "v0.2.0")
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}

	if got.Base == nil || got.Base.String() != "0.1.0" || got.Head.String() != "0.2.0" {
		t.Errorf("base=%v head=%v", got.Base, got.Head)
	}
	if !strings.Contains(transport.calls[0].url, "compare/v0.1.0...v0.2.0") {
		t.Errorf("compare should use three dots: %s", transport.calls[0].url)
	}

	// GitHub returns commits oldest-first; this system reads newest-first.
	if got.Commits[0].SHA != "newest" || got.Commits[1].SHA != "oldest" {
		t.Errorf("commits not reversed: %v", got.Commits)
	}
	if got.Commits[1].Subject != "Add docs" || got.Commits[1].Body != "body" {
		t.Errorf("message not split into subject and body: %+v", got.Commits[1])
	}

	byPath := map[string]git.FileChange{}
	for _, f := range got.Files {
		byPath[f.Path] = f
	}
	if renamed := byPath["internal/new.go"]; renamed.Kind != git.Renamed || renamed.PreviousPath != "pkg/old.go" {
		t.Errorf("rename: %+v", renamed)
	}
	if copied := byPath["copied.go"]; copied.Kind != git.Added {
		t.Errorf("a copy is an add: %+v", copied)
	}
	if got.Insertions() != 54 || got.Deletions() != 12 {
		t.Errorf("insertions=%d deletions=%d", got.Insertions(), got.Deletions())
	}
	// Files are sorted by path so output is stable.
	if !slices.IsSortedFunc(got.Files, func(a, b git.FileChange) int { return strings.Compare(a.Path, b.Path) }) {
		t.Errorf("files not sorted: %v", got.Files)
	}
}

// GitHub reports `renamed` for renames with edits too, always with a previous
// filename. Without one the domain object would reject the change, so it must
// degrade rather than fail.
func TestCompareDegradesRenameWithoutPreviousFilename(t *testing.T) {
	body := `{"files":[{"filename":"a.go","status":"renamed","additions":1,"deletions":1}],"commits":[]}`
	transport := &fakeTransport{responses: map[string]github.Response{"GET compare/": ok(body)}}

	got, err := newReleases(t, transport).Compare("v0.1.0", "v0.2.0")
	if err != nil {
		t.Fatalf("Compare returned error: %v", err)
	}
	if got.Files[0].Kind != git.Modified {
		t.Errorf("a rename with no origin should degrade to modified, got %s", got.Files[0].Kind)
	}
}

func TestCompareRejectsNonVersionRefs(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{"GET compare/": ok(`{"files":[],"commits":[]}`)}}
	if _, err := newReleases(t, transport).Compare("v0.1.0", "HEAD"); err == nil {
		t.Error("Compare with a non-version head should fail")
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	transport := &fakeTransport{responses: map[string]github.Response{
		"GET releases/latest": {Status: 200, Body: []byte(`<html>not json</html>`)},
	}}
	if _, err := newReleases(t, transport).LatestRelease(); err == nil {
		t.Error("a non-JSON body should surface as an error")
	}
}
