package adapter

import "github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"

// The JSON schemas for the reasoning stages. Each mirrors the shape of the loop type it
// decodes into ([loop.Plan], [loop.Evaluation], …) — the schema is ADVICE to the model that
// makes a well-shaped answer likely, and the Go type is the ENFORCEMENT that makes it safe.
// See llm.Structured for why the second is the one that matters.
//
// They live in one file because they are a set: a field added to a loop type and not to its
// schema is a field the model is never asked for, and keeping them side by side is what makes
// that omission visible.

func planSchema(taskTypes []string) llm.Schema {
	return llm.Schema{
		Name:        "execution_plan",
		Description: "A plan that decomposes the objective into a small number of executable tasks.",
		Definition: llm.Object(map[string]any{
			"rationale": llm.String("One sentence on why the plan is shaped this way."),
			"tasks": map[string]any{
				"type":        "array",
				"description": "The tasks, ordered so dependencies come first.",
				"items": llm.Object(map[string]any{
					"id":           llm.String("A short, unique, hyphenated id."),
					"type":         llm.String("The kind of work.", taskTypes...),
					"description":  llm.String("One line on what this task is for."),
					"instructions": llm.String("What the agent should actually do."),
					"dependsOn": map[string]any{
						"type":        "array",
						"description": "Ids of tasks that must finish first.",
						"items":       map[string]any{"type": "string"},
					},
				}, "id", "type", "description", "instructions"),
			},
		}, "rationale", "tasks"),
	}
}

func evaluationSchema() llm.Schema {
	return llm.Schema{
		Name:        "evaluation",
		Description: "A verdict on the task result and a decision about what the loop does next.",
		Definition: llm.Object(map[string]any{
			"taskSucceeded": llm.Bool("Did the task actually do what it was asked?"),
			"goalAchieved":  llm.Bool("Is the whole objective now met? Only true if genuinely complete."),
			"retry":         llm.Bool("Should this task be attempted again?"),
			"replan":        llm.Bool("Is the whole approach wrong, needing a different plan?"),
			"humanRequired": llm.Bool("Does this need a person to decide?"),
			"confidence":    numberInRange("How sure you are of this verdict, from 0 to 1."),
			"reason":        llm.String("One sentence explaining the decision."),
		}, "taskSucceeded", "goalAchieved", "retry", "replan", "humanRequired", "confidence", "reason"),
	}
}

func reflectionSchema() llm.Schema {
	return llm.Schema{
		Name:        "reflection",
		Description: "An analysis of a failure and a revised instruction for the next attempt.",
		Definition: llm.Object(map[string]any{
			"analysis":            llm.String("Why the attempt failed, concretely."),
			"revisedInstructions": llm.String("Corrected instructions for the next attempt; empty to retry as-is."),
			"adjustment":          llm.String("A short phrase naming the strategy change."),
		}, "analysis"),
	}
}

func summarySchema() llm.Schema {
	return llm.Schema{
		Name:        "summary",
		Description: "A short, factual account of the loop run.",
		Definition: llm.Object(map[string]any{
			"outcome":   llm.String("The run's headline — use the factual outcome given."),
			"narrative": llm.String("Two or three honest sentences on what happened."),
			"result":    llm.String("The deliverable or its location, if there is one; empty otherwise."),
		}, "outcome", "narrative"),
	}
}

// numberInRange is a [0, 1] number field. llm has no number helper (its schema builders cover
// string, integer and bool), so confidence — the one fractional field in the loop's
// vocabulary — is spelled out here. The Go side enforces the range in loop.Evaluation.Validate;
// this only advises the model.
func numberInRange(description string) map[string]any {
	return map[string]any{
		"type":        "number",
		"description": description,
		"minimum":     0,
		"maximum":     1,
	}
}
