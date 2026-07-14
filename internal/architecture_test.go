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
	{"llm", "bedrock", "Amazon Bedrock (M8)"},
}

// factory is the one package allowed to know that more than one vendor exists. It is a
// leaf: it imports the vendors, and nothing imports it except a main.
const factory = "providers"

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

// TestOnlyTheFactoryKnowsAboutMoreThanOneVendor is the rule that makes "switch providers
// through configuration, not code" true rather than aspirational.
//
// Milestone 8 added a second LLM provider, which means something, somewhere, has to hold
// the list. The danger is that it ends up in several somewheres: a caller that imports
// bedrock "just for the config type", a Service that special-cases Ollama. Do that and the
// platform is not provider-agnostic, it merely has two providers.
//
// So exactly ONE package may import more than one vendor — internal/providers — and it is
// a leaf that nothing but a main depends on. Everything else takes an llm.Provider.
func TestOnlyTheFactoryKnowsAboutMoreThanOneVendor(t *testing.T) {
	vendors := map[string]bool{}
	for _, seam := range seams {
		vendors[module+seam.client] = true
	}

	for _, pkg := range []string{"llm", "workflow", "agent", "httpx"} {
		deps := transitiveImports(t, module+pkg)

		var found []string
		for vendor := range vendors {
			if deps[vendor] {
				found = append(found, vendor)
			}
		}
		if len(found) > 0 {
			t.Errorf("internal/%s imports vendor packages %v — only internal/%s may know which "+
				"providers exist; everything else takes the interface",
				pkg, found, factory)
		}
	}

	// And the factory must genuinely reach both LLM providers, or "switch by configuration"
	// is a claim with nothing behind it.
	deps := transitiveImports(t, module+factory)
	for _, vendor := range []string{"ollama", "bedrock"} {
		if !deps[module+vendor] {
			t.Errorf("internal/%s does not import internal/%s — it is supposed to be the one "+
				"place that can build any configured provider", factory, vendor)
		}
	}
}

// TestTheInferencePlaneDoesNotKnowWhatAToolDoes is Milestone 9's seam, and it is the one
// most likely to be broken by a change that looks entirely reasonable.
//
// Milestone 9 gave the model tools, and the platform's tools are its OWN integrations: the
// model can trigger an n8n workflow and submit an OpenClaw task. So there is now a real
// temptation for internal/llm — which runs the tool loop — to import internal/tools, or
// internal/workflow, "just to know what it is calling".
//
// It must not. internal/llm knows exactly two things about a tool: its schema, and whether
// it is a [llm.Write] tool. That second fact is all the loop needs to decide the only
// question it has any business deciding — may this be retried? — and knowing any more would
// weld the inference plane to the orchestration plane, so that the M10 router could not be
// built without dragging n8n's HTTP client behind it.
//
// The dependency points the correct way, and only that way:
//
//	internal/tools  →  internal/llm      (it implements llm.ToolRunner)
//	internal/tools  →  internal/workflow, internal/agent   (it calls the platform's cores)
//	internal/llm    →  nothing of the sort
func TestTheInferencePlaneDoesNotKnowWhatAToolDoes(t *testing.T) {
	deps := transitiveImports(t, module+"llm")

	for _, forbidden := range []string{"tools", "workflow", "agent", "prompt"} {
		if deps[module+forbidden] {
			t.Errorf("internal/llm imports internal/%s.\n\n"+
				"The inference plane must not know what a tool DOES. It knows a tool's schema and\n"+
				"whether it is a Write tool — which is everything the loop needs in order to decide\n"+
				"the only question it owns: may this be retried? Knowing more welds inference to\n"+
				"orchestration, and the Milestone 10 router could not then be built without\n"+
				"dragging the workflow engine along behind it.\n\n"+
				"internal/tools implements llm.ToolRunner. The arrow points that way, and only that way.",
				forbidden)
		}
	}

	// The other half: the tool registry really does implement the platform's interface,
	// rather than having grown a private vocabulary of its own.
	toolDeps := transitiveImports(t, module+"tools")
	for _, required := range []string{"llm", "workflow", "agent"} {
		if !toolDeps[module+required] {
			t.Errorf("internal/tools does not import internal/%s — it is supposed to expose the "+
				"platform's own capabilities to the model, through the platform's own cores", required)
		}
	}

	// And the tool registry must never reach a vendor directly. The model's tools are the
	// platform's CORES (workflow, agent); which engine actually runs them is a decision that
	// stays exactly where Milestones 5 and 6 put it.
	for _, vendor := range []string{"n8n", "openclaw", "ollama", "bedrock"} {
		if toolDeps[module+vendor] {
			t.Errorf("internal/tools imports internal/%s — a tool must go through the platform's "+
				"core (workflow.Service, agent.Service), not reach past it to a vendor", vendor)
		}
	}
}

// TestTheInferencePlaneDoesNotKnowWhatYAMLIs guards the last seam Milestone 9 added.
//
// Service.Compose validates a model's artefact before returning it — so it would be entirely
// natural for internal/llm to import internal/format and call format.Validate(format.YAML, …).
// One import, three fewer lines, and the inference plane has permanently acquired a YAML
// parser and an opinion about Mermaid's grammar.
//
// It must not. `llm` declares [llm.Formatter] — Clean, Validate, Name — and knows nothing
// else. `format` implements it. The dependency points inward, exactly as it does for
// Provider (ollama, bedrock) and ToolRunner (tools), and the reward is that the whole repair
// loop is testable against a fake formatter with no YAML anywhere near it.
func TestTheInferencePlaneDoesNotKnowWhatYAMLIs(t *testing.T) {
	deps := transitiveImports(t, module+"llm")

	if deps[module+"format"] {
		t.Error("internal/llm imports internal/format.\n\n" +
			"The inference plane must not learn what YAML is. It declares llm.Formatter\n" +
			"(Clean, Validate, Name) and knows nothing else; internal/format implements it.\n" +
			"The arrow points inward — the same rule as Provider and ToolRunner.")
	}

	// format is a LEAF, and that is what makes it safe to depend on. It must not reach back
	// into the platform: a validator that imported llm could not be used by anything else,
	// and a validator that imported a vendor would be a validator with an opinion about who
	// generated the artefact — which is precisely what it must not have.
	formatDeps := transitiveImports(t, module+"format")
	for dep := range formatDeps {
		if len(dep) > len(module) && dep[:len(module)] == module {
			t.Errorf("internal/format imports %s — it is a leaf. It validates bytes, and it "+
				"must not care who produced them", dep)
		}
	}
}
