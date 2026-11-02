package proxy

import (
	"bytes"
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
)

// Reading a reply's tail: the last bytes the observer kept hold the usage,
// the finish reason and the engine's per-request stats.

// lastObject finds the last non-null object under key in a reply's tail. A
// stream may repeat "usage": null in every chunk before the one that carries
// it.
func lastObject(tail []byte, key string) (map[string]json.RawMessage, bool) {
	value, _, ok := lastObjectAt(tail, key)
	return value, ok
}

// lastObjectAt is lastObject with the offset of its key in the tail.
func lastObjectAt(tail []byte, key string) (map[string]json.RawMessage, int, bool) {
	needle := []byte(`"` + key + `"`)
	end := len(tail)
	for {
		at := bytes.LastIndex(tail[:end], needle)
		if at < 0 {
			return nil, -1, false
		}
		end = at
		rest := bytes.TrimLeft(tail[at+len(needle):], " \t\r\n")
		if len(rest) == 0 || rest[0] != ':' {
			continue
		}
		rest = bytes.TrimLeft(rest[1:], " \t\r\n")
		object, ok := jsonObject(rest)
		if !ok {
			continue
		}
		var value map[string]json.RawMessage
		if json.Unmarshal(object, &value) == nil {
			return value, at, true
		}
	}
}

// lastStringField finds the last non-null string under key in a reply's
// tail ("finish_reason": "stop"), or "".
func lastStringField(tail []byte, key string) string {
	value, _ := lastStringFieldAt(tail, key)
	return value
}

// lastStringFieldAt is lastStringField with the offset of its key in the
// tail, -1 when there is none.
func lastStringFieldAt(tail []byte, key string) (string, int) {
	needle := []byte(`"` + key + `"`)
	end := len(tail)
	for {
		at := bytes.LastIndex(tail[:end], needle)
		if at < 0 {
			return "", -1
		}
		end = at
		rest := bytes.TrimLeft(tail[at+len(needle):], " \t\r\n")
		if len(rest) == 0 || rest[0] != ':' {
			continue
		}
		rest = bytes.TrimLeft(rest[1:], " \t\r\n")
		if len(rest) == 0 || rest[0] != '"' {
			continue
		}
		closing := bytes.IndexByte(rest[1:], '"')
		if closing < 0 || closing > 64 {
			continue
		}
		return string(rest[1 : 1+closing]), at
	}
}

// requestStats reads the deployment's request-stat rules out of the tail:
// each rule's first key names an object the engine wrote into the reply's
// last event (metrics, tensorfold, timings), the rest is a path inside it.
func requestStats(tail []byte, rules []inferencemetrics.RequestStatRule) map[string]float64 {
	if len(rules) == 0 || len(tail) == 0 {
		return nil
	}
	var out map[string]float64
	roots := map[string]map[string]json.RawMessage{}
	for _, rule := range rules {
		keys := rule.Keys()
		root, seen := roots[keys[0]]
		if !seen {
			root, _ = lastObject(tail, keys[0])
			roots[keys[0]] = root
		}
		if root == nil || len(keys) < 2 {
			continue
		}
		value, ok := numberAt(root, keys[1:])
		if !ok || value < 0 {
			continue
		}
		if out == nil {
			out = map[string]float64{}
		}
		out[rule.Stat] = value * rule.Factor()
	}
	return out
}

// numberAt reads the number at path inside object.
func numberAt(object map[string]json.RawMessage, path []string) (float64, bool) {
	raw, ok := object[path[0]]
	if !ok {
		return 0, false
	}
	if len(path) > 1 {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) != nil {
			return 0, false
		}
		return numberAt(nested, path[1:])
	}
	var value *float64
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return 0, false
	}
	return *value, true
}

// jsonObject returns the JSON object b starts with, or false when b does not
// start with one or the tail ends inside it.
func jsonObject(b []byte) ([]byte, bool) {
	if len(b) == 0 || b[0] != '{' {
		return nil, false
	}
	depth, inString, escaped := 0, false, false
	for i, c := range b {
		switch {
		case escaped:
			escaped = false
		case inString:
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return b[:i+1], true
			}
		}
	}
	return nil, false
}

// usageCount reads a non-negative count at path inside usage, and whether
// the path held a number at all.
func usageCount(usage map[string]json.RawMessage, path []string) (uint64, bool) {
	value, ok := numberAt(usage, path)
	if !ok || value < 0 {
		return 0, false
	}
	return uint64(value), true
}
