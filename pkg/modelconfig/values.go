package modelconfig

import (
	"encoding/json"
	"strings"
)

// intOf returns the first key present with an integer-representable value.
// Lists, objects, booleans and non-numeric strings are ignored.
func intOf(m map[string]any, keys ...string) int {
	for _, key := range keys {
		switch value := m[key].(type) {
		case float64:
			return int(value)
		case int:
			return value
		case int64:
			return int(value)
		case json.Number:
			if parsed, err := value.Int64(); err == nil {
				return int(parsed)
			}
		case string:
			// A few repositories quote their numbers.
			if parsed, err := json.Number(strings.TrimSpace(value)).Int64(); err == nil {
				return int(parsed)
			}
		}
	}
	return 0
}

func stringOf(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func intList(m map[string]any, keys ...string) []int {
	for _, key := range keys {
		raw, ok := m[key].([]any)
		if !ok || len(raw) == 0 {
			continue
		}
		out := make([]int, 0, len(raw))
		for _, item := range raw {
			switch value := item.(type) {
			case float64:
				out = append(out, int(value))
			case int:
				out = append(out, value)
			case json.Number:
				if parsed, err := value.Int64(); err == nil {
					out = append(out, int(parsed))
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func stringList(m map[string]any, keys ...string) []string {
	for _, key := range keys {
		raw, ok := m[key].([]any)
		if !ok || len(raw) == 0 {
			continue
		}
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if value, ok := item.(string); ok {
				out = append(out, strings.TrimSpace(value))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func mapOf(m map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := m[key].(map[string]any); ok {
			return value
		}
	}
	return nil
}

func boolOf(m map[string]any, keys ...string) *bool {
	for _, key := range keys {
		if value, ok := m[key].(bool); ok {
			flag := value
			return &flag
		}
	}
	return nil
}
