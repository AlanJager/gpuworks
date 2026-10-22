package modelconfig

import (
	"bytes"
)

// replaceNonFinite rewrites the bare Infinity / -Infinity / NaN literals that
// JSON numbers cannot express as null, leaving string contents untouched. It
// reports whether anything changed, so a genuinely malformed document keeps its
// original error.
func replaceNonFinite(body []byte) ([]byte, bool) {
	out := make([]byte, 0, len(body))
	changed := false
	inString, escaped := false, false
	for i := 0; i < len(body); {
		char := body[i]
		if inString {
			out = append(out, char)
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			i++
			continue
		}
		if char == '"' {
			inString = true
			out = append(out, char)
			i++
			continue
		}
		if length, ok := nonFiniteAt(body[i:]); ok {
			out = append(out, "null"...)
			i += length
			changed = true
			continue
		}
		out = append(out, char)
		i++
	}
	return out, changed
}

// nonFiniteAt reports the length of a non-finite literal at the start of body.
// Outside a string a bare token can only be one of these: JSON itself spells
// every other unquoted value as a number, true, false or null.
func nonFiniteAt(body []byte) (int, bool) {
	for _, literal := range []string{"-Infinity", "Infinity", "NaN"} {
		if bytes.HasPrefix(body, []byte(literal)) {
			return len(literal), true
		}
	}
	return 0, false
}
