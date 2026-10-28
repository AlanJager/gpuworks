package store

import "gorm.io/gorm"

// addDeploymentUnitsAuto adds whether the platform places a multi-node or PD
// deployment's units. Existing deployments named their nodes themselves and
// keep that: false.
func addDeploymentUnitsAuto(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) || db.Migrator().HasColumn(&Deployment{}, "UnitsAuto") {
		return nil
	}
	return db.Migrator().AddColumn(&Deployment{}, "UnitsAuto")
}

// SetDeploymentUnitsAuto switches a multi-node or PD deployment between
// automatic placement and the operator's own. placement, when not empty,
// replaces the stored placement in the same write (a switch to automatic
// drops the PD units' pinned cards).
func (s *Store) SetDeploymentUnitsAuto(id uint, auto bool, placement string) error {
	updates := map[string]any{"units_auto": auto}
	if placement != "" {
		updates["placement"] = placement
	}
	res := s.db.Model(&Deployment{}).Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.Bus.PublishChange("deployment", uintKey(id), "placement")
	return nil
}
