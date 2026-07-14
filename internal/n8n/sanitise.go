package n8n

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/workflow"
)

// Keys whose values never leave this process. The list is matched case- and
// separator-insensitively, so "access_token", "accessToken" and "ACCESS-TOKEN"
// all match "accesstoken".
//
// # Why sanitise a payload we did not write and are only passing on?
//
// Because "we are only passing it on" is exactly how secrets travel. A GitHub
// webhook payload is large, nested, versioned by someone else, and occasionally
// carries credential-shaped things — an installation token on an App event, a
// client secret in a poorly-configured integration, a URL with a token in its
// query string. Forwarding it verbatim means those land in n8n's execution
// history, which is a database, which gets backed up, and which anyone with n8n
// UI access can read.
//
// The platform gains nothing by forwarding them, and it is the platform's
// responsibility not to widen the blast radius of someone else's mistake. So we
// forward the payload — a workflow genuinely needs it — with the credential-shaped
// fields replaced.
var sensitiveKeys = map[string]bool{
	"token":            true,
	"accesstoken":      true,
	"refreshtoken":     true,
	"idtoken":          true,
	"apikey":           true,
	"apisecret":        true,
	"secret":           true,
	"clientsecret":     true,
	"password":         true,
	"passwd":           true,
	"authorization":    true,
	"auth":             true,
	"credentials":      true,
	"privatekey":       true,
	"sessionid":        true,
	"cookie":           true,
	"setcookie":        true,
	"signature":        true,
	"xhubsignature":    true,
	"xhubsignature256": true,
	"encryptedvalue":   true,
	"connectionstring": true,
}

// redactedValue is what a sensitive value becomes. It is deliberately obvious in
// a log or an n8n execution: someone reading it should immediately understand
// that a value was removed on purpose, rather than wondering why a field is oddly
// empty.
const redactedValue = "[REDACTED BY PLATFORM]"

// sanitisePayload prepares an untrusted event payload for forwarding: it enforces
// a size cap and redacts credential-shaped fields at any depth.
//
// A payload that is not valid JSON is rejected rather than forwarded blind — if
// we cannot parse it, we cannot sanitise it, and forwarding something unreadable
// and unsanitised is the worst of both.
func sanitisePayload(raw json.RawMessage, maxBytes int64) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if int64(len(raw)) > maxBytes {
		// Truncating JSON produces invalid JSON, and silently dropping a payload
		// produces a workflow that mysteriously sees nothing. Refuse, loudly: the
		// caller can decide to send the metadata without the payload.
		return nil, fmt.Errorf("%w: event payload is %d bytes, over the %d byte limit (%s)",
			workflow.ErrInvalidRequest, len(raw), maxBytes, EnvMaxPayloadBytes)
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("%w: event payload is not valid JSON: %v", workflow.ErrInvalidRequest, err)
	}

	cleaned, err := json.Marshal(redact(decoded))
	if err != nil {
		return nil, fmt.Errorf("%w: re-encoding sanitised payload: %v", workflow.ErrInvalidRequest, err)
	}
	return cleaned, nil
}

// redact walks the decoded JSON and replaces the values of sensitive keys,
// wherever they are nested.
//
// It replaces the value rather than deleting the key on purpose: a workflow that
// reads a field it expects to exist should find a clear marker, not a nil that it
// will misinterpret as "there was no token here".
func redact(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, inner := range value {
			if isSensitive(key) {
				out[key] = redactedValue
				continue
			}
			out[key] = redact(inner)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, inner := range value {
			out[i] = redact(inner)
		}
		return out
	default:
		return value
	}
}

// isSensitive normalises a key and checks it against the list. It also catches
// compound names — "github_token", "installationAccessToken" — by suffix, because
// the real world does not restrict itself to the exact word.
func isSensitive(key string) bool {
	normalised := strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', ' ', '.':
			return -1
		}
		return r
	}, strings.ToLower(key))

	if sensitiveKeys[normalised] {
		return true
	}
	for candidate := range sensitiveKeys {
		// Suffix, not substring: "tokenizer" and "secretary" are not secrets, but
		// "github_token" and "client_secret" are.
		if len(normalised) > len(candidate) && strings.HasSuffix(normalised, candidate) {
			return true
		}
	}
	return false
}
