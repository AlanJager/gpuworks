package store

import (
	"encoding/json"
	"strings"
)

// OperationsUnset reports whether a model's stored operations declare nothing:
// an empty column or a JSON array with no element. Such a model exposes no API
// at all, so its source (a template on deploy) is free to fill it in; the two
// forms are one state because registration, a cleared edit and an old row all
// end up there. A malformed value is somebody's data and counts as set.
func OperationsUnset(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	var operations []json.RawMessage
	return json.Unmarshal([]byte(raw), &operations) == nil && len(operations) == 0
}

// operationsUnsetSQL is OperationsUnset for the operations column, spelled so
// the conditional UPDATE can test it without reading the row first. Blanks are
// stripped so a hand-written "[ ]" matches like the canonical "[]".
const operationsUnsetSQL = "REPLACE(REPLACE(REPLACE(TRIM(COALESCE(operations, '')), ' ', ''), char(10), ''), char(9), '') IN ('', '[]', 'null')"

// InitModelInferenceConfig gives a model the inference contract its source
// declares, column by column, wherever the model has none yet. A model whose
// operations are empty - never set, registered with no inferable task, or
// cleared by hand - exposes no API, so the source's declaration replaces
// them; a non-empty list is someone's decision and stays. The condition sits
// in the UPDATE itself: a concurrent edit through SetModelInferenceConfig
// that lands a non-empty value always wins. Input modalities are filled only
// from an empty column. An empty argument leaves its column alone.
func (s *Store) InitModelInferenceConfig(id uint, operations, inputModalities string) (*Model, error) {
	changed := false
	for _, column := range []struct{ name, value, unset string }{
		{"operations", operations, operationsUnsetSQL},
		{"input_modalities", inputModalities, "TRIM(COALESCE(input_modalities, '')) = ''"},
	} {
		if strings.TrimSpace(column.value) == "" {
			continue
		}
		result := s.db.Model(&Model{}).
			Where("id = ? AND "+column.unset, id).
			Update(column.name, column.value)
		if result.Error != nil {
			return nil, result.Error
		}
		changed = changed || result.RowsAffected > 0
	}
	if changed {
		s.Bus.PublishChange("model", uintKey(id), "inference_config")
	}
	return s.GetModel(id)
}
