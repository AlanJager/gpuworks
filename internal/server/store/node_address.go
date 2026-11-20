package store

import (
	"strings"

	"gorm.io/gorm"
)

// SetNodeDirectAddress updates only the agent-reachable data-plane address.
// Keeping this as a narrow explicit store operation avoids replaying a partial
// NodeStatus, which would otherwise reconcile (and potentially erase) the
// node's GPU inventory.
// The address and its notification commit together. A missed publish stays
// unpublished so reconciliation can deliver it once without creating another row.
func (s *Store) SetNodeDirectAddress(uuid, address string) error {
	address = strings.TrimSpace(address)
	err := s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&Node{}).Where("uuid = ?", uuid).Update("direct_address", address)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if uuid == "" {
			return nil
		}
		return saveUnpublishedNotification(tx, CommittedNotification{
			RequestKey: nodeNotifyKey(uuid), ResourceID: uuid, Topic: "node", Reason: "direct_address",
		})
	})
	if err != nil || s.Bus == nil || uuid == "" {
		return err
	}
	_ = s.publishCommittedNotification(nodeNotifyKey(uuid))
	return nil
}
