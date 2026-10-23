package store

import "slices"

// StorageModelReferences contains persisted sources and references; callers
// decide which models should survive storage removal.
type StorageModelReferences struct {
	ModelIDs    []uint
	OtherFiles  []ModelFile
	ModelUsers  []Deployment
	DirectUsers StorageInUseError
}

func (s *Store) StorageModelReferences(storageID uint) (StorageModelReferences, error) {
	db := s.reader()
	var refs StorageModelReferences
	target := db.Model(&ModelFile{}).Where("storage_id = ?", storageID).Distinct("model_id")
	if err := target.Order("model_id").Pluck("model_id", &refs.ModelIDs).Error; err != nil {
		return refs, err
	}
	if len(refs.ModelIDs) > 0 {
		if err := db.Model(&ModelFile{}).Select("DISTINCT model_id, state").
			Where("storage_id <> ? AND model_id IN (?)", storageID, target.Select("model_id")).Find(&refs.OtherFiles).Error; err != nil {
			return refs, err
		}
		if err := db.Select("id", "model_id").Where("model_id IN ?", refs.ModelIDs).Find(&refs.ModelUsers).Error; err != nil {
			return refs, err
		}
	}
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
		return refs, err
	}
	for _, reader := range readers {
		refs.DirectUsers.DeploymentIDs = append(refs.DirectUsers.DeploymentIDs, reader.DeploymentID)
		refs.DirectUsers.InstanceUUIDs = append(refs.DirectUsers.InstanceUUIDs, reader.UUID)
	}
	var err error
	refs.DirectUsers.DeploymentIDs, err = deploymentsUsingStorage(db, storageID, refs.DirectUsers.DeploymentIDs)
	slices.Sort(refs.DirectUsers.InstanceUUIDs)
	refs.DirectUsers.InstanceUUIDs = slices.Compact(refs.DirectUsers.InstanceUUIDs)
	return refs, err
}
