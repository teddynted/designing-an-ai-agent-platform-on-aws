// Package github is a minimal client for the parts of the GitHub REST API that
// publishing a release needs: creating or updating a release, and attaching
// assets to it.
//
// It is written against net/http rather than a generated SDK to keep the module
// free of third-party dependencies. Only the response fields the tool uses are
// modelled.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotFound is returned when a resource does not exist. Callers use it to
// distinguish "create a release" from "update the existing one".
var ErrNotFound = errors.New("not found")

// Default endpoints for github.com. GitHub Enterprise Server uses different
// hosts, which is why they are configurable.
const (
	DefaultAPIURL    = "https://api.github.com"
	DefaultUploadURL = "https://uploads.github.com"

	// apiVersion pins the REST API so that a future default change on GitHub's
	// side cannot alter the shape of the responses parsed here.
	apiVersion = "2022-11-28"
)

// Client talks to the GitHub REST API on behalf of one token.
type Client struct {
	httpClient *http.Client
	token      string
	apiURL     string
	uploadURL  string
	userAgent  string
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithAPIURL sets the REST API base URL, for GitHub Enterprise Server.
func WithAPIURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.apiURL = strings.TrimSuffix(u, "/")
		}
	}
}

// WithUploadURL sets the asset upload base URL, for GitHub Enterprise Server.
func WithUploadURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.uploadURL = strings.TrimSuffix(u, "/")
		}
	}
}

// WithUserAgent sets the User-Agent header. GitHub requires one.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// NewClient returns a client authenticated with token.
func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		token:      token,
		apiURL:     DefaultAPIURL,
		uploadURL:  DefaultUploadURL,
		userAgent:  "go-release-cli",
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Release is a GitHub Release.
type Release struct {
	ID         int64  `json:"id"`
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	Body       string `json:"body"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	HTMLURL    string `json:"html_url"`
}

// Asset is a file attached to a release.
type Asset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// ReleaseInput is the payload for creating or updating a release.
type ReleaseInput struct {
	TagName    string `json:"tag_name,omitempty"`
	Name       string `json:"name,omitempty"`
	Body       string `json:"body"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// APIError describes a non-2xx response from the API.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// GetReleaseByTag looks up the release attached to a tag. It returns ErrNotFound
// when no release exists for that tag yet.
func (c *Client) GetReleaseByTag(ctx context.Context, owner, repo, tag string) (*Release, error) {
	path := fmt.Sprintf("/repos/%s/%s/releases/tags/%s", owner, repo, url.PathEscape(tag))
	var out Release
	if err := c.do(ctx, http.MethodGet, c.apiURL+path, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateRelease publishes a new release.
func (c *Client) CreateRelease(ctx context.Context, owner, repo string, in ReleaseInput) (*Release, error) {
	path := fmt.Sprintf("/repos/%s/%s/releases", owner, repo)
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out Release
	if err := c.do(ctx, http.MethodPost, c.apiURL+path, bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateRelease rewrites an existing release, so that re-running the workflow
// for a tag refreshes the notes rather than failing.
func (c *Client) UpdateRelease(ctx context.Context, owner, repo string, id int64, in ReleaseInput) (*Release, error) {
	path := fmt.Sprintf("/repos/%s/%s/releases/%d", owner, repo, id)
	// The tag of an existing release is immutable; sending it back is a no-op
	// at best and an error at worst.
	in.TagName = ""
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out Release
	if err := c.do(ctx, http.MethodPatch, c.apiURL+path, bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAssets returns the assets already attached to a release.
func (c *Client) ListAssets(ctx context.Context, owner, repo string, id int64) ([]Asset, error) {
	path := fmt.Sprintf("/repos/%s/%s/releases/%d/assets?per_page=100", owner, repo, id)
	var out []Asset
	if err := c.do(ctx, http.MethodGet, c.apiURL+path, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteAsset removes an asset by ID.
func (c *Client) DeleteAsset(ctx context.Context, owner, repo string, assetID int64) error {
	path := fmt.Sprintf("/repos/%s/%s/releases/assets/%d", owner, repo, assetID)
	return c.do(ctx, http.MethodDelete, c.apiURL+path, nil, "", nil)
}

// UploadAsset attaches a file to a release. GitHub rejects a name that is
// already taken, so callers that need to be re-runnable should delete the
// colliding asset first; ListAssets and DeleteAsset exist for that.
func (c *Client) UploadAsset(ctx context.Context, owner, repo string, releaseID int64, path string) (*Asset, error) {
	name := filepath.Base(path)

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets?name=%s",
		c.uploadURL, owner, repo, releaseID, url.QueryEscape(name))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, file)
	if err != nil {
		return nil, err
	}
	// Setting ContentLength explicitly lets GitHub reject a truncated upload
	// rather than accept a partial asset.
	req.ContentLength = info.Size()
	c.setHeaders(req, contentType(name))

	var out Asset
	if err := c.send(req, http.MethodPost, endpoint, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// contentType guesses an asset's media type from its extension, defaulting to
// an opaque binary.
func contentType(name string) string {
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func (c *Client) do(ctx context.Context, method, endpoint string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	c.setHeaders(req, contentType)
	return c.send(req, method, endpoint, out)
}

func (c *Client) setHeaders(req *http.Request, contentType string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", c.userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
}

func (c *Client) send(req *http.Request, method, endpoint string, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       endpoint,
			Message:    errorMessage(resp),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github: decoding %s %s response: %w", method, endpoint, err)
	}
	return nil
}

// errorMessage extracts the human-readable part of an API error response,
// falling back to the raw body.
func errorMessage(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil || len(body) == 0 {
		return http.StatusText(resp.StatusCode)
	}

	var payload struct {
		Message string `json:"message"`
		Errors  []struct {
			Resource string `json:"resource"`
			Field    string `json:"field"`
			Code     string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Message == "" {
		return strings.TrimSpace(string(body))
	}

	var msg strings.Builder
	msg.WriteString(payload.Message)
	for _, e := range payload.Errors {
		fmt.Fprintf(&msg, " (%s.%s: %s)", e.Resource, e.Field, e.Code)
	}
	return msg.String()
}
