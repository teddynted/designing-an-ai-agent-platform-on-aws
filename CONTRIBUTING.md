# Contributing

Thanks for helping. This project is small on purpose: no third-party
dependencies, no code generation, and no build steps beyond the Go toolchain.

## Getting set up

You need Go 1.25 or newer and Git 2.x.

```bash
git clone https://github.com/teddynted/designing-an-ai-agent-platform-on-aws.git
cd designing-an-ai-agent-platform-on-aws

make verify   # gofmt check, go vet, and the tests with -race
```

`make help` lists every target. All of them wrap the Go toolchain or the release
CLI; there is no logic in the Makefile itself.

| Target | What it does |
| --- | --- |
| `make build` | Build the CLI into `bin/` |
| `make test` | Run the tests with the race detector |
| `make cover` | Run the tests and open an HTML coverage report |
| `make verify` | Everything CI runs: format check, vet, tests |
| `make dist` | Cross-compile the release binaries and checksums |
| `make check` | Run the release preflight validations |

## Commit messages

Commits follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/).
This is not decoration: the subject line decides which section of the changelog
a change lands in, and whether it is flagged as breaking.

```text
<type>(<optional scope>)<optional !>: <description>

<optional body>

<optional footers>
```

A real example:

```text
feat(semver): support pre-release series switching

Starting a new series on the same core version restarts the counter,
so --pre beta after --pre rc yields 1.3.0-beta.0.
```

### Types

| Type | Changelog section | Shown in release notes |
| --- | --- | --- |
| `feat` | Features | yes |
| `fix` | Bug Fixes | yes |
| `perf` | Performance Improvements | yes |
| `revert` | Reverts | yes |
| `docs` | Documentation | yes |
| `refactor` | Code Refactoring | yes |
| `build` | Build System | no |
| `ci` | Continuous Integration | no |
| `test` | Tests | no |
| `style` | Styles | no |
| `chore` | Chores | no |

A subject that does not match the grammar is not lost. It is filed under
**Other Changes**, so nothing silently disappears from a release.

### Breaking changes

Mark a breaking change with `!` before the colon, a `BREAKING CHANGE:` footer,
or both. Either one promotes the commit into the **Breaking Changes** section at
the top of the release notes; the footer also supplies the explanatory note.

```text
feat(api)!: drop the v1 endpoints

BREAKING CHANGE: the /v1 routes are gone. Use /v2 instead.
```

The footer must begin a line. Prose that merely mentions the phrase mid-sentence
is not treated as a footer.

## Code

Follow the standards the code already sets:

- **Idiomatic Go.** `gofmt` is enforced by CI. Errors are wrapped with `%w` and
  matched with `errors.Is`, never by comparing message strings.
- **No new dependencies.** The standard library has been enough so far. If you
  believe a dependency is unavoidable, open an issue first.
- **Respect the dependency rule.** `internal/semver` and `internal/changelog`
  are pure: no `os`, no `exec`, no `net`. `internal/release` owns the policy and
  reaches the outside world only through interfaces. See
  [docs/architecture.md](docs/architecture.md).
- **Comment the why.** The code says what it does. A comment earns its place by
  explaining a constraint that the code cannot: why `git tag` runs with
  `--cleanup=verbatim`, why numeric identifiers are compared by length.

### Tests

Every package has tests, and they run in milliseconds because nothing touches
the network or the filesystem:

- `internal/semver` is table-driven, and walks the precedence chain from the
  specification verbatim.
- `internal/git` drives a stub `Runner` instead of the `git` binary.
- `internal/release` drives an in-memory `Git` implementation, so the whole
  validate-plan-apply flow is covered without a repository.
- `internal/github` runs against `httptest`.

Add tests for behaviour, not for coverage. A test that pins *why* a line exists
— `TestCreateTagKeepsMarkdownHeadings`, `TestBumpAlwaysIncreasesPrecedence` —
is worth more than one that restates the implementation.

## Pull requests

1. Branch from `main`.
2. Make the change, with tests.
3. Run `make verify`.
4. Open the pull request. CI runs the tests on Linux, macOS, and Windows, checks
   formatting, and performs a release dry run to prove the next version can
   still be calculated from your commit.

Keep the pull request focused. A refactor and a behaviour change in one diff are
hard to review and harder to revert.

## How a change becomes a release

You do not need to touch the version, the changelog, or the tag. Merging is the
end of your work.

A maintainer later runs one command on `main`:

```bash
go run ./cmd/release minor
```

which validates the repository, calculates the version from the tag history,
and pushes an annotated tag. GitHub Actions then publishes the release, uploads
the binaries, and commits the `CHANGELOG.md` entry that contains your commit.

Never edit `CHANGELOG.md` by hand — it is generated, and your edit will be
overwritten. The full process is described in
[RELEASE_MANAGEMENT.md](RELEASE_MANAGEMENT.md).
