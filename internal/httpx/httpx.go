// Package httpx holds the HTTP resilience the platform's outbound integrations
// share: retrying the right things, backing off without synchronising a herd,
// reading a response without trusting it, and never printing a secret.
//
// # Why this exists
//
// Milestone 5 taught the platform to call n8n. Milestone 6 taught it to call
// OpenClaw. Both need exponential backoff with jitter, both must honour
// Retry-After, both must bound what they read, and both must keep a token out of a
// log line. Written twice, those drift: one grows a fix the other never gets, and
// the bug surfaces in whichever one you were not looking at.
//
// So the *mechanics* live here, and the *policy* does not. This package
// deliberately does NOT know what is retryable — that is a domain decision, and a
// wrong one is expensive:
//
//   - For n8n, a workflow that ran and failed must never be retried, because
//     retrying it runs it again.
//   - For OpenClaw, an agent that ran and failed must never be retried, because
//     retrying it costs money and may open a second pull request.
//
// Both callers pass their own Retryable func. What they get from here is a correct
// backoff and a bounded read, which is the part nobody should be writing twice.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxBackoff caps the exponential growth. Without a cap, the fourth retry of a
// long-running integration waits for hours — and an unbounded backoff is
// indistinguishable from a hang.
const MaxBackoff = 30 * time.Second

// Policy describes how hard to try. Everything in it is configuration, because
// what is sensible for a webhook handler answering GitHub in three seconds is not
// sensible for an agent run that takes twenty minutes.
type Policy struct {
	// Attempts is the TOTAL number of attempts, not the number of retries after
	// the first. 1 means "try once, never retry". Naming this wrong is how a
	// service ends up making four requests when its operator configured three.
	Attempts int

	// Delay is the base for the exponential backoff.
	Delay time.Duration

	// Retryable decides whether asking again could plausibly help. It is supplied
	// by the caller because only the caller knows what its failures mean.
	Retryable func(error) bool

	// Sleep and Jitter are injectable so tests run instantly and deterministically.
	// Nil means the real ones.
	Sleep  func(context.Context, time.Duration) error
	Jitter func(time.Duration) time.Duration
}

// ErrRetriesExhausted is returned when every attempt failed on a retryable error.
// It always wraps the last one, so errors.Is still finds the cause: "we gave up"
// is far less useful to an on-call engineer than "we gave up on a timeout".
var ErrRetriesExhausted = errors.New("retries exhausted")

// RetryAfterError carries a server-specified delay, so a struggling service can
// tell us when to come back and we can believe it.
type RetryAfterError struct {
	Err   error
	Delay time.Duration
}

func (e RetryAfterError) Error() string { return e.Err.Error() }
func (e RetryAfterError) Unwrap() error { return e.Err }

// Attempt is the function Do retries. It receives the 1-based attempt number,
// which callers put in the request body so the far side can tell a retry from a
// new request.
type Attempt func(ctx context.Context, attempt int) error

// Do runs the attempt, retrying what the policy says is worth retrying. It
// reports the number of attempts made, which belongs in the logs: "it worked, on
// the third try" is a different fact from "it worked", and the difference is a
// service that is quietly degrading.
func Do(ctx context.Context, p Policy, fn Attempt) (attempts int, err error) {
	if p.Attempts < 1 {
		p.Attempts = 1
	}
	sleep, jitter := p.Sleep, p.Jitter
	if sleep == nil {
		sleep = SleepCtx
	}
	if jitter == nil {
		jitter = FullJitter
	}

	var last error
	for attempt := 1; attempt <= p.Attempts; attempt++ {
		attempts = attempt

		if err := fn(ctx, attempt); err == nil {
			return attempts, nil
		} else {
			last = err
		}

		if p.Retryable == nil || !p.Retryable(last) {
			// A 401 will be a 401 next time too. Retrying it burns the caller's
			// deadline, and on an auth failure it can look like an attack.
			return attempts, last
		}
		if attempt == p.Attempts {
			break
		}

		if err := sleep(ctx, Backoff(p.Delay, attempt, last, jitter)); err != nil {
			// The caller's deadline expired while we waited to retry. Report the
			// reason we were retrying, not the sleep — the interesting failure is
			// the one that started all this.
			return attempts, fmt.Errorf("%w: %v", last, err)
		}
	}

	return attempts, fmt.Errorf("%w after %d attempts: %w", ErrRetriesExhausted, attempts, last)
}

// Backoff is exponential, capped, jittered — and yields to a server that told us
// when to come back.
func Backoff(base time.Duration, attempt int, err error, jitter func(time.Duration) time.Duration) time.Duration {
	var after RetryAfterError
	if errors.As(err, &after) && after.Delay > 0 {
		// The server knows more about its own load than our exponent does, and
		// ignoring Retry-After while it is asking for room is how you finish
		// knocking over something that was merely struggling.
		return after.Delay
	}
	if jitter == nil {
		jitter = FullJitter
	}
	grown := float64(base) * math.Pow(2, float64(attempt-1))
	return jitter(time.Duration(math.Min(grown, float64(MaxBackoff))))
}

// FullJitter picks uniformly in [0, d].
//
// Jitter is not decoration. A fleet of handlers recovering from a restart, all
// retrying on the same exponential schedule, retries in lockstep and knocks the
// service straight back over. "Full jitter" beats the alternatives in AWS's own
// analysis, and its worst case — retrying almost immediately — is exactly what you
// want when the outage was a blip.
func FullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(d) + 1))
}

// NoJitter is for tests that need an exact delay.
func NoJitter(d time.Duration) time.Duration { return d }

// SleepCtx waits, unless the context gives up first.
func SleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ParseRetryAfter reads the header, which may be seconds or an HTTP date.
func ParseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(h); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// ReadBounded reads at most max bytes.
//
// Without a bound, a service that answers with a gigabyte — because it is broken,
// or because it is not the service you think you are talking to — can exhaust this
// process's memory just by replying. Once per retry.
func ReadBounded(r io.Reader, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, max))
}

// Drain empties and closes a response body so its connection can be reused. A body
// left unread is a connection thrown away — and under retry pressure, the pool is
// exactly what you want.
func Drain(res *http.Response, max int64) {
	if res == nil || res.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, max))
	_ = res.Body.Close()
}

// Snippet bounds an untrusted string before it reaches a log line.
//
// A service that answers with a megabyte of HTML should not be able to write a
// megabyte into CloudWatch on our behalf — once per retry, at whatever CloudWatch
// charges per GB.
func Snippet(b []byte) string {
	const max = 256
	s := strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " ")
	if s == "" {
		return "(empty body)"
	}
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// Redacted describes a secret without revealing it, so a start-up log can say
// "here is exactly what I am configured with" — which is worth a great deal at 3am
// — without that being the thing that leaks the credential.
//
// It never shows a prefix. A prefix of a short token is most of the token.
func Redacted(secret string) string {
	if secret == "" {
		return "(not set)"
	}
	return fmt.Sprintf("(set, %d chars)", len(secret))
}
