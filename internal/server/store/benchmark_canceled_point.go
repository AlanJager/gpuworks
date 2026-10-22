package store

import (
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The cancellation path marks the run terminal before asking the executor to
// stop. Its final partial point can arrive afterward. Live process curves may
// already have created a running placeholder; replace only that placeholder's
// result, preserving its curve and identity. Terminal results are immutable.
func (s *Store) preserveCanceledBenchmarkPoint(runID uint, raw []byte) error {
	point, ok := decodeBenchmarkPoint(raw)
	if !ok || point.Seq <= 0 || point.State != "canceled" {
		return nil
	}
	point.ID, point.RunID = 0, runID
	changed := false
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		// Acquire the writer lock only while the canceled parent still exists.
		// A concurrent delete then either wins first or also deletes this point.
		// UpdateColumn leaves the terminal run's timestamps and facts intact.
		parent := tx.Model(&BenchmarkRun{}).Where("id = ? AND state = ?", runID, "canceled").
			UpdateColumn("state", "canceled")
		if parent.Error != nil || parent.RowsAffected == 0 {
			return parent.Error
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&point)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			result = tx.Model(&BenchmarkPoint{}).
				Where("run_id = ? AND seq = ? AND state = ?", runID, point.Seq, "running").
				Select("*").Omit("id", "run_id", "seq", "created_at", "curve").Updates(&point)
			if result.Error != nil {
				return result.Error
			}
		}
		changed = result.RowsAffected > 0
		return nil
	}); err != nil {
		return err
	}
	if changed && s.Bus != nil {
		s.Bus.PublishChange("benchmark", fmt.Sprint(runID), "point")
	}
	return nil
}
