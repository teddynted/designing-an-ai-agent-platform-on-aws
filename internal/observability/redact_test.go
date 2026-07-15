package observability

import "testing"

func TestIsSensitive(t *testing.T) {
	sensitive := []string{
		"token", "Token", "TOKEN",
		"secret", "password", "passwd",
		"authorization", "Authorization",
		"api_key", "API-Key", "apikey",
		"n8n_api_key",    // suffix match on a compound key
		"github_secret",  // suffix
		"webhook_token",  // suffix
		"aws_access_key", // suffix on accesskey
		"prompt",         // repository content
		"completion",
		"payload",
	}
	for _, k := range sensitive {
		if !isSensitive(k) {
			t.Errorf("isSensitive(%q) = false, want true", k)
		}
	}

	safe := []string{
		"service", "component", "correlationId", "workflowId",
		"repository", "branch", "commitSha", "durationMs",
		"attempts", "status", "tokens", // "tokens" is a count, not a secret
	}
	for _, k := range safe {
		if isSensitive(k) {
			t.Errorf("isSensitive(%q) = true, want false (this is not a secret)", k)
		}
	}
}
