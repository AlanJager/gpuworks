package store

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

// UnfinishedCacheCopies is a startup-only inventory for reconstructing the
// controller's derived claim index. Progress and reconciliation never call it.
func (s *Store) UnfinishedCacheCopies() ([]ModelFile, error) {
	var rows []ModelFile
	err := s.reader().Table("model_files").Select("model_files.*").Joins("JOIN storages ON storages.id = model_files.storage_id").Where("storages.type = ? AND model_files.node_id <> 0 AND model_files.acquisition = ? AND model_files.state IN ?", proto.StorageNFSCache, proto.ModelAcquireCopy, []string{proto.MFPending, proto.MFDownloading, proto.MFFailed, proto.MFInsufficientDisk}).Find(&rows).Error
	return rows, err
}

// SuspendCacheCopy withdraws a no-longer-needed cache intent without deleting
// resumable bytes. Rotating its existing ID fences any late worker result.
func (s *Store) SuspendCacheCopy(id uint, transferID string) error {
	result := s.db.Model(&ModelFile{}).Where("id = ? AND transfer_id = ? AND acquisition = ? AND state IN ?", id, transferID, proto.ModelAcquireCopy, []string{proto.MFPending, proto.MFDownloading, proto.MFFailed, proto.MFInsufficientDisk}).Updates(map[string]any{"state": proto.MFAbsent, "transfer_id": auth.NewUUID(), "rate_bps": 0, "message": ""})
	if result.Error == nil && result.RowsAffected > 0 {
		s.publishModelFile(id, proto.MFAbsent)
	}
	return result.Error
}
