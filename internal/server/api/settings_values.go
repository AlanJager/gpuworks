package api

import (
	"encoding/json"
	"strings"
)

// Settings are JSON documents because the settings API returns raw JSON.
// Accept an unquoted value as well for databases created by older releases.
func stringSettingValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var value string
	if json.Unmarshal([]byte(raw), &value) == nil {
		return strings.TrimSpace(value)
	}
	return raw
}
