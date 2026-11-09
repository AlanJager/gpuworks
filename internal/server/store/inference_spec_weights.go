package store

import (
	"errors"
	"strings"

	"gorm.io/gorm"
)

// InferenceSpecWeightSources reads the weight sources one spec declares, by
// its identity (template, source, name), without loading the template's other
// specs. The JSON is inferencecatalog.WeightSource values.
func (s *Store) InferenceSpecWeightSources(templateID uint, source, name string) (string, error) {
	var row InferenceSpecRow
	err := s.db.Select("weight_sources").
		Where("model_id = ? AND source = ? AND name = ?", templateID, strings.TrimSpace(source), strings.TrimSpace(name)).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrNotFound
	}
	return row.WeightSources, err
}
