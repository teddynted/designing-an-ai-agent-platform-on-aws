// Package github talks to the GitHub REST API.
//
// This is a hand-written client over net/http rather than the go-github SDK.
// The reasoning, and what would reverse it, is recorded in ADR-0013. In short:
// we call seven endpoints, the REST API is pinned independently of any SDK by
// the X-GitHub-Api-Version header, and go-github's major version — a new import
// path each time — moves considerably faster than those seven endpoints do.
//
// Transport is the seam. HTTPTransport performs real requests; a test passes a
// fake that returns canned bodies. Client therefore contains only the things
// worth testing — URL construction, auth headers, error mapping, pagination —
// and none of the things that need a network.
package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the public GitHub API. GitHub Enterprise installations
// differ, which is why it is a field rather than a constant reference.
const DefaultBaseURL = "https://api.github.com"

// APIVersion pins the REST API's own contract, independently of this client.
const APIVersion = "2022-11-28"

// MaxPerPage is GitHub's cap on every paginated collection.
const MaxPerPage = 100

// ErrNotFound is returned when a release or comparison does not exist. Callers
// distinguish "no such release" from "the request failed" with errors.Is.
var ErrNotFound = errors.New("not found")

// Error is a failure reported by GitHub, or by the attempt to reach it.
//
// Err carries a sentinel such as ErrNotFound so that errors.Is answers the
// question callers actually ask, while Status stays available for the ones that
// want the code.
type Error struct {
	Message string
	Status  int
	Err     error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

// Response is an HTTP response reduced to what this client reads.
type Response struct {
	Status  int
	Body    []byte
	Headers map[string]string
}

// Header reads a response header case-insensitively.
func (r Response) Header(name string) string {
	if r.Headers == nil {
		return ""
	}
	return r.Headers[strings.ToLower(name)]
}

// Transport performs one HTTP request. The only thing that touches the network.
type Transport interface {
	Do(method, url string, body []byte, headers map[string]string) (Response, error)
}

// HTTPTransport is the real transport.
type HTTPTransport struct {
	Client *http.Client
}

// Do performs the request. A 4xx or 5xx is a Response, not an error: the body
// carries GitHub's explanation, which is the only useful part of it.
func (t HTTPTransport) Do(method, target string, body []byte, headers map[string]string) (Response, error) {
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		return Response{}, &Error{Message: fmt.Sprintf("could not build request: %v", err)}
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := client.Do(request)
	if err != nil {
		return Response{}, &Error{Message: fmt.Sprintf("could not reach GitHub: %v", err)}
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return Response{}, &Error{Message: fmt.Sprintf("could not read GitHub's response: %v", err)}
	}

	flattened := make(map[string]string, len(response.Header))
	for key := range response.Header {
		flattened[strings.ToLower(key)] = response.Header.Get(key)
	}
	return Response{Status: response.StatusCode, Body: payload, Headers: flattened}, nil
}

// Client is authenticated access to one repository's REST endpoints.
type Client struct {
	repository string
	token      string
	transport  Transport
	baseURL    string
}

// Option configures a Client.
type Option func(*Client)

// WithTransport replaces the network transport, for tests.
func WithTransport(t Transport) Option { return func(c *Client) { c.transport = t } }

// WithBaseURL points the client at a different API host, such as an Enterprise
// installation.
func WithBaseURL(base string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(base, "/") }
}

// NewClient builds a client for a repository named "owner/name". The token may
// be empty, which permits unauthenticated reads at a much lower rate limit.
func NewClient(repository, token string, options ...Option) (*Client, error) {
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if !strings.Contains(repository, "/") {
		return nil, fmt.Errorf("repository must be owner/name, got %q", repository)
	}
	client := &Client{
		repository: repository,
		token:      token,
		transport:  HTTPTransport{},
		baseURL:    DefaultBaseURL,
	}
	for _, option := range options {
		option(client)
	}
	return client, nil
}

// Repository is the owner/name this client addresses.
func (c *Client) Repository() string { return c.repository }

func (c *Client) headers() map[string]string {
	headers := map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": APIVersion,
		"User-Agent":           "release-management",
	}
	if c.token != "" {
		headers["Authorization"] = "Bearer " + c.token
	}
	return headers
}

// url builds a repository-scoped endpoint URL. Callers escape any path segment
// they interpolate; a tag name may legally contain characters that would
// otherwise change the path.
func (c *Client) url(path string, query url.Values) string {
	target := fmt.Sprintf("%s/repos/%s/%s", c.baseURL, c.repository, strings.TrimLeft(path, "/"))
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return target
}

// check maps a non-2xx response onto an error that says what actually happened.
//
// A 403 carrying x-ratelimit-remaining: 0 is not GitHub saying "forbidden", it
// is GitHub saying "wait". A release pipeline that dies with "403 Forbidden"
// during a rate-limit window sends people looking for a permissions bug that
// does not exist.
func check(response Response, context string) error {
	if response.Status >= 200 && response.Status < 300 {
		return nil
	}

	if response.Status == http.StatusNotFound {
		return &Error{Message: context + ": not found", Status: 404, Err: ErrNotFound}
	}

	if response.Status == http.StatusForbidden && response.Header("x-ratelimit-remaining") == "0" {
		reset := response.Header("x-ratelimit-reset")
		if reset == "" {
			reset = "unknown"
		}
		return &Error{
			Status: 403,
			Message: fmt.Sprintf(
				"%s: GitHub rate limit exhausted (resets at epoch %s). This is a throttle, not a permissions failure.",
				context, reset,
			),
		}
	}

	if response.Status == http.StatusUnauthorized || response.Status == http.StatusForbidden {
		return &Error{
			Status: response.Status,
			Message: fmt.Sprintf(
				"%s: GitHub rejected the credentials (%d). A release needs a token with `contents: write`.",
				context, response.Status,
			),
		}
	}

	message := ""
	var payload struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(response.Body, &payload) == nil {
		message = payload.Message
	}
	return &Error{
		Status:  response.Status,
		Message: strings.TrimSpace(fmt.Sprintf("%s: GitHub returned %d %s", context, response.Status, message)),
	}
}

// do performs a request and decodes a successful JSON body into out. A nil out
// discards the body, which is what DELETE wants.
func (c *Client) do(method, path string, query url.Values, payload, out any) error {
	var body []byte
	headers := c.headers()
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return &Error{Message: fmt.Sprintf("could not encode request body: %v", err)}
		}
		body = encoded
		headers["Content-Type"] = "application/json"
	}

	response, err := c.transport.Do(method, c.url(path, query), body, headers)
	if err != nil {
		return err
	}
	if err := check(response, method+" "+path); err != nil {
		return err
	}
	if out == nil || len(response.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(response.Body, out); err != nil {
		return &Error{
			Status:  response.Status,
			Message: fmt.Sprintf("%s %s: GitHub returned a %d with a body that is not the expected JSON", method, path, response.Status),
		}
	}
	return nil
}

func (c *Client) get(path string, query url.Values, out any) error {
	return c.do(http.MethodGet, path, query, nil, out)
}

func (c *Client) post(path string, payload, out any) error {
	return c.do(http.MethodPost, path, nil, payload, out)
}

func (c *Client) patch(path string, payload, out any) error {
	return c.do(http.MethodPatch, path, nil, payload, out)
}

func (c *Client) delete(path string) error {
	return c.do(http.MethodDelete, path, nil, nil, nil)
}

// paginate walks pages until limit items are collected or a short page arrives.
//
// It uses page/per_page rather than parsing the Link header: these endpoints are
// stably ordered, and a Link parser is more code to be wrong.
func (c *Client) paginate(path string, limit int, out *[]json.RawMessage) error {
	if limit <= 0 {
		return nil
	}
	for page := 1; len(*out) < limit; page++ {
		perPage := min(MaxPerPage, limit-len(*out))

		query := url.Values{}
		query.Set("page", fmt.Sprint(page))
		query.Set("per_page", fmt.Sprint(perPage))

		var batch []json.RawMessage
		if err := c.get(path, query, &batch); err != nil {
			return err
		}
		*out = append(*out, batch...)
		if len(batch) < perPage {
			break
		}
	}
	if len(*out) > limit {
		*out = (*out)[:limit]
	}
	return nil
}
