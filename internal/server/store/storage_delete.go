package store

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// DeleteStorage removes the storage, its registrations and the models selected
// by the caller in one transaction. Physical files are left alone. Omitting
// modelIDs keeps model definitions, as storage deletion previously did.
func (s *Store) DeleteStorage(id uint, modelIDs ...uint) error {
	modelIDs = sortedUnique(modelIDs)
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var row Storage
		if err := tx.First(&row, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if row.IsDefault {
			return ErrStorageIsDefault
		}
		if err := checkStorageIdle(tx, id, true); err != nil {
			return err
		}
		if len(modelIDs) > 0 {
			var users []uint
			if err := tx.Model(&Deployment{}).Where("model_id IN ?", modelIDs).Order("id").Pluck("id", &users).Error; err != nil {
				return err
			}
			if len(users) > 0 {
				return &StorageInUseError{DeploymentIDs: users}
			}
			if err := checkNoCacheDeletion(tx.Where("model_id IN ?", modelIDs)); err != nil {
				return err
			}
		}
		// A deployment that saved into the storage uses the default order now.
		if err := tx.Model(&Deployment{}).Where("weight_storage_id = ?", id).Update("weight_storage_id", 0).Error; err != nil {
			return err
		}
		for _, entity := range []any{&ModelFile{}, &StorageNode{}, &StorageNodeState{}, &StorageCacheOperation{}} {
			if err := tx.Where("storage_id = ?", id).Delete(entity).Error; err != nil {
				return err
			}
		}
		if len(modelIDs) > 0 {
			for _, entity := range []any{&ModelFile{}, &Profile{}} {
				if err := tx.Where("model_id IN ?", modelIDs).Delete(entity).Error; err != nil {
					return err
				}
			}
			result := tx.Where("id IN ?", modelIDs).Delete(&Model{})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != int64(len(modelIDs)) {
				return fmt.Errorf("model cleanup deleted %d rows, expected %d", result.RowsAffected, len(modelIDs))
			}
		}
		return tx.Delete(&Storage{}, id).Error
	})
	if err != nil {
		return err
	}
	s.publishStorage(id, "deleted")
	for _, modelID := range modelIDs {
		s.Bus.PublishChange("model", uintKey(modelID), "deleted")
	}
	return nil
}
