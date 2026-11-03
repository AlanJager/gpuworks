package store

import "gorm.io/gorm"

// Holding a sleep is a fact of each instance (Instance.SleepHeld): an
// operator's or a schedule's sleep holds exactly the instances it orders, a
// wake lifts the hold of exactly the instances it wakes or leaves waiting
// for room, and a sleep the platform chose (the residency arbiter's) neither
// sets nor clears it. A deployment-wide sleep order (deployments.
// desired_sleep) is always held: only an operator or a schedule orders a
// whole deployment to sleep; the platform names the replicas it puts to
// sleep.

// WakesOnRequest says a request for the deployment wakes in, one of its
// sleepers, rather than being refused: the deployment can sleep, and in's
// sleep is the platform's choice rather than an operator's or a schedule's.
// The gateway, the residency arbiter, placement, batches and the playground
// all ask this one question, per sleeper; what they say of a whole
// deployment is derived from it (planning.RequestWakes).
func (d *Deployment) WakesOnRequest(in *Instance) bool {
	return d != nil && in != nil && d.Sleepable && !in.SleepHeld
}

// ReleaseSleepHolds lifts the hold of every held instance of the deployment
// without waking any, so a request now wakes them, and returns their uuids
// (none when none was held). A deployment-wide sleep order is withdrawn with
// it: that order is always held, and a replica started under it would start
// asleep and held again. The instances keep their own sleep intent, which is
// what the order wrote on them, so they stay asleep until a request or an
// operator wakes them.
func (s *Store) ReleaseSleepHolds(depID uint) ([]string, error) {
	var released []string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Deployment{}).Where("id = ? AND desired_sleep > 0", depID).Update("desired_sleep", 0).Error; err != nil {
			return err
		}
		var held []Instance
		if err := tx.Select("id", "uuid").Where("deployment_id = ? AND sleep_held = ?", depID, true).Order("id").Find(&held).Error; err != nil {
			return err
		}
		if len(held) == 0 {
			return nil
		}
		ids := make([]uint, 0, len(held))
		for _, in := range held {
			ids = append(ids, in.ID)
			released = append(released, in.UUID)
		}
		return tx.Model(&Instance{}).Where("id IN ?", ids).Update("sleep_held", false).Error
	})
	if err != nil {
		return nil, err
	}
	for _, uuid := range released {
		s.Bus.PublishChange("instance", uuid, "sleep-held")
	}
	s.Bus.PublishChange("deployment", uintKey(depID), "sleep")
	return released, nil
}
