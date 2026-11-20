package store

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"
)

// FillModelMetaIfEmpty stores a catalog estimate only when the model has no
// fetched or user-provided config. It returns the current row so a caller does
// not keep evaluating with a catalog fact that lost to a newer model edit.
func (s *Store) FillModelMetaIfEmpty(ctx context.Context, id uint, config string) (*Model, error) {
	if id == 0 {
		return nil, ErrNotFound
	}
	if strings.TrimSpace(config) == "" {
		return s.GetModel(id)
	}
	var out Model
	changed := false
	err := s.commitBusinessSchedulingDecision(ctx, func(tx *gorm.DB) error {
		if err := tx.First(&out, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if strings.TrimSpace(out.Meta) != "" {
			return nil
		}
		res := tx.Model(&out).Update("meta", config)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.First(&out, id).Error; err != nil {
			return err
		}
		if err := saveUnpublishedNotification(tx, CommittedNotification{
			RequestKey: modelNotifyKey(id), ResourceID: uintKey(id), Topic: "model", Reason: "metadata",
		}); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if changed && s.Bus != nil {
		_ = s.publishCommittedNotification(modelNotifyKey(id))
	}
	return &out, nil
}
