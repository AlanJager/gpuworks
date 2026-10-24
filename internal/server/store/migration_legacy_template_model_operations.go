package store

import (
	"encoding/json"

	"gorm.io/gorm"
)

// repairLegacyTemplateModelOperations replaces the chat placeholder migration
// 80 wrote to every pre-template model when the newest non-placeholder
// template snapshot declares a different API. Template deployments before this
// repair kept the non-empty placeholder instead of the API the template
// selected, so embedding and rerank models appeared as chat-only deployments.
func repairLegacyTemplateModelOperations(db *gorm.DB) error {
	if !db.Migrator().HasTable("models") || !db.Migrator().HasTable("deployments") ||
		!db.Migrator().HasColumn("deployments", "model_id") ||
		!db.Migrator().HasColumn("deployments", "template_snapshot") {
		return nil
	}
	var rows []struct {
		ModelID    uint
		Operations string
		Snapshot   string
	}
	// The placeholder is recognized structurally below; SQL only narrows the
	// scan to rows that mention the chat operation at all.
	if err := db.Table("deployments").
		Select("deployments.model_id AS model_id, models.operations AS operations, deployments.template_snapshot AS snapshot").
		Joins("JOIN models ON models.id = deployments.model_id").
		Where("models.operations LIKE ? AND TRIM(COALESCE(deployments.template_snapshot, '')) <> ''",
			"%"+legacyChatOperationType+"%").
		Order("deployments.id DESC").
		Scan(&rows).Error; err != nil {
		return err
	}
	done := map[uint]bool{}
	for _, row := range rows {
		if done[row.ModelID] || !IsDefaultChatOperations(row.Operations) {
			continue
		}
		var snapshot templateSnapshotAPI
		if json.Unmarshal([]byte(row.Snapshot), &snapshot) != nil || snapshot.API == nil {
			continue
		}
		var operations []json.RawMessage
		if json.Unmarshal(snapshot.API.Operations, &operations) != nil || len(operations) == 0 ||
			IsDefaultChatOperations(string(snapshot.API.Operations)) {
			continue
		}
		// Guard on the exact bytes judged, so a row edited since the scan is
		// left alone.
		if err := db.Table("models").
			Where("id = ? AND operations = ?", row.ModelID, row.Operations).
			Update("operations", string(snapshot.API.Operations)).Error; err != nil {
			return err
		}
		done[row.ModelID] = true
	}
	return nil
}
