# ADR-0013: Write a minimal GitHub REST client rather than depend on go-github

**Status:** Accepted
**Date:** 2026-07-10

## Context

The Release Management module ([13 — Release Management](../architecture/13-release-management.md)) publishes GitHub Releases and reads GitHub's Compare API. It needs exactly seven endpoints:

| Verb | Endpoint | Used for |
|---|---|---|
| `GET` | `/repos/{o}/{r}/releases` | listing releases |
| `GET` | `/repos/{o}/{r}/releases/latest` | the newest stable release |
| `GET` | `/repos/{o}/{r}/releases/tags/{tag}` | resolving a release by tag |
| `POST` | `/repos/{o}/{r}/releases` | publishing |
| `PATCH` | `/repos/{o}/{r}/releases/{id}` | re-publishing after a retry |
| `DELETE` | `/repos/{o}/{r}/releases/{id}` | removing a bad release |
| `GET` | `/repos/{o}/{r}/compare/{base}...{head}` | release notes |

The obvious implementation is [`github.com/google/go-github`](https://github.com/google/go-github), the de facto Go SDK. It is actively maintained, well tested, and models the entire API surface. The project brief asks that it be evaluated before any custom client is written.

Three facts decided it.

**The SDK's major version moves far faster than the endpoints do.** go-github reached **v75** in September 2025, having passed through v66 within roughly the preceding year. Each major version is a **new module path** (`go-github/v74` → `go-github/v75`), so every bump is an import rewrite across every file that touches it. That churn tracks the whole GitHub API — an endpoint we do not call, changing in a way we do not care about, still moves the import path. For seven stable endpoints, the maintenance is paid in full and the benefit is not collected.

**The REST contract is versioned independently of any SDK.** GitHub pins it with the `X-GitHub-Api-Version: 2022-11-28` request header. The seven endpoints above have been stable across the SDK's entire major-version run. The thing we actually depend on is the header, not the library.

**The failure modes we care about are small and specific.** A release pipeline needs to distinguish exactly three things, and the third is the one that costs a day:

- `404` — no such release. Not an error; the caller asked whether one exists.
- `403` **with** `x-ratelimit-remaining: 0` — a **throttle**, not a rejection.
- `401` / `403` otherwise — the token lacks `contents: write`.

That is about thirty lines. A pipeline that dies with "403 Forbidden" during a rate-limit window sends people looking for a permissions bug that does not exist, and no SDK spares us from having to say so clearly.

## Decision

**Write a minimal client over `net/http`**, in `internal/github`, behind a `Transport` interface.

`Transport` performs one HTTP request and is the only thing that touches the network. `HTTPTransport` is the real one; tests pass a fake returning canned bodies. The client therefore contains only what is worth testing — URL construction, auth headers, error mapping, pagination — and none of what needs a network.

The client is reached only through the `release.Host` interface, which is expressed in domain types (`release.Release`, `git.Comparison`) and names no GitHub concept. **Adopting go-github later means rewriting one adapter**, with the interface and its tests unchanged.

Pagination uses `page` / `per_page` rather than parsing the `Link` header: these collections are stably ordered, and a `Link` parser is more code to be wrong.

The module's **only** third-party dependency is `github.com/goccy/go-yaml`, for the hand-editable `RELEASES.yaml` roadmap. YAML is not in the standard library, a roadmap is edited by people and wants comments, and `gopkg.in/yaml.v3` is effectively unmaintained.

## Consequences

### Positive

- The GitHub surface is 200 lines we own, read, and can debug, with no import-path churn.
- Rate-limit exhaustion reports itself as a throttle rather than a permissions failure.
- The whole client is unit-tested with no network and no recorded fixtures.
- One dependency in `go.mod`, so `go mod tidy` output stays legible and the supply chain stays small.

### Negative

- **We own the client.** If GitHub changes a payload we read, we fix it; go-github users would get that fix from a dependency bump. Mitigated by the pinned API version and the narrow surface.
- **Secondary rate limits are not handled.** go-github understands GitHub's abuse-detection backoff; we do not. A release publishes a handful of requests, so this is theoretical — but a future feature that walks every release in a large repository would need it, and would be the moment to reconsider.
- **No typed models for the rest of the API.** Any new endpoint costs a struct and a method. Past roughly fifteen endpoints this trade inverts, and this ADR should be superseded.
- **We reimplement pagination.** Correct for stably-ordered collections; wrong for anything requiring `Link`-header cursors.

## When to revisit

Adopt go-github if any of these becomes true:

- The module calls more than ~15 endpoints.
- We need GitHub App authentication, or secondary rate-limit backoff.
- We start consuming endpoints whose payloads change more often than yearly.

Because the client sits behind `release.Host`, that migration touches one file.
