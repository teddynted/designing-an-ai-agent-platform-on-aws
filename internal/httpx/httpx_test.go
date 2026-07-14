package httpx

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

var (
	errTransient = errors.New("transient")
	errPermanent = errors.New("permanent")
)

func retryTransient(err error) bool { return errors.Is(err, errTransient) }

// instant is a Policy that never actually waits, so the tests are fast and
// deterministic. Everything else about it is the real thing.
func instant(attempts int, retryable func(error) bool) (Policy, *[]time.Duration) {
	var slept []time.Duration
	return Policy{
		Attempts:  attempts,
		Delay:     time.Second,
		Retryable: retryable,
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
		Jitter: NoJitter,
	}, &slept
}

// --- what gets retried ------------------------------------------------------

func TestDoRetriesUntilItSucceeds(t *testing.T) {
	policy, slept := instant(3, retryTransient)
	calls := 0

	attempts, err := Do(context.Background(), policy, func(_ context.Context, attempt int) error {
		calls++
		if attempt != calls {
			t.Errorf("attempt = %d, want %d — the attempt number is sent to the far side, so it must be right", attempt, calls)
		}
		if calls < 3 {
			return errTransient
		}
		return nil
	})

	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 — a caller logs this to see a degrading service", attempts)
	}
	if len(*slept) != 2 {
		t.Errorf("slept %d times, want 2 (there is no wait after the last attempt)", len(*slept))
	}
}

// Retrying the wrong thing is worse than not retrying at all: a 401 will not become
// valid, and an agent that ran and failed will simply run and fail again — after
// spending the money a second time.
func TestDoDoesNotRetryWhatCannotBeFixed(t *testing.T) {
	policy, _ := instant(5, retryTransient)
	calls := 0

	attempts, err := Do(context.Background(), policy, func(context.Context, int) error {
		calls++
		return errPermanent
	})

	if !errors.Is(err, errPermanent) {
		t.Fatalf("error = %v, want the permanent error, unwrapped", err)
	}
	if errors.Is(err, ErrRetriesExhausted) {
		t.Error("a non-retryable failure is not 'retries exhausted' — we never retried")
	}
	if calls != 1 || attempts != 1 {
		t.Errorf("called %d times, want exactly 1", calls)
	}
}

func TestRetriesExhaustedWrapsTheCause(t *testing.T) {
	policy, _ := instant(3, retryTransient)

	attempts, err := Do(context.Background(), policy, func(context.Context, int) error {
		return errTransient
	})

	if !errors.Is(err, ErrRetriesExhausted) {
		t.Errorf("error = %v, want ErrRetriesExhausted", err)
	}
	// The cause must survive: "we gave up" is far less useful on call than "we gave
	// up on a timeout".
	if !errors.Is(err, errTransient) {
		t.Errorf("error = %v, must still unwrap to the cause", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestAttemptsIsTotalNotRetries(t *testing.T) {
	// Naming this wrong is how a service makes four requests when its operator
	// configured three.
	policy, _ := instant(1, retryTransient)
	calls := 0

	if _, err := Do(context.Background(), policy, func(context.Context, int) error {
		calls++
		return errTransient
	}); err == nil {
		t.Fatal("want an error")
	}
	if calls != 1 {
		t.Errorf("Attempts=1 made %d calls, want exactly 1 (it counts attempts, not retries)", calls)
	}
}

// A caller's deadline must win over our retry schedule. Otherwise a webhook handler
// that promised GitHub an answer in three seconds sits in a backoff for thirty.
func TestDoRespectsTheCallersDeadline(t *testing.T) {
	policy := Policy{
		Attempts:  5,
		Delay:     time.Second,
		Retryable: retryTransient,
		Sleep:     SleepCtx, // the real one
		Jitter:    NoJitter,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Do(ctx, policy, func(context.Context, int) error { return errTransient })

	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Do took %v — it kept backing off after the caller had given up", elapsed)
	}
	// The reported failure is the reason we were retrying, not the sleep: the
	// interesting error is the one that started all this.
	if !errors.Is(err, errTransient) {
		t.Errorf("error = %v, want it to report the cause", err)
	}
}

// --- backoff ----------------------------------------------------------------

func TestBackoffIsExponentialAndCapped(t *testing.T) {
	base := time.Second

	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second} {
		if got := Backoff(base, attempt, nil, NoJitter); got != want {
			t.Errorf("Backoff(attempt %d) = %v, want %v", attempt, got, want)
		}
	}
	// Unbounded exponential backoff eventually waits for hours, which is
	// indistinguishable from a hang.
	if got := Backoff(base, 20, nil, NoJitter); got != MaxBackoff {
		t.Errorf("Backoff(20) = %v, want it capped at %v", got, MaxBackoff)
	}
}

// A server that is struggling knows more about its own load than our exponent does.
// Ignoring Retry-After while it asks for room is how you finish knocking it over.
func TestRetryAfterBeatsTheExponent(t *testing.T) {
	err := RetryAfterError{Err: errTransient, Delay: 7 * time.Second}

	if got := Backoff(time.Millisecond, 1, err, NoJitter); got != 7*time.Second {
		t.Errorf("Backoff = %v, want the 7s the server asked for", got)
	}
	// And it must still unwrap, or the retry policy cannot classify it.
	if !errors.Is(err, errTransient) {
		t.Error("RetryAfterError must unwrap to its cause")
	}
}

func TestRetryAfterIsUsedByDo(t *testing.T) {
	policy, slept := instant(2, retryTransient)

	_, _ = Do(context.Background(), policy, func(context.Context, int) error {
		return RetryAfterError{Err: errTransient, Delay: 3 * time.Second}
	})

	if len(*slept) != 1 || (*slept)[0] != 3*time.Second {
		t.Errorf("slept %v, want a single 3s wait as the server requested", *slept)
	}
}

// Jitter is not decoration. A fleet of handlers recovering from a restart, all
// retrying on the same exponential schedule, retries in lockstep and knocks the
// service straight back over.
func TestFullJitterActuallySpreads(t *testing.T) {
	const d = time.Second
	var sawShorter bool

	for i := 0; i < 200; i++ {
		got := FullJitter(d)
		if got < 0 || got > d {
			t.Fatalf("FullJitter produced %v, outside [0, %v]", got, d)
		}
		if got < d/2 {
			sawShorter = true
		}
	}
	if !sawShorter {
		t.Error("jitter never shortened a delay; the herd would stay synchronised")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := ParseRetryAfter("5"); got != 5*time.Second {
		t.Errorf("seconds form = %v, want 5s", got)
	}
	// http.TimeFormat, not time.RFC1123: an HTTP date ends in "GMT", and RFC1123 on
	// a UTC time renders "UTC", which http.ParseTime rejects. Real servers send GMT.
	if got := ParseRetryAfter(time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)); got <= 0 {
		t.Errorf("HTTP-date form = %v, want a positive delay", got)
	}
	// A date in the past means "come back now", not "wait a negative amount".
	if got := ParseRetryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)); got != 0 {
		t.Errorf("past date = %v, want 0", got)
	}
	if got := ParseRetryAfter("soon"); got != 0 {
		t.Errorf("garbage = %v, want 0", got)
	}
}

// --- reading responses ------------------------------------------------------

// A service that answers with a gigabyte — because it is broken, or because it is
// not the service you think you are talking to — must not be able to exhaust this
// process's memory just by replying. Once per retry.
func TestReadBoundedStopsReading(t *testing.T) {
	huge := strings.NewReader(strings.Repeat("A", 10_000))

	got, err := ReadBounded(huge, 100)
	if err != nil {
		t.Fatalf("ReadBounded: %v", err)
	}
	if len(got) != 100 {
		t.Errorf("read %d bytes, want it capped at 100", len(got))
	}
}

// An untrusted body must not be able to write a megabyte into CloudWatch on our
// behalf, once per retry, at whatever CloudWatch charges per GB.
func TestSnippetBoundsAnUntrustedBody(t *testing.T) {
	got := Snippet([]byte(strings.Repeat("A", 5000)))
	if len(got) > 300 {
		t.Errorf("snippet is %d chars — a log line is not a place for a body", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a truncated snippet should show that it was truncated")
	}
	// Newlines would forge extra log lines in a line-oriented log.
	if strings.Contains(Snippet([]byte("a\nb")), "\n") {
		t.Error("newlines must be flattened, or a body can forge log lines")
	}
	if Snippet([]byte("   ")) != "(empty body)" {
		t.Error("an empty body should say so rather than log nothing")
	}
}

// --- secrets ----------------------------------------------------------------

func TestRedactedNeverRevealsAPrefix(t *testing.T) {
	const secret = "sk-ant-super-secret-value"

	got := Redacted(secret)
	if strings.Contains(got, "sk-") || strings.Contains(got, secret) {
		t.Errorf("Redacted() = %q — a prefix of a short token is most of the token", got)
	}
	if !strings.Contains(got, "set,") {
		t.Errorf("Redacted() = %q, want it to still confirm a secret IS set", got)
	}
	if Redacted("") != "(not set)" {
		t.Error("an absent secret should be distinguishable from a present one")
	}
}
