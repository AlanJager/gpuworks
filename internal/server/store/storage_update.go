package store

import "gorm.io/gorm"

// checkStorageReaderTargets runs in the update transaction, after replacing
// targets, so it compares the normalized, deduplicated targets actually saved.
// A rejected change rolls the entire patch back, including other fields.
func checkStorageReaderTargets(tx *gorm.DB, before, after Storage, previousTargets []StorageNode) error {
	var targets []StorageNode
	if err := tx.Where("storage_id = ?", before.ID).Order("id asc").Find(&targets).Error; err != nil {
		return err
	}
	if err := checkCacheDeletionTargets(tx, before, after, previousTargets, targets); err != nil {
		return err
	}
	var nodes []Node
	readers := liveReads(tx).Where("model_files.storage_id = ?", before.ID).Select("instances.node_id")
	if err := tx.Where("id IN (?)", readers).Order("id asc").Find(&nodes).Error; err != nil {
		return err
	}
	var inUse StorageInUseError
	for _, node := range nodes {
		labels, err := NodeLabels(node.Labels)
		if err != nil {
			return err
		}
		old, _ := effectiveStorage(before, previousTargets, node.ID, labels)
		next, applies := effectiveStorage(after, targets, node.ID, labels)
		if applies && old.Path == next.Path && old.CachePath == next.CachePath && old.Existing == next.Existing {
			continue
		}
		_, uuids, err := liveReadersOfStorage(tx, before.ID, node.ID)
		if err != nil {
			return err
		}
		inUse.InstanceUUIDs = append(inUse.InstanceUUIDs, uuids...)
	}
	if len(inUse.InstanceUUIDs) > 0 {
		return &inUse
	}
	return nil
}
