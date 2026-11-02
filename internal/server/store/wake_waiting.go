package store

import (
	"time"

	"gorm.io/gorm"
)

// A wake that did not fit can wait for room (Instance.WakeWaitingSince): the
// automation wakes the instances once their GPUs can take them back, and gives
// up after the wait limit. Any sleep or wake order written for an instance
// ends its wait (writeSleepIntent).

// MarkWakeWaiting starts the wait of a deployment's sleeping instances whose
// wake did not fit. An instance already waiting keeps its start, so asking
// again does not extend the wait. A wake that waits is a wake all the same:
// the named instances' sleep is no longer held (Instance.SleepHeld), so a
// request for the deployment is held for the wake rather than refused; the
// deployment's other sleepers keep their holds.
func (s *Store) MarkWakeWaiting(depID uint, uuids []string, at time.Time) error {
	if len(uuids) == 0 {
		return nil
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Instance{}).
			Where("deployment_id = ? AND uuid IN ? AND wake_waiting_since IS NULL", depID, uuids).
			Update("wake_waiting_since", at.UTC()).Error; err != nil {
			return err
		}
		return tx.Model(&Instance{}).Where("deployment_id = ? AND uuid IN ? AND sleep_held = ?", depID, uuids, true).
			Update("sleep_held", false).Error
	})
	if err != nil {
		return err
	}
	s.Bus.PublishChange("deployment", uintKey(depID), "wake-waiting")
	return nil
}

// EndWakeWaiting ends the wait of the named instances without changing their
// sleep intent: the wait gave up, or the operator withdrew it.
func (s *Store) EndWakeWaiting(depID uint, uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}
	res := s.db.Model(&Instance{}).
		Where("deployment_id = ? AND uuid IN ? AND wake_waiting_since IS NOT NULL", depID, uuids).
		Update("wake_waiting_since", nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		s.Bus.PublishChange("deployment", uintKey(depID), "wake-waiting")
	}
	return nil
}

// InstancesWakeWaiting is every instance whose wake waits for room, oldest
// wait first. It reads the wake_waiting_since index, so it costs nothing when
// no wake waits.
func (s *Store) InstancesWakeWaiting() ([]Instance, error) {
	var out []Instance
	err := s.db.Where("wake_waiting_since IS NOT NULL").Order("wake_waiting_since, id").Find(&out).Error
	return out, err
}
