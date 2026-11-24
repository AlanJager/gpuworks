package store

import (
	"errors"

	"gorm.io/gorm"
)

// UpdateImageSyncProgressIfActive records progress only for the currently
// active transfer. A late relay callback must not overwrite an agent-reported
// terminal state or a newer retry.
func (s *Store) UpdateImageSyncProgressIfActive(imageID, nodeID uint, transferID string, sizeBytes, transferredBytes, rateBPS int64) (bool, error) {
	res := s.db.Model(&ImageNode{}).
		Where("image_version_id = ? AND node_id = ? AND state = ? AND transfer_id = ?", imageID, nodeID, ImageStateSyncing, transferID).
		Updates(map[string]any{
			"size_bytes":        sizeBytes,
			"transferred_bytes": transferredBytes,
			"rate_bps":          rateBPS,
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.RowsAffected > 0, nil
}

// BeginDirectImageSync persists the selected source and per-transfer limit.
// The limit is cleared by ApplyImageNodeStatus or a terminal failure helper.
func (s *Store) BeginDirectImageSync(imageID, nodeID, sourceNodeID uint, transferID string, sizeBytes, rateLimitKiB int64) error {
	if transferID == "" || sourceNodeID == 0 || rateLimitKiB < 0 {
		return errors.New("direct image sync parameters are invalid")
	}
	updates := map[string]any{
		"state": ImageStateSyncing, "transfer_id": transferID, "source_node_id": sourceNodeID,
		"source": "peer", "transferred_bytes": int64(0), "rate_bps": int64(0),
		"rate_limit_kib": rateLimitKiB, "message": "starting direct peer sync", "transfer_error_code": "",
	}
	if sizeBytes > 0 {
		updates["size_bytes"] = sizeBytes
	}
	res := s.db.Model(&ImageNode{}).Where("image_version_id = ? AND node_id = ?", imageID, nodeID).Updates(updates)
	if res.Error == nil && res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "syncing")
	}
	return res.Error
}

// RecordImageTransferBytes records how many bytes a transfer carried once the
// sending side has finished it. The count shown while a transfer runs is a
// sample: the receiving node reports one about once a second and reports the
// loaded image without one, and a relay's own last progress write is dropped
// once that report has finished the row. So the finished row would keep the
// last sample. This write is accepted in either order, on the row still
// syncing under transferID or on the row that transferID finished as present,
// and on no other. An archive size that was unknown becomes the count.
func (s *Store) RecordImageTransferBytes(imageID, nodeID uint, transferID string, bytes int64) (bool, error) {
	if transferID == "" || bytes <= 0 {
		return false, nil
	}
	res := s.db.Model(&ImageNode{}).
		Where("image_version_id = ? AND node_id = ?", imageID, nodeID).
		Where("(state = ? AND transfer_id = ?) OR (state = ? AND COALESCE(transfer_id, '') = '' AND last_transfer_id = ?)",
			ImageStateSyncing, transferID, ImageStatePresent, transferID).
		Updates(map[string]any{
			"transferred_bytes": bytes,
			"size_bytes":        gorm.Expr("CASE WHEN size_bytes > 0 THEN size_bytes ELSE ? END", bytes),
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.RowsAffected > 0, nil
}
