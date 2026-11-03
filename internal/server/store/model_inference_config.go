package store

import (
	"encoding/json"
	"errors"
	"strings"

	"gorm.io/gorm"
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
	return s.initModelInferenceConfig(id, operations, inputModalities, false)
}

// InitTemplateModelInferenceConfig also replaces a bare chat or video contract
// nobody chose (IsDefaultTemplateOperations) with the one the template declares.
func (s *Store) InitTemplateModelInferenceConfig(id uint, operations, inputModalities string) (*Model, error) {
	return s.initModelInferenceConfig(id, operations, inputModalities, true)
}

func (s *Store) initModelInferenceConfig(id uint, operations, inputModalities string, repairLegacyOperations bool) (*Model, error) {
	changed, err := updateModelInferenceConfig(s.db, id, operations, inputModalities, repairLegacyOperations)
	if err != nil {
		return nil, err
	}
	if changed {
		s.Bus.PublishChange("model", uintKey(id), "inference_config")
	}
	return s.GetModel(id)
}

// updateModelInferenceConfig performs the conditional writes on tx. A template
// placeholder is replaced only under the exact bytes that identified it, so an
// edit that lands after that read still wins. No event is published here: the
// caller owns the transaction boundary and publishes after its commit.
func updateModelInferenceConfig(tx *gorm.DB, id uint, operations, inputModalities string, repairLegacyOperations bool) (bool, error) {
	replaced, replaceLegacy, err := legacyOperationsToReplace(tx, id, repairLegacyOperations)
	if err != nil {
		return false, err
	}
	changed := false
	for _, column := range []struct{ name, value, unset string }{
		{"operations", operations, operationsUnsetSQL},
		{"input_modalities", inputModalities, "TRIM(COALESCE(input_modalities, '')) = ''"},
	} {
		if strings.TrimSpace(column.value) == "" {
			continue
		}
		query := tx.Model(&Model{}).Where("id = ?", id)
		if column.name == "operations" && replaceLegacy {
			query = query.Where("operations = ?", replaced)
		} else {
			query = query.Where(column.unset)
		}
		result := query.Update(column.name, column.value)
		if result.Error != nil {
			return false, result.Error
		}
		changed = changed || result.RowsAffected > 0
	}
	return changed, nil
}

// legacyOperationsToReplace returns the stored operations value when it is the
// inferred chat/video placeholder and this caller is allowed to replace it.
// Reading through the caller's transaction also makes a disappeared model fail
// the surrounding create rather than leaving a deployment without its model.
func legacyOperationsToReplace(tx *gorm.DB, id uint, repair bool) (string, bool, error) {
	if !repair {
		return "", false, nil
	}
	var stored Model
	if err := tx.Select("operations").First(&stored, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, ErrNotFound
	} else if err != nil {
		return "", false, err
	}
	if !IsDefaultTemplateOperations(stored.Operations) {
		return "", false, nil
	}
	return stored.Operations, true, nil
}
