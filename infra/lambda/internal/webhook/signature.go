// Package webhook is the platform's GitHub webhook entry point: the Lambda that
// GitHub calls when something happens in a repository, and the first — and only —
// place an inbound request is trusted or refused.
//
// # What this Lambda does, and the one thing it must not
//
// It verifies the request is really from GitHub, parses it, decides whether the
// platform cares, and — if it does — publishes a small, curated event onto the
// platform's EventBridge bus. Then it returns. That is the whole job.
//
// What it must NOT do is any of the WORK. It does not call n8n, it does not start
// an agent, it does not reach a model. GitHub gives a webhook ten seconds to
// answer and retries the ones that time out, so a webhook that blocked on an agent
// run — minutes to hours (see the agent package in the main module) — would time
// out, be retried, and start the work twice. The webhook's answer to "something
// happened" is to put a durable event on a bus and get out of the way; everything
// downstream reads that bus on its own schedule. This is the same submit-fast,
// process-later shape the whole platform is built on, applied at its front door.
//
// # Why the pieces are separated the way they are
//
// Signature verification, parsing, and filtering are three different decisions with
// three different failure modes, and they are three files so each can be wrong (and
// tested) on its own. The handler ([Handler.Handle]) sequences them; nothing else
// does. And every AWS dependency is behind an interface ([EventsAPI]), so the whole
// of Handle runs in a unit test with no AWS account — the same discipline the spot
// package in this module uses, for the same reason.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// SignatureHeader is the header GitHub signs the payload with. There is also an
// older X-Hub-Signature (SHA-1); this platform ignores it entirely. SHA-1 is broken,
// GitHub sends both only for backwards compatibility, and accepting the weaker of two
// signatures is how you get the security of the weaker one.
const SignatureHeader = "X-Hub-Signature-256"

// EventHeader names the event type — "push", "release", "ping".
const EventHeader = "X-GitHub-Event"

// DeliveryHeader is GitHub's unique id for this delivery. It is the thread the whole
// platform's correlation chain hangs from: it becomes the correlation id, which
// becomes the agent's idempotency key, which is what stops a redelivered webhook from
// opening two pull requests. See [Delivery.CorrelationID].
const DeliveryHeader = "X-GitHub-Delivery"

// signaturePrefix is what GitHub puts in front of the hex digest.
const signaturePrefix = "sha256="

// ErrNoSignature means the request carried no signature header at all. It is kept
// distinct from ErrBadSignature because it usually means something different: a
// misconfigured webhook (no secret set in GitHub), or a probe from someone who does
// not know the endpoint expects a signature — as opposed to a forgery, which arrives
// WITH a signature that does not check out.
var ErrNoSignature = errors.New("no signature header")

// ErrBadSignature means a signature was present and wrong. This is the security
// event: either the shared secret is misconfigured, or someone is forging requests.
// Either way the request is refused and nothing downstream ever sees it.
var ErrBadSignature = errors.New("signature verification failed")

// ErrNoSecret means the handler has no secret to verify against. It is a
// configuration error, not a request error, and it is fatal: a webhook endpoint that
// cannot verify signatures must refuse EVERY request rather than accept them
// unverified, because an endpoint that fails open is worse than one that is down.
var ErrNoSecret = errors.New("no webhook secret configured")

// VerifySignature checks that body was signed with secret, using the value of the
// X-Hub-Signature-256 header.
//
// # The three things this gets right, because getting them wrong is the whole ballgame
//
//  1. It verifies over the RAW body — the exact bytes GitHub sent — not a re-encoded
//     JSON. Re-marshalling changes whitespace and key order, and the signature is over
//     bytes, so a parse-then-verify would reject every legitimate request. This is why
//     the handler verifies before it parses, and never the other way round.
//
//  2. It compares in CONSTANT TIME, via hmac.Equal. A byte-by-byte compare that
//     returns early leaks, through timing, how much of a guess was correct, and that
//     is enough to forge a signature one byte at a time. This is the single most
//     important line in the package, and it is a library call precisely so nobody
//     reimplements it with `==`.
//
//  3. It recomputes the HMAC itself rather than trusting any field in the request.
//     The only inputs are the body and the secret; nothing the attacker controls
//     selects the algorithm or the key.
func VerifySignature(body []byte, header, secret string) error {
	if secret == "" {
		return ErrNoSecret
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return ErrNoSignature
	}

	// The header must be "sha256=<hex>". Reject anything else rather than trying to be
	// lenient: a signature in an unexpected shape is not a signature to trust.
	if !strings.HasPrefix(header, signaturePrefix) {
		return ErrBadSignature
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil {
		return ErrBadSignature
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)

	// hmac.Equal is constant time. Do not replace it with bytes.Equal or ==.
	if !hmac.Equal(got, want) {
		return ErrBadSignature
	}
	return nil
}

// Sign produces the header value GitHub would send for a body and secret. It exists
// for tests — which need to sign sample payloads the way GitHub does — and for
// nothing else. It is the exact inverse of what [VerifySignature] recomputes, so a
// test that signs with Sign and verifies with VerifySignature is testing the real
// path, not a parallel one.
func Sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}
