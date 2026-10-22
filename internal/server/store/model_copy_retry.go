package store

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

// RetryModelCopy changes only the failed intent whose worker the caller has
// confirmed stopped. A concurrent replacement or successful completion wins.
func (s *Store) RetryModelCopy(id uint, transferID string) error {
	result := s.db.Model(&ModelFile{}).Where("id = ? AND transfer_id = ? AND acquisition = ? AND state IN ?", id, transferID, proto.ModelAcquireCopy, []string{proto.MFFailed, proto.MFInsufficientDisk}).Updates(map[string]any{
		"state": proto.MFPending, "transfer_id": auth.NewUUID(), "rate_bps": 0, "message": "",
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrStateChanged
	}
	s.publishModelFile(id, proto.MFPending)
	return nil
}

// SetModelCopyRate changes pacing without changing the acquisition method.
func (s *Store) SetModelCopyRate(ref FileRef, rateLimitKiB int) error {
	if rateLimitKiB < 0 {
		return errors.New("model copy rate limit must be non-negative")
	}
	return refQuery(s.db.Model(&ModelFile{}), s.resolveRef(ref)).Where("acquisition = ?", proto.ModelAcquireCopy).Update("rate_limit", rateLimitKiB).Error
}
