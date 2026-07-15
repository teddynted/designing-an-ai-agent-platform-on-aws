package adapter

import (
	"strings"
	"testing"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/prompt"
)

// The reasoning prompts render with the EXACT data each adapter method supplies. The prompt
// package renders with missingkey=error, so a field the template names and the adapter forgets
// (or renames) is a render error — but only at runtime, on a real goal. This test moves that
// failure to the build: it renders each loop prompt with the same keys the corresponding
// Reasoner method passes, so a drift between a template and its caller fails here instead of
// on the first goal of the day.
func TestTheLoopPromptsRenderWithTheirData(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
	}{
		{"loop/plan", map[string]any{
			"Objective": "draft a post", "Repository": "x/y (main)", "TaskTypes": "repo-analysis, blog-draft", "Params": "",
		}},
		{"loop/evaluate", map[string]any{
			"Objective": "draft a post", "TaskDescription": "analyse", "Instructions": "read it",
			"ExecutorSuccess": false, "Output": "some output", "Error": "a blip",
		}},
		{"loop/reflect", map[string]any{
			"Objective": "draft a post", "TaskDescription": "analyse", "Instructions": "read it",
			"Output": "some output", "Error": "a blip", "EvaluationReason": "missed the point",
		}},
		{"loop/summarise", map[string]any{
			"Objective": "draft a post", "Outcome": "achieved", "Completed": "analyse, write",
			"Failed": "(none)", "Iterations": 2, "Reflections": 0, "CostUSD": "0.4200", "LastResult": "the draft",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := prompt.Load(tc.name)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			out, err := p.Render(tc.data)
			if err != nil {
				t.Fatalf("Render: %v — a template field the adapter does not supply", err)
			}
			// The objective must actually reach the model, not be dropped.
			if !strings.Contains(out, "draft a post") {
				t.Errorf("%s did not render the objective into the prompt", tc.name)
			}
		})
	}
}
