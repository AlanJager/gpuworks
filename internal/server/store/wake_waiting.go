package store

import (
	"time"
)

// A wake that did not fit can wait for room (Instance.WakeWaitingSince): the
// controller wakes the instances once their GPUs can take them back, and gives
// up after the wait limit. Any sleep or wake order written for an instance
// ends its wait (writeSleepIntent).

// MarkWakeWaiting starts the wait of a deployment's sleeping instances whose
// wake did not fit. An instance already waiting keeps its start, so asking
// again does not extend the wait.
func (s *Store) MarkWakeWaiting(depID uint, uuids []string, at time.Time) error {
	if len(uuids) == 0 {
		return nil
	}
	res := s.db.Model(&Instance{}).
		Where("deployment_id = ? AND uuid IN ? AND wake_waiting_since IS NULL", depID, uuids).
		Update("wake_waiting_since", at.UTC())
	if res.Error != nil {
		return res.Error
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
