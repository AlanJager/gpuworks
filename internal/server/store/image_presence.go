package store

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// MarkImageNodeAbsentIfUnchanged records a missing runtime image only when the
// presence row still matches the observation made before the agent RPC. A
// newer pull or status report wins over an older absence probe.
func (s *Store) MarkImageNodeAbsentIfUnchanged(imageID, nodeID uint, seen time.Time, message string) (bool, error) {
	var changed bool
	err := s.commitBusinessSchedulingDecision(context.Background(), func(tx *gorm.DB) error {
		res := tx.Model(&ImageNode{}).
			Where("image_version_id = ? AND node_id = ? AND state = ? AND updated_at = ?", imageID, nodeID, ImageStatePresent, seen).
			Updates(map[string]any{
				"state": ImageStateAbsent, "size_bytes": int64(0),
				"transferred_bytes": int64(0), "rate_bps": int64(0),
				"transfer_id": "", "message": message,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		changed = true
		return saveUnpublishedNotification(tx, CommittedNotification{
			RequestKey: imageNodeNotifyKey(imageID), ResourceID: uintKey(imageID), Topic: "image", Reason: "node",
		})
	})
	if err != nil {
		return false, err
	}
	if !changed || s.Bus == nil {
		return changed, nil
	}
	_ = s.publishCommittedNotification(imageNodeNotifyKey(imageID))
	return true, nil
}
