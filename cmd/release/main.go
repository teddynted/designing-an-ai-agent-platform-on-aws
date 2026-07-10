// Command release is the single entry point for this project's release
// management: it validates the repository, calculates the next semantic
// version, and creates and pushes the annotated Git tag that triggers the
// release workflow.
//
// The same binary is used by GitHub Actions after the tag lands, to render
// release notes, update CHANGELOG.md, and publish the GitHub Release. Keeping
// both halves in one program means the version rules are written down exactly
// once.
//
// Usage:
//
//	go run ./cmd/release patch      # 1.2.3 -> 1.2.4
//	go run ./cmd/release minor      # 1.2.3 -> 1.3.0
//	go run ./cmd/release major      # 1.2.3 -> 2.0.0
//
// Run "go run ./cmd/release help" for the full command list.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/semver"
)

// version is the version of this tool, stamped at build time with
//
//	-ldflags "-X main.version=v1.2.3"
var version = "dev"

// errAborted is returned when the user declines the confirmation prompt.
var errAborted = errors.New("aborted")

// Exit codes: 1 for a failed release, 2 for a usage error, 130 for a release
// the user cancelled.
const (
	exitFailure = 1
	exitUsage   = 2
	exitAborted = 130
)

func main() {
	// A release is a sequence of network calls; Ctrl-C should cancel the one in
	// flight rather than leave git waiting on a prompt.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:])
	switch {
	case err == nil:
		return
	case errors.Is(err, flag.ErrHelp):
		// The flag package has already printed the usage text.
		os.Exit(exitUsage)
	case errors.Is(err, errAborted):
		fmt.Fprintln(os.Stderr, "aborted")
		os.Exit(exitAborted)
	case errors.Is(err, errUsage):
		fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
		usage(os.Stderr)
		os.Exit(exitUsage)
	default:
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(exitFailure)
	}
}

// errUsage marks an error the user can fix by reading the usage text.
var errUsage = errors.New("usage")

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}

	command, rest := args[0], args[1:]
	switch command {
	case "major", "minor", "patch":
		bump, err := semver.ParseBump(command)
		if err != nil {
			return err
		}
		return bumpCommand(ctx, bump, rest)

	case "check":
		return checkCommand(ctx, rest)
	case "notes":
		return notesCommand(ctx, rest)
	case "changelog":
		return changelogCommand(ctx, rest)
	case "publish":
		return publishCommand(ctx, rest)

	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil

	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, command)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `release — Semantic Versioning and release management for this repository

Usage:
  go run ./cmd/release <command> [flags]

Cutting a release (run these locally, on the default branch):
  major       Tag the next major release, for incompatible changes
  minor       Tag the next minor release, for new backwards-compatible features
  patch       Tag the next patch release, for backwards-compatible bug fixes
  check       Run the preflight validations without tagging anything

Post-tag automation (run by GitHub Actions once the tag is pushed):
  notes       Render the release notes for a tag
  changelog   Render a CHANGELOG.md entry for a tag, or write it into the file
  publish     Create or update the GitHub Release for a tag

Other:
  version     Print the version of this tool
  help        Print this message

Examples:
  go run ./cmd/release minor --dry-run     Preview the next minor release
  go run ./cmd/release patch               Tag and push the next patch release
  go run ./cmd/release minor --pre rc      Tag v1.3.0-rc.0
  go run ./cmd/release notes --tag v1.3.0  Print the notes for an existing tag

Run "go run ./cmd/release <command> -h" to see the flags for a command.
`)
}
