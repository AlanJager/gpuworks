package store

import (
	"encoding/json"
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Sleep intent lives on each instance: its desired state's DesiredSleep is
// what the controller pushes and the gateway routes by. A deployment-wide
// order writes every live instance's and records the level on the deployment
// (deployments.desired_sleep: "the deployment as a whole is under a sleep
// order", which new instances start with); an order for some instances writes
// only theirs.

// rollbackSleepIntent rolls back the failed order an instance reported, when
// the order is still the one standing: the instance's own intent goes back to
// the level it is actually at, and a deployment-wide order at that level is
// withdrawn, so its new instances do not start asleep. A multi-node leader's
// report rolls back its generation's followers too: they take no order of
// their own and sleep or wake only with the leader's engine, so their intent
// must follow the leader's or the ledger and the gateway read the replica as
// asleep while it is awake (and the reverse). The other replicas' intents
// stay: a sleep that failed on one does not wake the rest. An order made after
// the agent started is left untouched.
func rollbackSleepIntent(tx *gorm.DB, inst *Instance, st proto.InstanceStatus) (bool, error) {
	if st.Phase != proto.PhaseSleep || st.Message == "" || st.SleepTarget == nil || *st.SleepTarget == st.SleepLevel {
		return false, nil
	}
	target := *st.SleepTarget
	rolled := false
	var current Instance
	if err := tx.First(&current, inst.ID).Error; err == nil && current.SleepIntent() == target {
		ranks := []Instance{current}
		if current.NNodes > 1 && current.DistGroup != "" && !current.Headless {
			var followers []Instance
			if err := tx.Where("deployment_id = ? AND dist_group = ? AND dist_attempt = ? AND headless = ?",
				current.DeploymentID, current.DistGroup, current.DistAttempt, true).Find(&followers).Error; err != nil {
				return false, err
			}
			ranks = append(ranks, followers...)
		}
		for i := range ranks {
			if ranks[i].DesiredStateJSON == "" || ranks[i].SleepIntent() != target {
				continue
			}
			if err := rewriteDesiredState(tx, &ranks[i], func(spec *proto.InstanceDesiredState) bool {
				spec.DesiredSleep = st.SleepLevel
				return true
			}); err != nil {
				return false, err
			}
			// Rolled back awake, it is no longer asleep, held or not.
			if st.SleepLevel == 0 && ranks[i].SleepHeld {
				if err := tx.Model(&Instance{}).Where("id = ?", ranks[i].ID).Update("sleep_held", false).Error; err != nil {
					return false, err
				}
			}
		}
		rolled = true
	}
	var dep Deployment
	if err := tx.First(&dep, inst.DeploymentID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return rolled, nil
		}
		return false, err
	}
	if dep.DesiredSleep != target {
		return rolled, nil
	}
	if err := tx.Model(&Deployment{}).Where("id = ? AND desired_sleep = ?", dep.ID, target).Update("desired_sleep", st.SleepLevel).Error; err != nil {
		return false, err
	}
	return true, nil
}

// HoldsSleepIntent reports whether an instance takes sleep orders: it fills
// its replica (proto.InstanceFillsReplica) and has a desired state.
func HoldsSleepIntent(in *Instance) bool {
	return proto.InstanceFillsReplica(in.State) && in.DesiredStateJSON != ""
}

// writeSleepIntent rewrites the sleep intent of the instances that hold one,
// inside a transaction. A new order supersedes a wake that was waiting for
// room: a wake has been admitted, or the instance is told to sleep again. A
// wake (level 0) lifts the hold of every instance it names, a held sleep
// holds the ones it puts to sleep, and a sleep that is not held leaves each
// instance's hold as it was (Instance.SleepHeld).
func writeSleepIntent(tx *gorm.DB, insts []Instance, level int, mode string, held bool) error {
	if err := writeSleepHeld(tx, insts, level, held); err != nil {
		return err
	}
	var waiting []uint
	for i := range insts {
		if insts[i].WakeWaitingSince != nil {
			waiting = append(waiting, insts[i].ID)
		}
	}
	if len(waiting) > 0 {
		if err := tx.Model(&Instance{}).Where("id IN ?", waiting).Update("wake_waiting_since", nil).Error; err != nil {
			return err
		}
	}
	for i := range insts {
		if !HoldsSleepIntent(&insts[i]) {
			continue
		}
		if err := rewriteDesiredState(tx, &insts[i], func(spec *proto.InstanceDesiredState) bool {
			if spec.DesiredSleep == level && (mode == "" || spec.SleepMode == mode) {
				return false
			}
			spec.DesiredSleep = level
			if mode != "" {
				spec.SleepMode = mode
			}
			return true
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeSleepHeld is writeSleepIntent's change to the instances' holds.
func writeSleepHeld(tx *gorm.DB, insts []Instance, level int, held bool) error {
	if level > 0 && !held {
		return nil
	}
	hold := level > 0
	var ids []uint
	for i := range insts {
		if insts[i].SleepHeld == hold || hold && !HoldsSleepIntent(&insts[i]) {
			continue
		}
		ids = append(ids, insts[i].ID)
		insts[i].SleepHeld = hold
	}
	if len(ids) == 0 {
		return nil
	}
	return tx.Model(&Instance{}).Where("id IN ?", ids).Update("sleep_held", hold).Error
}

// stampDeploymentSleep starts a new instance under its deployment's standing
// sleep order: the level the deployment as a whole is ordered to, read in the
// transaction that inserts the row. An order committed after the caller built
// the desired state therefore still reaches the instance, and one committed
// after the insert rewrites it with the others. That includes a wake: a
// deployment no longer under a sleep order starts its new instances awake,
// whatever level the caller read before the wake was committed. A
// deployment-wide sleep is held, so an instance started under one is held
// too, and one started awake is not.
func stampDeploymentSleep(tx *gorm.DB, in *Instance) error {
	if in.DeploymentID == 0 || in.DesiredStateJSON == "" {
		return nil
	}
	var dep Deployment
	if err := tx.Select("desired_sleep").First(&dep, in.DeploymentID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	in.SleepHeld = dep.DesiredSleep > 0
	if in.SleepIntent() == dep.DesiredSleep {
		return nil
	}
	spec, err := in.DesiredState()
	if err != nil {
		return err
	}
	spec.DesiredSleep = dep.DesiredSleep
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	in.DesiredStateJSON = string(raw)
	return nil
}

// SetDeploymentSleep sets the deployment-wide sleep level and every live
// instance's intent to it, and triggers reconcile. Level 0 is the
// deployment-wide wake.
func (s *Store) SetDeploymentSleep(id uint, level int) error {
	return s.OrderDeploymentSleep(id, level, "")
}

// OrderDeploymentSleep records a deployment-wide sleep order: the level on the
// deployment and, on every live instance, the level and pause mode (an empty
// mode keeps each instance's), atomically. Only an operator or a schedule
// orders a whole deployment to sleep, so the sleep holds every instance it
// puts to sleep, and a deployment-wide wake lifts every instance's hold
// (Instance.SleepHeld).
func (s *Store) OrderDeploymentSleep(id uint, level int, mode string) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Deployment{}).Where("id = ?", id).Update("desired_sleep", level).Error; err != nil {
			return err
		}
		var insts []Instance
		if err := tx.Where("deployment_id = ?", id).Find(&insts).Error; err != nil {
			return err
		}
		return writeSleepIntent(tx, insts, level, mode, true)
	})
	if err == nil {
		s.Bus.PublishChange("deployment", uintKey(id), "sleep")
	}
	return err
}

// OrderInstancesSleep records a sleep (level > 0) or wake (level 0) order for
// some of a deployment's instances that is not held: the platform's sleep, or
// a wake. A sleep leaves the deployment-wide level alone: the deployment as a
// whole is not under that order. A wake withdraws a deployment-wide sleep
// order, since the deployment is no longer wholly under it; its other
// sleepers keep their own intent and hold.
func (s *Store) OrderInstancesSleep(depID uint, uuids []string, level int, mode string) error {
	return s.OrderInstancesSleepHeld(depID, uuids, level, mode, false)
}

// OrderInstancesSleepHeld is OrderInstancesSleep that also says whether the
// sleep is held (an operator's or a schedule's): a held sleep holds exactly
// the instances named, a wake lifts exactly their holds, and a sleep that is
// not held leaves every hold as it was, so the platform putting a replica to
// sleep neither lifts nor sets an operator's hold (Instance.SleepHeld).
func (s *Store) OrderInstancesSleepHeld(depID uint, uuids []string, level int, mode string, held bool) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if level == 0 {
			if err := tx.Model(&Deployment{}).Where("id = ?", depID).Update("desired_sleep", 0).Error; err != nil {
				return err
			}
		}
		var insts []Instance
		if err := tx.Where("deployment_id = ? AND uuid IN ?", depID, uuids).Find(&insts).Error; err != nil {
			return err
		}
		if len(insts) != len(uuids) {
			return ErrNotFound
		}
		return writeSleepIntent(tx, insts, level, mode, held)
	})
	if err == nil {
		for _, uuid := range uuids {
			s.Bus.PublishChange("instance", uuid, "desired-state")
		}
		s.Bus.PublishChange("deployment", uintKey(depID), "sleep")
	}
	return err
}
