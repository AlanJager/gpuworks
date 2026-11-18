package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	sqlite "github.com/glebarez/go-sqlite"
	"gorm.io/gorm"
	sqlite3 "modernc.org/sqlite/lib"

	"gpuworks.ai/gpuworks/internal/proto"
)

// EnsureModelAcquisition creates the initial automatic acquisition only when
// absent. A concurrent API request may already own a transfer by the time a
// deployment's earlier source snapshot reaches this transaction.
func (s *Store) EnsureModelAcquisition(ref FileRef, path string, deploymentID uint) (*ModelFile, error) {
	ref = s.resolveRef(ref)
	var row ModelFile
	created := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		err := refQuery(tx, ref).First(&row).Error
		if err == nil {
			if row.State == proto.MFDeleting {
				return ErrCacheDeletionPending
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		path, err = localModelTransferPath(tx, ref, path)
		if err != nil {
			return err
		}
		if err := sharedModelPathConflict(tx, ref, path); err != nil {
			return err
		}
		if err := checkNodeRowAllowed(tx, ref); err != nil {
			return err
		}
		row = ModelFile{ModelID: ref.ModelID, StorageID: ref.StorageID, NodeID: ref.NodeID,
			Path: path, State: proto.MFPending, Acquisition: proto.ModelAcquireAuto, DownloadDeploymentID: deploymentID}
		if err := tx.Create(&row).Error; err != nil {
			return modelFilePathWriteError(tx, ref, path, err)
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if created {
		s.publishModelFile(row.ID, "created")
	}
	return &row, nil
}

// localModelTransferPath keeps automatically written copies of different
// model identities apart without moving existing default directories. Agent
// reports turn relative paths into absolute ones, so compare their relative
// suffix too; another storage or node remains a different location.
func localModelTransferPath(db *gorm.DB, ref FileRef, requested string) (string, error) {
	if ref.NodeID == 0 || requested == "" {
		return requested, nil
	}
	var rows []ModelFile
	if err := db.Select("model_id", "path").Where("storage_id = ? AND node_id = ?", ref.StorageID, ref.NodeID).Find(&rows).Error; err != nil {
		return "", err
	}
	suffix := fmt.Sprintf("-model-%d", ref.ModelID)
	fallback := requested
	if !strings.HasSuffix(requested, suffix) {
		fallback += suffix
	}
	chosen := requested
	for _, row := range rows {
		if row.ModelID == ref.ModelID && sameLocalModelPath(row.Path, fallback) {
			chosen = fallback // Keep the allocated directory even after its old neighbour is deleted.
		}
	}
	owner := func(target string) *ModelFile {
		for i := range rows {
			if rows[i].ModelID != ref.ModelID && sameLocalModelPath(rows[i].Path, target) {
				return &rows[i]
			}
		}
		return nil
	}
	if chosen == requested && owner(requested) == nil {
		return requested, nil
	}
	if occupied := owner(fallback); occupied != nil {
		return "", &ModelFilePathOccupiedError{StorageID: ref.StorageID, Path: fallback, ModelID: occupied.ModelID}
	}
	return fallback, nil
}

func sameLocalModelPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	if filepath.IsAbs(a) && !filepath.IsAbs(b) {
		return strings.HasSuffix(a, string(filepath.Separator)+b)
	}
	if filepath.IsAbs(b) && !filepath.IsAbs(a) {
		return strings.HasSuffix(b, string(filepath.Separator)+a)
	}
	return false
}

type ModelFilePathOccupiedError struct {
	StorageID uint
	Path      string
	ModelID   uint
}

func (e *ModelFilePathOccupiedError) Error() string {
	return fmt.Sprintf("storage %d path %q is occupied by model %d", e.StorageID, e.Path, e.ModelID)
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
