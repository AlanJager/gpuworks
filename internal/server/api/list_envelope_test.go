package api

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// unmarshalList decodes a collection response, which must be exactly the
// dto.List envelope {"items": [...], "total": N} with N = len(items), into
// out. Tests use it where they used to decode a bare array, so every such
// test also pins the envelope (docs/reference/api-conventions.md).
func unmarshalList[T any](body []byte, out *[]T) error {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("list envelope: %w (body %s)", err, body)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "items,total" {
		return fmt.Errorf("list envelope keys = %v, want [items total] (body %s)", keys, body)
	}
	if strings.TrimSpace(string(env["items"])) == "null" {
		return fmt.Errorf("list envelope items is null, want an array (body %s)", body)
	}
	if err := json.Unmarshal(env["items"], out); err != nil {
		return err
	}
	var total int
	if err := json.Unmarshal(env["total"], &total); err != nil {
		return fmt.Errorf("list envelope total: %w", err)
	}
	if total != len(*out) {
		return fmt.Errorf("list envelope total = %d, want len(items) = %d", total, len(*out))
	}
	return nil
}

// unmarshalCursorPage decodes a cursor-paged response, which must be exactly
// {"items": [...]} (dto.CursorPage, no total), into out.
func unmarshalCursorPage[T any](body []byte, out *[]T) error {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("cursor page: %w (body %s)", err, body)
	}
	if len(env) != 1 || env["items"] == nil || strings.TrimSpace(string(env["items"])) == "null" {
		return fmt.Errorf("cursor page = %s, want exactly {\"items\": [...]}", body)
	}
	return json.Unmarshal(env["items"], out)
}
