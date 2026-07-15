package observability

import (
	"log/slog"
	"strings"
)

// redacted is what a suppressed value is replaced with. It is deliberately loud:
// a field that reads "[REDACTED]" tells the next person that the platform *chose*
// not to log this, which is a very different message from a field that is simply
// absent (did nobody set it? did it get dropped?).
const redacted = "[REDACTED]"

// sensitiveKeys are attribute names whose *values* must never reach a log group.
//
// Two kinds of thing live here, and both matter:
//
//   - Credentials — token, secret, password, an Authorization header, an API key.
//     A log group is a database that gets backed up; a token that lands in one has
//     to be treated as compromised. The whole platform already refuses to log
//     these at the source (see internal/n8n, internal/openclaw), and this is the
//     backstop for the line that forgets.
//   - Repository content — a prompt, a completion, a raw webhook payload. These
//     are not secret in the credential sense, they are *somebody's source code*,
//     and shipping it into a log group is the same mistake as shipping it to a
//     hosted model: it has left. The inference plane logs a size and a hash
//     instead (internal/llm), and this keeps a stray `log.Info("prompt", prompt)`
//     from undoing that.
//
// Matching is on a normalised key (case-insensitive, underscores and dashes
// removed), so "api_key", "API-Key" and "apikey" are one rule, not three.
var sensitiveKeys = map[string]bool{
	"token":         true,
	"secret":        true,
	"password":      true,
	"passwd":        true,
	"authorization": true,
	"apikey":        true,
	"accesskey":     true,
	"accesskeyid":   true,
	"secretkey":     true,
	"credential":    true,
	"credentials":   true,
	"cookie":        true,
	"setcookie":     true,
	"privatekey":    true,
	"prompt":        true,
	"completion":    true,
	"payload":       true,
}

// normalizeKey lower-cases a key and strips the separators people vary, so the
// rule set does not have to enumerate every spelling of the same name.
func normalizeKey(k string) string {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "_", "")
	k = strings.ReplaceAll(k, "-", "")
	return k
}

// isSensitive reports whether a key names a value that must be redacted. It
// matches both the exact normalised name and a suffix like "authtoken" or
// "githubsecret", because the sensitive part is almost always the *end* of a
// compound key.
func isSensitive(key string) bool {
	n := normalizeKey(key)
	if sensitiveKeys[n] {
		return true
	}
	for s := range sensitiveKeys {
		// A prefix like "x" on "xsecret" is not meaningful; a suffix is. "apitoken",
		// "webhooksecret", "githubapikey" all end in a sensitive word.
		if len(n) > len(s) && strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// redactAttr is the slog [slog.HandlerOptions.ReplaceAttr] hook that enforces the
// rule. It runs on every attribute of every line, so redaction is a property of
// the handler rather than a discipline asked of each caller — the only kind of
// redaction that survives contact with a hurried Tuesday.
//
// It leaves the standard fields (time, level, msg, and the correlation IDs)
// untouched — those are the whole point of the line — and replaces the *value* of
// anything whose key looks sensitive, at any depth, including inside a group.
func redactAttr(groups []string, a slog.Attr) slog.Attr {
	if isSensitive(a.Key) {
		return slog.String(a.Key, redacted)
	}
	return a
}
