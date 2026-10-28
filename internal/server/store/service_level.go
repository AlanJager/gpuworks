package store

import (
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

// Service levels a deployment shares GPUs at (Deployment.ServiceLevel); the
// policy each stands for is in package servicelevel.
const (
	ServiceLevelResident = servicelevel.Resident
	ServiceLevelOnline   = servicelevel.Online
	ServiceLevelShared   = servicelevel.Shared
	ServiceLevelOffline  = servicelevel.Offline
)

// ValidServiceLevel says level is one of the ServiceLevel* constants.
func ValidServiceLevel(level string) bool { return servicelevel.Valid(level) }

// addDeploymentServiceLevel adds the service level and the held-sleep flag.
// An existing deployment reads as resident with no held sleep, so nothing
// about it changes: it shares no cards and a request does not wake it unless
// idle sleep put it to sleep.
func addDeploymentServiceLevel(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) {
		return nil
	}
	for _, column := range []string{"ServiceLevel", "SleepHeld"} {
		if db.Migrator().HasColumn(&Deployment{}, column) {
			continue
		}
		if err := db.Migrator().AddColumn(&Deployment{}, column); err != nil {
			return err
		}
	}
	return nil
}

// SetDeploymentServiceLevel stores the deployment's service level.
func (s *Store) SetDeploymentServiceLevel(id uint, level string) error {
	res := s.db.Model(&Deployment{}).Where("id = ?", id).Update("service_level", level)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.Bus.PublishChange("deployment", uintKey(id), "service-level")
	return nil
}

// SharingDeployments are the active deployments that share cards by service
// level: every level but resident. The residency arbiter reads only these.
func (s *Store) SharingDeployments() ([]Deployment, error) {
	var out []Deployment
	err := s.reader().Where("desired_state = ? AND service_level <> ?", DeployActive, ServiceLevelResident).
		Order("id").Find(&out).Error
	return out, err
}
