# ADR-0014: Delegate to the `git` binary rather than depend on go-git

**Status:** Accepted
**Date:** 2026-07-10

## Context

Release notes are a **diff between two releases**. The Release Management module must read tags, walk a commit range, and diff two refs — and it must produce the *same* answer whether the data came from a local clone or from GitHub's Compare API, because the module falls back from one to the other ([ADR-0013](0013-hand-written-github-rest-client.md)).

The brief asks that an existing Go Git library be evaluated before any custom Git functionality is written. The candidate is [`go-git`](https://github.com/go-git/go-git): pure Go, no CGO, no `git` binary required.

Four properties of `git`'s output are load-bearing here, and each is a place where a reimplementation may differ.

**Three-dot diffs, two-dot logs.** `git diff base...head` diffs from the **merge base** of the two refs. That is precisely what GitHub's Compare API reports, and therefore what keeps the local and remote comparisons agreeing. `git log base..head`, meanwhile, is the set of commits reachable from `head` but not `base` — the ordinary "what is new" question. Three dots on a log is the *symmetric difference*, which is emphatically not that. The two spellings differ by one character and mean different things.

**Rename detection is a heuristic, not a fact.** Git detects renames only when asked (`-M`) and otherwise reports an add plus a delete. GitHub runs its own detection server-side. go-git implements a third. Three heuristics, three answers, on the same commit range.

**`-z` framing.** Paths may contain spaces, quotes, and newlines. Git's default output *quotes* such paths (`"a\nb"`), and un-quoting them correctly is a parser nobody should write twice. `-z` sidesteps it by emitting raw bytes with NUL terminators.

**A binary file has no line counts.** `--numstat` prints `-` for both, which must become "not a meaningful unit" and not `0`. Summing the latter as zero silently understates a release.

We are not choosing between "use a library" and "write Git in Go". We are choosing between **delegating to git** and **depending on a second implementation of it**.

## Decision

**Shell out to the `git` binary**, behind a `Runner` interface in `internal/git`.

```go
type Runner interface {
    Run(args ...string) (string, error)
}
```

`ExecRunner` invokes the real binary; tests pass a fake that answers from a table and records the exact arguments issued. That last part matters: the tests assert that the diff uses `...` and the log uses `..`, which is the kind of bug no amount of output-shape testing would catch.

Everything is asked for in machine-readable form — `--format` with explicit ASCII unit and record separators, `-z` for paths, `--numstat` and `--name-status` for counts and kinds. Never the porcelain a human reads.

Rejected alternatives:

- **go-git v5.** Stable, but in maintenance. Its rename detection, merge-base diff, and binary handling are independent reimplementations; matching GitHub's Compare API output is the comparison layer's entire job, and a second heuristic makes that harder, not easier.
- **go-git v6.** Still `v6.0.0-alpha` as of this decision. Not a foundation for release tooling.

## Consequences

### Positive

- The diff semantics are **git's**, not an approximation of them. `base...head` means what `git` means by it, on every platform, forever.
- Zero dependencies for the Git surface.
- `-z` and `--numstat` handling is exercised directly against the format git actually emits, including paths containing newlines.
- The `Runner` seam makes the whole package unit-testable with no repository on disk.

### Negative

- **`git` must be on `PATH`.** This is the real cost. It is acceptable because release tooling runs where git already is — a developer's machine, and a CI job that had to clone the repository to exist. The failure is explicit: `git is not on PATH`, not a mysterious empty result.
- **Process spawn per operation.** A release performs on the order of ten `git` invocations. Irrelevant here; it would not be in a tool walking thousands of commits.
- **Output parsing is our problem.** Git's plumbing formats are stable by policy, and we use only plumbing — but a parser is a parser. Mitigated by testing the exact byte framing, including the trailing-NUL case that made an early version of the rename parser read past the end of a truncated record.
- **No in-process access to object internals.** Anything wanting to read blobs without a working tree would need go-git after all.

## When to revisit

Adopt go-git if the module ever needs to operate on a repository it has not checked out, run in an environment without `git`, or read object internals directly. None of those is on the roadmap; all three would be a different tool.
