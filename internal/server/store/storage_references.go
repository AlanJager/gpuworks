package store

import (
	"slices"

	"gorm.io/gorm"
)

// StorageModelReferences contains persisted sources and references; callers
// decide which models should survive storage removal.
type StorageModelReferences struct {
	ModelIDs      []uint
	OtherModelIDs []uint
	ModelUsers    []Deployment
	DirectUsers   StorageInUseError
}

func (s *Store) StorageModelReferences(storageID uint, excludedStates []string) (StorageModelReferences, error) {
	db := s.reader()
	var refs StorageModelReferences
	target := db.Model(&ModelFile{}).Where("storage_id = ?", storageID).Distinct("model_id")
	if err := target.Order("model_id").Pluck("model_id", &refs.ModelIDs).Error; err != nil {
		return refs, err
	}
	if len(refs.ModelIDs) > 0 {
		if err := storageModelsWithOtherSources(db, storageID, excludedStates).
			Where("id IN ?", refs.ModelIDs).Order("id").Pluck("id", &refs.OtherModelIDs).Error; err != nil {
			return refs, err
		}
		if err := db.Select("id", "model_id").Where("model_id IN ?", refs.ModelIDs).Find(&refs.ModelUsers).Error; err != nil {
			return refs, err
		}
	}
	var err error
	refs.DirectUsers, err = storageDirectUsers(db, storageID)
	return refs, err
}

// StorageUsers reads only service references, without collecting every model
// on the storage. The caller supplies the same source-state policy as deletion.
func (s *Store) StorageUsers(storageID uint, excludedStates []string) (StorageInUseError, error) {
	db := s.reader()
	users, err := storageDirectUsers(db, storageID)
	if err != nil {
		return users, err
	}
	target := db.Model(&ModelFile{}).Select("1").
		Where("model_id = deployments.model_id AND storage_id = ?", storageID)
	other := storageModelsWithOtherSources(db, storageID, excludedStates).
		Select("1").Where("models.id = deployments.model_id")
	var modelUsers []uint
	if err := db.Model(&Deployment{}).Where("EXISTS (?) AND NOT EXISTS (?)", target, other).
		Pluck("id", &modelUsers).Error; err != nil {
		return users, err
	}
	users.DeploymentIDs = sortedUnique(append(users.DeploymentIDs, modelUsers...))
	return users, nil
}

func storageModelsWithOtherSources(db *gorm.DB, storageID uint, excludedStates []string) *gorm.DB {
	other := db.Model(&ModelFile{}).Select("1").
		Where("model_id = models.id").
		Where("COALESCE(state, '') NOT IN ?", excludedStates).Session(&gorm.Session{})
	// Two index ranges skip the target storage's per-node replicas entirely.
	return db.Model(&Model{}).Where("EXISTS (?) OR EXISTS (?)",
		other.Where("storage_id < ?", storageID), other.Where("storage_id > ?", storageID))
}

func storageDirectUsers(db *gorm.DB, storageID uint) (StorageInUseError, error) {
	var users StorageInUseError
	var readers []struct {
		DeploymentID uint
		UUID         string
	}
	if err := db.Table("instance_model_files").
		Joins("JOIN instances ON instances.id = instance_model_files.instance_id").
		Joins("JOIN model_files ON model_files.id = instance_model_files.model_file_id").
		Where("model_files.storage_id = ?", storageID).
		Distinct("instances.deployment_id", "instances.uuid").
		Order("instances.deployment_id, instances.uuid").Scan(&readers).Error; err != nil {
		return users, err
	}
	for _, reader := range readers {
		users.DeploymentIDs = append(users.DeploymentIDs, reader.DeploymentID)
		users.InstanceUUIDs = append(users.InstanceUUIDs, reader.UUID)
	}
	var err error
	users.DeploymentIDs, err = deploymentsUsingStorage(db, storageID, users.DeploymentIDs)
	slices.Sort(users.InstanceUUIDs)
	users.InstanceUUIDs = slices.Compact(users.InstanceUUIDs)
	return users, err
}
