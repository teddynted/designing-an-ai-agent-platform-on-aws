package webhook

import (
	"errors"
	"strings"
	"testing"
)

// Signature verification is the security boundary of the whole platform's front door,
// so it gets the most careful test in the package. The happy path is one line; the
// value is in the ways it must say no.

func TestVerifySignature(t *testing.T) {
	secret := "it's a secret to everybody"
	body := []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)
	good := Sign(body, secret)

	tests := []struct {
		name   string
		body   []byte
		header string
		secret string
		want   error
	}{
		{"a correct signature verifies", body, good, secret, nil},
		{"no secret is a configuration error", body, good, "", ErrNoSecret},
		{"no signature header is its own error", body, "", secret, ErrNoSignature},
		{"the wrong secret is refused", body, Sign(body, "wrong"), secret, ErrBadSignature},
		{"a tampered body is refused", []byte(`{"zen":"evil"}`), good, secret, ErrBadSignature},
		{"a signature without the sha256= prefix is refused", body, strings.TrimPrefix(good, "sha256="), secret, ErrBadSignature},
		{"a non-hex signature is refused", body, "sha256=nothexatall", secret, ErrBadSignature},
		{"the sha1 header is not accepted as sha256", body, "sha1=" + strings.TrimPrefix(good, "sha256="), secret, ErrBadSignature},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifySignature(tc.body, tc.header, tc.secret)
			if !errors.Is(err, tc.want) {
				t.Errorf("VerifySignature = %v, want %v", err, tc.want)
			}
		})
	}
}

// The signature is over the EXACT bytes, so a re-encoded body — same JSON, different
// whitespace — must NOT verify. This is why the handler verifies the raw body before
// parsing, and the test that would catch anyone "helpfully" verifying a re-marshalled
// payload.
func TestSignatureIsOverRawBytesNotSemanticJSON(t *testing.T) {
	secret := "s"
	compact := []byte(`{"a":1,"b":2}`)
	spaced := []byte(`{ "a": 1, "b": 2 }`) // same meaning, different bytes

	sig := Sign(compact, secret)

	if err := VerifySignature(compact, sig, secret); err != nil {
		t.Fatalf("the exact bytes must verify: %v", err)
	}
	if err := VerifySignature(spaced, sig, secret); !errors.Is(err, ErrBadSignature) {
		t.Error("a re-encoded body must NOT verify — the signature is over bytes, not meaning")
	}
}

// Sign and VerifySignature are exact inverses, over any input. If they ever diverge,
// every real GitHub delivery would be rejected, so this pins the round trip on a range
// of bodies including the empty one.
func TestSignRoundTrips(t *testing.T) {
	secret := "round-trip-secret"
	for _, body := range [][]byte{{}, []byte("x"), []byte(`{"nested":{"deeply":[1,2,3]}}`)} {
		if err := VerifySignature(body, Sign(body, secret), secret); err != nil {
			t.Errorf("round trip failed for %q: %v", body, err)
		}
	}
}
