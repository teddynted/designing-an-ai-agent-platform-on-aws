package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestClient points a Client at a test server for both API and uploads.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient("test-token", WithAPIURL(srv.URL), WithUploadURL(srv.URL), WithHTTPClient(srv.Client()))
}

func TestGetReleaseByTag(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/repos/o/r/releases/tags/v1.0.0"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != apiVersion {
			t.Errorf("X-GitHub-Api-Version = %q, want %q", got, apiVersion)
		}
		json.NewEncoder(w).Encode(Release{ID: 7, TagName: "v1.0.0", HTMLURL: "https://example.test/r/1"})
	}))

	rel, err := c.GetReleaseByTag(context.Background(), "o", "r", "v1.0.0")
	if err != nil {
		t.Fatalf("GetReleaseByTag: %v", err)
	}
	if rel.ID != 7 || rel.HTMLURL != "https://example.test/r/1" {
		t.Errorf("release = %+v", rel)
	}
}

func TestGetReleaseByTagNotFound(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"Not Found"}`)
	}))

	_, err := c.GetReleaseByTag(context.Background(), "o", "r", "v9.9.9")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetReleaseByTag on a missing tag = %v, want ErrNotFound", err)
	}
}

func TestCreateRelease(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/o/r/releases" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		var in ReleaseInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if in.TagName != "v1.0.0" || in.Body != "notes" || !in.Prerelease {
			t.Errorf("input = %+v", in)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(Release{ID: 1, TagName: in.TagName})
	}))

	rel, err := c.CreateRelease(context.Background(), "o", "r", ReleaseInput{
		TagName: "v1.0.0", Name: "v1.0.0", Body: "notes", Prerelease: true,
	})
	if err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}
	if rel.ID != 1 {
		t.Errorf("release ID = %d, want 1", rel.ID)
	}
}

// The tag of an existing release cannot be changed, so it must not be sent.
func TestUpdateReleaseOmitsTag(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/o/r/releases/42" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		var raw map[string]any
		json.NewDecoder(r.Body).Decode(&raw)
		if _, present := raw["tag_name"]; present {
			t.Errorf("tag_name should be omitted on update, body = %v", raw)
		}
		json.NewEncoder(w).Encode(Release{ID: 42})
	}))

	if _, err := c.UpdateRelease(context.Background(), "o", "r", 42, ReleaseInput{TagName: "v1.0.0", Body: "fresh"}); err != nil {
		t.Fatalf("UpdateRelease: %v", err)
	}
}

func TestAPIErrorIncludesValidationDetail(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"message":"Validation Failed","errors":[{"resource":"Release","field":"tag_name","code":"already_exists"}]}`)
	}))

	_, err := c.CreateRelease(context.Background(), "o", "r", ReleaseInput{TagName: "v1.0.0"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("CreateRelease error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
	for _, want := range []string{"Validation Failed", "Release.tag_name", "already_exists"} {
		if !strings.Contains(apiErr.Error(), want) {
			t.Errorf("error %q should mention %q", apiErr.Error(), want)
		}
	}
}

func TestUploadAsset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "release_linux_amd64.tar.gz")
	if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	var uploaded []byte
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/o/r/releases/5/assets" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		if got := r.URL.Query().Get("name"); got != "release_linux_amd64.tar.gz" {
			t.Errorf("upload name = %q", got)
		}
		// An explicit length lets GitHub reject a truncated upload.
		if r.ContentLength != int64(len("payload")) {
			t.Errorf("ContentLength = %d, want %d", r.ContentLength, len("payload"))
		}
		uploaded, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(Asset{ID: 100, Name: "release_linux_amd64.tar.gz"})
	}))

	asset, err := c.UploadAsset(context.Background(), "o", "r", 5, path)
	if err != nil {
		t.Fatalf("UploadAsset: %v", err)
	}
	if string(uploaded) != "payload" {
		t.Errorf("uploaded %q, want %q", uploaded, "payload")
	}
	if asset.ID != 100 {
		t.Errorf("asset = %+v", asset)
	}
}

func TestListAndDeleteAssets(t *testing.T) {
	var deleted int64
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/releases/5/assets":
			json.NewEncoder(w).Encode([]Asset{{ID: 99, Name: "old.txt"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/o/r/releases/assets/99":
			deleted = 99
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	assets, err := c.ListAssets(context.Background(), "o", "r", 5)
	if err != nil {
		t.Fatalf("ListAssets: %v", err)
	}
	if len(assets) != 1 || assets[0].Name != "old.txt" {
		t.Fatalf("ListAssets = %+v", assets)
	}
	if err := c.DeleteAsset(context.Background(), "o", "r", assets[0].ID); err != nil {
		t.Fatalf("DeleteAsset: %v", err)
	}
	if deleted != 99 {
		t.Error("DeleteAsset should have deleted asset 99")
	}
}

func TestContentType(t *testing.T) {
	if got := contentType("checksums.txt"); got == "" || got == "application/octet-stream" {
		t.Errorf("contentType(.txt) = %q, want a text media type", got)
	}
	if got := contentType("release_linux_amd64"); got != "application/octet-stream" {
		t.Errorf("contentType(no extension) = %q, want application/octet-stream", got)
	}
}
