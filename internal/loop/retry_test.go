package loop

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: 30 * time.Second, Multiplier: 2}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 0},                 // not a retry
		{1, 1 * time.Second},   // base
		{2, 2 * time.Second},   // ×2
		{3, 4 * time.Second},   // ×2
		{4, 8 * time.Second},   // ×2
		{5, 16 * time.Second},  // ×2
		{6, 30 * time.Second},  // capped (would be 32)
		{10, 30 * time.Second}, // still capped
	}
	for _, tc := range tests {
		if got := p.Backoff(tc.attempt); got != tc.want {
			t.Errorf("Backoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// A multiplier of 1.0 is a fixed delay, not compounding growth.
func TestBackoffFixedDelay(t *testing.T) {
	p := RetryPolicy{BaseDelay: 5 * time.Second, MaxDelay: time.Minute, Multiplier: 1}
	for attempt := 1; attempt <= 5; attempt++ {
		if got := p.Backoff(attempt); got != 5*time.Second {
			t.Errorf("Backoff(%d) = %v, want a fixed 5s", attempt, got)
		}
	}
}

func TestShouldRetry(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 2}
	tests := []struct {
		name         string
		outcome      Outcome
		eval         Evaluation
		retriesSoFar int
		want         bool
	}{
		{"transient failure the evaluator wants retried, budget left", Outcome{Transient: true}, Evaluation{Retry: true}, 0, true},
		{"deterministic failure is never retried", Outcome{Transient: false}, Evaluation{Retry: true}, 0, false},
		{"evaluator did not ask to retry", Outcome{Transient: true}, Evaluation{Retry: false}, 0, false},
		{"a success is not retried", Outcome{Transient: true}, Evaluation{TaskSucceeded: true, Retry: true}, 0, false},
		{"budget exhausted", Outcome{Transient: true}, Evaluation{Retry: true}, 2, false},
		{"last of the budget", Outcome{Transient: true}, Evaluation{Retry: true}, 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRetry(tc.outcome, tc.eval, tc.retriesSoFar, policy); got != tc.want {
				t.Errorf("shouldRetry = %v, want %v", got, tc.want)
			}
		})
	}
}
