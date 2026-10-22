package cloud

import "gorm.io/gorm/clause"

// SaveCloudHostSnapshot refreshes host inventory and the linked node while
// preserving the initial reservation baseline used by resource leases.
func (s *Store) SaveCloudHostSnapshot(host CloudHost) error {
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "provider_id"}, {Name: "external_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"snapshot", "node_uuid", "updated_at"}),
	}).Create(&host).Error
}
