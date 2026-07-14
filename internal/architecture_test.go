// Package internal_test enforces the architecture the documentation claims.
//
// Every integration milestone (5, 6, 7) is built the same way: a core package that
// owns the platform's side of a boundary — its types, its errors, its Service, its
// interface — and a client package that implements that interface against one vendor.
//
//	internal/workflow  ← the platform's side   |  internal/n8n       ← speaks to n8n
//	internal/agent     ← the platform's side   |  internal/openclaw  ← speaks to OpenClaw
//	internal/llm       ← the platform's side   |  internal/ollama    ← speaks to Ollama
//
// The dependency must point INWARD: the client knows about the core, and the core knows
// nothing about the client. That is what makes the interface a seam rather than a
// decoration — and it is what lets each Service be tested against a fake with no HTTP
// server anywhere.
//
// # Why this is a test and not a sentence in a document
//
// It IS a sentence in several documents, and the documents are right. But an
// architecture rule that is only checked when someone remembers to check it is a rule
// that will be broken by a reasonable-looking import on a Tuesday — the kind that
// compiles, passes every other test, and quietly welds the platform to a vendor.
//
// It was also nearly missed once: the shell one-liner used to verify this by hand
// silently did nothing (zsh does not word-split unquoted parameters) and reported
// success for all three seams without testing any of them. A check that can pass
// vacuously is worse than no check, because it buys false confidence. So it is a test.
package internal_test

import (
	"go/build"
	"testing"
)

const module = "github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/"

// seams are the boundaries the platform's clean architecture rests on.
var seams = []struct {
	core   string // the platform's side: types, errors, Service, interface
	client string // the vendor's side: HTTP, auth, retries, quirks
	vendor string
}{
	{"workflow", "n8n", "n8n (M5)"},
	{"agent", "openclaw", "OpenClaw (M6)"},
	{"llm", "ollama", "Ollama (M7)"},
}

// TestTheCoreNeverDependsOnItsVendor is the mechanical test that the seams are real.
//
// If this fails, an interface has stopped being a seam: something in the platform's own
// vocabulary has learned about a specific vendor, and swapping that vendor — or running
// two during a migration, which is the whole point of Milestones 8 to 10 — is no longer
// a change of one implementation.
func TestTheCoreNeverDependsOnItsVendor(t *testing.T) {
	for _, seam := range seams {
		t.Run(seam.core+"_does_not_import_"+seam.client, func(t *testing.T) {
			deps := transitiveImports(t, module+seam.core)

			if deps[module+seam.client] {
				t.Errorf(
					"internal/%s imports internal/%s.\n\n"+
						"The dependency must point the other way: %s is the PLATFORM's side of the\n"+
						"boundary (its types, its errors, its Service, its interface) and %s is one\n"+
						"vendor's implementation of it. A core that knows about its vendor is not an\n"+
						"abstraction — it is a vendor with extra steps, and %s can no longer be\n"+
						"replaced without touching everything above it.",
					seam.core, seam.client, seam.core, seam.client, seam.vendor)
			}
		})
	}
}

// TestTheVendorDependsOnTheCore is the other half, and it is not a formality.
//
// A client that did NOT import its core would be one that had grown its own private
// vocabulary — its own error types, its own request struct — and the Service above it
// would be translating between two shapes instead of orchestrating one. The interface
// would still compile. It would simply have stopped meaning anything.
func TestTheVendorDependsOnTheCore(t *testing.T) {
	for _, seam := range seams {
		t.Run(seam.client+"_implements_"+seam.core, func(t *testing.T) {
			deps := transitiveImports(t, module+seam.client)

			if !deps[module+seam.core] {
				t.Errorf("internal/%s does not import internal/%s — it should be implementing that "+
					"package's interface and speaking its errors, not inventing its own",
					seam.client, seam.core)
			}
		})
	}
}

// TestNoVendorKnowsAboutAnother keeps the integrations independent of each other.
//
// The temptation is real and it always looks harmless: the agent's client wants an
// error the LLM's client already defines, so it imports it. Do that twice and the three
// integrations are one integration, and the router in Milestone 10 cannot swap a
// provider without dragging OpenClaw's HTTP client along with it.
//
// Shared MECHANICS live in internal/httpx, deliberately. Shared VOCABULARY does not
// exist, deliberately.
func TestNoVendorKnowsAboutAnother(t *testing.T) {
	for _, seam := range seams {
		deps := transitiveImports(t, module+seam.client)

		for _, other := range seams {
			if other.client == seam.client {
				continue
			}
			if deps[module+other.client] {
				t.Errorf("internal/%s imports internal/%s — the integrations must not know about "+
					"each other. Shared mechanics belong in internal/httpx; shared vocabulary "+
					"between two vendors is a coupling nobody asked for",
					seam.client, other.client)
			}
		}
	}
}

// transitiveImports returns every package reachable from the given one, excluding the
// standard library.
//
// It uses go/build rather than shelling out, so it cannot pass vacuously the way the
// shell version did.
func transitiveImports(t *testing.T, pkg string) map[string]bool {
	t.Helper()

	seen := map[string]bool{}
	var walk func(string)

	walk = func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true

		p, err := build.Import(path, "", 0)
		if err != nil {
			// A package that does not resolve is a broken build, which every other test
			// would already be shouting about. Do not double-report it.
			return
		}
		for _, imported := range p.Imports {
			// The standard library has no dots in its first path element. Skipping it keeps
			// this walk small and its failures readable.
			if !hasDot(imported) {
				continue
			}
			walk(imported)
		}
	}

	walk(pkg)
	delete(seen, pkg) // a package does not import itself
	return seen
}

func hasDot(path string) bool {
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			return false // reached a separator before a dot: standard library
		}
		if path[i] == '.' {
			return true
		}
	}
	return false
}
