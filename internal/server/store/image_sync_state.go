package store

import (
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
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
// and on no other. An archive size that was unknown becomes the count, and
// the rate is cleared: nothing is moving any more.
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
			"rate_bps":          int64(0),
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		// Only counters moved; the row's state is what it was.
		s.Bus.PublishChange("image", uintKey(imageID), bus.ReasonProgress)
	}
	return res.RowsAffected > 0, nil
}

// imageStatusCount is what a node status with a byte count writes as the
// row's count. A status is taken as it is, with one exception: a progress
// sample of a peer copy never lowers the count of the transfer it belongs to.
// The receiving node's samples and the sending side's final count reach the
// server on different streams, so a sample sent earlier can be handled after
// the final count; the comparison is made by the UPDATE itself, on the row as
// it is then, because the final count can land after the caller read the row.
//
// Every other count may fall and must be shown falling: a registry pull keeps
// one transfer id across its sources and across download and extraction, and
// an archive download restarts from zero when its server ignores the range.
func imageStatusCount(current ImageNode, st proto.ImageNodeStatus) any {
	peerSample := st.State == ImageStateSyncing && current.State == ImageStateSyncing &&
		current.Source == "peer" && (st.Source == "" || st.Source == "peer") &&
		st.TransferID != "" && st.TransferID == current.TransferID
	if !peerSample {
		return st.TransferredBytes
	}
	return gorm.Expr("CASE WHEN transferred_bytes > ? THEN transferred_bytes ELSE ? END", st.TransferredBytes, st.TransferredBytes)
}
