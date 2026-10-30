package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// Actions a lifecycle schedule takes.
const (
	ScheduleActionSleep    = "sleep"
	ScheduleActionWake     = "wake"
	ScheduleActionReplicas = "replicas"
)

// LifecycleSchedule is one standing order of a deployment's schedule: at
// Minute (minutes after midnight) on the weekdays in Weekdays (bit 0 Sunday …
// bit 6 Saturday), in Timezone (an IANA name, "" for the server's own zone),
// sleep at SleepLevel, wake, or run Replicas replicas. Each occurrence runs
// once: LastFiredAt is the scheduled time of the newest occurrence that was
// handled, so a manual order in between is not overridden again.
type LifecycleSchedule struct {
	ID           uint   `gorm:"primaryKey"`
	DeploymentID uint   `gorm:"index;not null"`
	Action       string `gorm:"size:16;not null"`
	Weekdays     int    `gorm:"not null"`
	Minute       int    `gorm:"not null"`
	Timezone     string `gorm:"size:64;not null;default:''"`
	SleepLevel   int    `gorm:"not null;default:0"`
	Replicas     int    `gorm:"not null;default:0"`
	// Role is the PD role a replicas order scales ("prefill", "decode"); empty
	// for every other order, and for a replicas order of a deployment that is
	// not PD.
	Role        string `gorm:"size:16;not null;default:''"`
	Enabled     bool   `gorm:"not null;index"`
	LastFiredAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func createLifecycleSchedules(db *gorm.DB) error {
	return db.AutoMigrate(&LifecycleSchedule{})
}

// addLifecycleScheduleRole adds the PD role a replicas order scales. An
// existing order scales no role: PD deployments could not be given one.
func addLifecycleScheduleRole(db *gorm.DB) error {
	if !db.Migrator().HasTable(&LifecycleSchedule{}) || db.Migrator().HasColumn(&LifecycleSchedule{}, "Role") {
		return nil
	}
	return db.Migrator().AddColumn(&LifecycleSchedule{}, "Role")
}

// LifecycleSchedules is a deployment's schedule in creation order.
func (s *Store) LifecycleSchedules(deploymentID uint) ([]LifecycleSchedule, error) {
	var out []LifecycleSchedule
	err := s.reader().Where("deployment_id = ?", deploymentID).Order("id").Find(&out).Error
	return out, err
}

// EnabledLifecycleSchedules is every enabled schedule of an active
// deployment, for the automation to evaluate.
func (s *Store) EnabledLifecycleSchedules() ([]LifecycleSchedule, error) {
	var out []LifecycleSchedule
	err := s.reader().
		Joins("JOIN deployments ON deployments.id = lifecycle_schedules.deployment_id").
		Where("lifecycle_schedules.enabled = ? AND deployments.desired_state = ?", true, DeployActive).
		Order("lifecycle_schedules.id").Find(&out).Error
	return out, err
}

// CreateLifecycleSchedule adds one order to a deployment's schedule. A new
// order has not fired: its first occurrence is the next one after now.
func (s *Store) CreateLifecycleSchedule(sc *LifecycleSchedule) error {
	sc.ID = 0
	now := time.Now().UTC()
	sc.LastFiredAt = &now
	if err := s.db.Create(sc).Error; err != nil {
		return err
	}
	s.Bus.PublishChange("deployment", uintKey(sc.DeploymentID), "schedule")
	return nil
}

// UpdateLifecycleSchedule rewrites one order of a deployment's schedule. A
// changed time is a new order: occurrences before now do not fire.
func (s *Store) UpdateLifecycleSchedule(sc *LifecycleSchedule) error {
	now := time.Now().UTC()
	res := s.db.Model(&LifecycleSchedule{}).Where("id = ? AND deployment_id = ?", sc.ID, sc.DeploymentID).Updates(map[string]any{
		"action": sc.Action, "weekdays": sc.Weekdays, "minute": sc.Minute, "timezone": sc.Timezone,
		"sleep_level": sc.SleepLevel, "replicas": sc.Replicas, "role": sc.Role, "enabled": sc.Enabled, "last_fired_at": now,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	sc.LastFiredAt = &now
	s.Bus.PublishChange("deployment", uintKey(sc.DeploymentID), "schedule")
	return nil
}

// DeleteLifecycleSchedule removes one order of a deployment's schedule.
func (s *Store) DeleteLifecycleSchedule(deploymentID, id uint) error {
	res := s.db.Where("id = ? AND deployment_id = ?", id, deploymentID).Delete(&LifecycleSchedule{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.Bus.PublishChange("deployment", uintKey(deploymentID), "schedule")
	return nil
}

// MarkLifecycleScheduleFired records that the occurrence scheduled at at was
// handled. It never moves the mark back, so a late evaluation cannot make an
// occurrence fire twice.
func (s *Store) MarkLifecycleScheduleFired(id uint, at time.Time) error {
	return s.db.Model(&LifecycleSchedule{}).
		Where("id = ? AND (last_fired_at IS NULL OR last_fired_at < ?)", id, at.UTC()).
		Update("last_fired_at", at.UTC()).Error
}

// LifecycleSchedule reads one order of a deployment's schedule.
func (s *Store) LifecycleSchedule(deploymentID, id uint) (*LifecycleSchedule, error) {
	var sc LifecycleSchedule
	err := s.reader().Where("id = ? AND deployment_id = ?", id, deploymentID).First(&sc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &sc, err
}

// indexInstanceEventKind lets the scheduling board read the newest events of
// one kind across all deployments without scanning the timeline.
func indexInstanceEventKind(db *gorm.DB) error {
	if !db.Migrator().HasTable(&InstanceEvent{}) {
		return nil
	}
	return db.Exec("CREATE INDEX IF NOT EXISTS idx_instance_events_kind_id ON instance_events(kind, id)").Error
}

// RecentEventsOfKind is the newest limit events of a kind across every
// deployment, newest first.
func (s *Store) RecentEventsOfKind(kind string, limit int) ([]InstanceEvent, error) {
	var out []InstanceEvent
	err := s.reader().Where("kind = ?", kind).Order("id desc").Limit(limit).Find(&out).Error
	return out, err
}
