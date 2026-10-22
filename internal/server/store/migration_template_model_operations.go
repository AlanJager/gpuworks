package store

import (
	"encoding/json"

	"gorm.io/gorm"
)

// templateSnapshotAPI is the one part of a deployment's template snapshot
// (template_snapshot, a deploy.CatalogSelection) this migration reads: the API
// contract the template declared, as snapshots spelled it when it was written.
type templateSnapshotAPI struct {
	API *struct {
		Operations json.RawMessage `json:"operations"`
	} `json:"api"`
}

// backfillTemplateModelOperations repairs model rows a template deployment
// created without an API. Migration 80 gave every row of its day an
// operations value, so an empty one since can only come from the template
// path, which recorded the template's API in the deployment snapshot but not
// on the model. The deployment view, the Playground and its proxy read the
// model row alone, so those deployments were invisible there while the
// launch, which falls back to the snapshot, served them. Each such row takes
// the operations of the newest deployment snapshot that declares some; a row
// that holds any value, including an explicit [], is left alone.
func backfillTemplateModelOperations(db *gorm.DB) error {
	if !db.Migrator().HasTable("models") || !db.Migrator().HasTable("deployments") ||
		!db.Migrator().HasColumn("deployments", "template_snapshot") {
		return nil
	}
	var rows []struct {
		ModelID  uint
		Snapshot string
	}
	if err := db.Table("deployments").
		Select("deployments.model_id AS model_id, deployments.template_snapshot AS snapshot").
		Joins("JOIN models ON models.id = deployments.model_id").
		Where("TRIM(COALESCE(models.operations, '')) = '' AND TRIM(COALESCE(deployments.template_snapshot, '')) <> ''").
		Order("deployments.id DESC").
		Scan(&rows).Error; err != nil {
		return err
	}
	done := map[uint]bool{}
	for _, row := range rows {
		if done[row.ModelID] {
			continue
		}
		var snapshot templateSnapshotAPI
		if json.Unmarshal([]byte(row.Snapshot), &snapshot) != nil || snapshot.API == nil {
			continue
		}
		var operations []json.RawMessage
		if json.Unmarshal(snapshot.API.Operations, &operations) != nil || len(operations) == 0 {
			continue
		}
		if err := db.Table("models").
			Where("id = ? AND TRIM(COALESCE(operations, '')) = ''", row.ModelID).
			Update("operations", string(snapshot.API.Operations)).Error; err != nil {
			return err
		}
		done[row.ModelID] = true
	}
	return nil
}
