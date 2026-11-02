package store

import (
	"errors"
	"fmt"

	sqlite "github.com/glebarez/go-sqlite"
	"gorm.io/gorm"
	sqlite3 "modernc.org/sqlite/lib"
)

type ModelFilePathOccupiedError struct {
	StorageID uint
	Path      string
	ModelID   uint
}

func (e *ModelFilePathOccupiedError) Error() string {
	return fmt.Sprintf("shared storage %d path %q is occupied by model %d", e.StorageID, e.Path, e.ModelID)
}

func sharedModelPathConflict(db *gorm.DB, ref FileRef, path string) error {
	if ref.NodeID != 0 {
		return nil
	}
	var owner ModelFile
	err := db.Select("model_id").Where("storage_id = ? AND node_id = 0 AND path = ? AND model_id <> ?", ref.StorageID, path, ref.ModelID).First(&owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return &ModelFilePathOccupiedError{StorageID: ref.StorageID, Path: path, ModelID: owner.ModelID}
}

func modelFilePathWriteError(db *gorm.DB, ref FileRef, path string, err error) error {
	var constraint *sqlite.Error
	if errors.As(err, &constraint) && constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		var occupied *ModelFilePathOccupiedError
		if conflict := sharedModelPathConflict(db, ref, path); errors.As(conflict, &occupied) {
			return occupied
		}
	}
	return err
}
