package dto

import (
	"bytes"
	"encoding/json"
)

// NullableField distinguishes an omitted patch field from explicit inheritance
// (null) and a concrete value. It is only used for decoding patch requests.
type NullableField[T any] struct {
	Present bool
	Value   *T
}

func (f *NullableField[T]) UnmarshalJSON(data []byte) error {
	f.Present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		f.Value = nil
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	f.Value = &value
	return nil
}

// StorageNodeOverridesRequest updates a fixed node set, leaving omitted fields
// intact. Explicit null restores the storage's default for that field.
type StorageNodeOverridesRequest struct {
	NodeUUIDs     []string              `json:"node_uuids"`
	CachePath     NullableField[string] `json:"cache_path"`
	CacheMaxBytes NullableField[int64]  `json:"cache_max_bytes"`
}
