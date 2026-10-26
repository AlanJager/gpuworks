package loadgen

import (
	"encoding/json"
	"strings"
)

// ResponseErrorText unwraps OpenAI/gateway error envelopes before redaction.
// Decoding can turn JSON-escaped URL delimiters and credentials into text, so
// callers must never decode an envelope after its final redaction boundary.
func ResponseErrorText(text string, knownSecrets ...string) string {
	if start := strings.Index(text, "{"); start >= 0 {
		var envelope struct {
			Error   json.RawMessage `json:"error"`
			Message string          `json:"message"`
		}
		if json.Unmarshal([]byte(text[start:]), &envelope) == nil {
			var nested struct {
				Message string `json:"message"`
			}
			var plain string
			switch {
			case json.Unmarshal(envelope.Error, &nested) == nil && nested.Message != "":
				text = nested.Message
			case envelope.Message != "":
				text = envelope.Message
			case json.Unmarshal(envelope.Error, &plain) == nil && plain != "":
				text = plain
			}
		}
	}
	return RedactErrorText(strings.ReplaceAll(text, `\/`, `/`), knownSecrets...)
}
