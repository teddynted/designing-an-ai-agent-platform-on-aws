package loop

import "time"

// RetryPolicy bounds how a single task is retried after a transient failure.
//
// It is the loop's OWN retry layer, and it sits above two others that already exist — the
// provider's per-inference retries (Milestone 7/8) and the agent runtime's per-call retries
// (Milestone 6). That is deliberate and it is a different job: those lower layers retry a
// single network call that blipped; this one retries a whole TASK — a fresh agent execution,
// a fresh piece of reasoning — because the task, not the call, was what failed. Stacking them
// is fine as long as each is bounded, which is the entire point of a policy rather than a
// loop with a bare `for`.
type RetryPolicy struct {
	// MaxRetries is how many additional attempts a failed task gets. Zero means a task gets
	// its one attempt and no more — a legitimate choice for a loop that would rather stop and
	// be looked at than spend twice.
	MaxRetries int

	// BaseDelay is the wait before the first retry.
	BaseDelay time.Duration

	// MaxDelay caps the backoff, so exponential growth cannot turn the third retry into an
	// hour-long wait that outlives the loop's own timeout.
	MaxDelay time.Duration

	// Multiplier is the backoff factor. 2.0 doubles the wait each attempt. 1.0 makes it a
	// fixed delay — also valid, and easier to reason about when the failures are not
	// contention but a flaky dependency.
	Multiplier float64
}

// Backoff returns the delay before the given retry attempt (1-based: attempt 1 is the first
// retry). The delay is returned to the DRIVER, which does the waiting — the reducer never
// sleeps. In the synchronous Runner that is a sleep; under n8n it is a durable wait node,
// which is the whole reason backoff is a value the reducer emits rather than a `time.Sleep`
// it performs: a delay the loop controls but does not execute survives a process that dies
// during it.
//
// # Why backoff at all
//
// A transient failure is usually a system under momentary stress — a throttled provider, a
// runtime mid-restart, a Spot instance being reclaimed elsewhere. Retrying instantly hammers
// the exact thing that is already struggling; backing off gives it room to recover, and
// exponential backoff assumes that if it has not recovered after one short wait, it needs a
// longer one. It is the same reasoning the router's health cooldown rests on.
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	delay := p.BaseDelay
	for i := 1; i < attempt; i++ {
		next := time.Duration(float64(delay) * p.Multiplier)
		if next <= delay {
			// A multiplier of 1.0 (or less) means a fixed delay; stop compounding rather than
			// looping pointlessly.
			break
		}
		delay = next
		if p.MaxDelay > 0 && delay >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	if p.MaxDelay > 0 && delay > p.MaxDelay {
		return p.MaxDelay
	}
	return delay
}

// shouldRetry decides whether a failed task gets another attempt. It is the loop's version of
// the router's canFailOver, and it answers with the same discipline: only a TRANSIENT failure
// is worth repeating, because re-running a task that failed deterministically — the agent ran
// and produced something we rejected, the objective was impossible — gets the same failure
// and a second bill.
//
// The retry is also gated on the evaluator having asked for it AND the budget allowing it.
// All three must hold: a deterministic failure is not retried however much the evaluator
// wants it, a transient failure is not retried past its budget however transient, and neither
// is retried if the evaluator judged the task actually succeeded.
func shouldRetry(o Outcome, e Evaluation, retriesSoFar int, policy RetryPolicy) bool {
	if e.TaskSucceeded {
		return false
	}
	if !e.Retry {
		return false
	}
	if !o.Transient {
		// Deterministic. The next attempt fails the same way. This is the line that stops a
		// loop from spending its whole retry budget re-running a doomed task.
		return false
	}
	return retriesSoFar < policy.MaxRetries
}
