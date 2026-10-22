package store

import (
	"time"

	"gorm.io/gorm"
)

// repairableLegacyMigration lists additive schema changes whose numbers were
// occupied by other historical branches. Replaying arbitrary renamed migrations
// could repeat destructive cutovers, so the repair is deliberately bounded.
func repairableLegacyMigration(m migration) bool {
	switch m.Version {
	case 27:
		return m.Name == "terminal_sessions"
	case 28:
		return m.Name == "deployment_dgpu_quota_fields"
	case 40:
		return m.Name == "instance_image_digest_snapshot"
	}
	return false
}

func repairLegacyMigration(db *gorm.DB, m migration) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return tx.Model(&SchemaMigration{}).Where("version = ?", m.Version).
			Updates(map[string]any{"name": m.Name, "applied_at": time.Now().UTC()}).Error
	})
}

// reconcileLegacyResourceValues also repairs databases that already recorded
// the original migrations after AutoMigrate had introduced their columns.
func reconcileLegacyResourceValues(db *gorm.DB) error {
	if db.Migrator().HasColumn(&Instance{}, "Role") {
		if err := db.Exec("UPDATE instances SET role = '' WHERE role IS NULL").Error; err != nil {
			return err
		}
	}
	return renameDeploymentStateColumn(db)
}
