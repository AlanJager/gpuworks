package diagnostics

import (
	"encoding/json"
	"regexp"
	"strings"
)

// sensitiveKey matches the names of fields and settings that hold a
// credential: tokens, secrets, passwords, keys, authorizations, and the hashes
// stored for them.
var sensitiveKey = regexp.MustCompile(`(?i)(token|secret|passw|credential|private_?key|api_?key|access_?key|secret_?key|auth|cookie|hash|signature|sealed)`)

// RedactedPrefix is how many leading characters of a credential a
// collection keeps: as many as a node token's log prefix, enough to match an
// API key or token with the prefix the console lists for it.
const RedactedPrefix = 8

// redactValue returns a credential cut to its first RedactedPrefix characters.
func redactValue(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= RedactedPrefix {
		return "***"
	}
	return string(runes[:RedactedPrefix]) + "***"
}

// Redact is v as JSON with every credential-named field cut to its prefix.
// A string field holding JSON (a setting's value) is redacted inside too.
func Redact(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return redactWalk(generic, false), nil
}

func redactWalk(v any, sensitive bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = redactWalk(child, sensitive || sensitiveKey.MatchString(k))
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = redactWalk(child, sensitive)
		}
		return t
	case string:
		if trimmed := strings.TrimSpace(t); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var inner any
			if json.Unmarshal([]byte(trimmed), &inner) == nil {
				out, _ := json.Marshal(redactWalk(inner, sensitive))
				return string(out)
			}
		}
		if sensitive {
			return redactValue(t)
		}
		return t
	default:
		return v
	}
}
