package store

import (
	"encoding/json"

	"gorm.io/gorm"
)

// Automation event codes and the trigger the residency arbiter gives its
// orders, as the automation package writes them (the store cannot import it).
const (
	residencyAutoSleep      = "AUTO_SLEEP"
	residencyAutoWake       = "AUTO_WAKE"
	residencyTriggerPreempt = "preempt"
)

// ResidencySwitches counts, over a window, how often a deployment changed
// places with others on its cards.
type ResidencySwitches struct {
	// Yielded: it was put to sleep to make room for another deployment.
	Yielded int
	// YieldedTo: another deployment was put to sleep for it.
	YieldedTo int
	// Wakes: the platform woke it (a request, a resume, a schedule).
	Wakes int
}

type switchEventData struct {
	Trigger       string `json:"trigger"`
	ForDeployment uint   `json:"for_deployment"`
}

// residencyOwnEventsQuery reads the sleeps and wakes the automation recorded
// on some deployments from firstID on: a range of the (deployment_id, id)
// index per deployment. No kind is asked for, so that the planner reads by
// deployment rather than by every automation event of the window; these two
// codes are written as automation events only.
func residencyOwnEventsQuery(db *gorm.DB, deploymentIDs []uint, firstID uint) *gorm.DB {
	return db.Model(&InstanceEvent{}).
		Where("deployment_id IN ? AND id >= ? AND code IN ?", deploymentIDs, firstID, []string{residencyAutoSleep, residencyAutoWake})
}

// ResidencySwitchCounts counts the switches of the deployments since the
// window began (firstID, from ResidencyWindowStart). A deployment is
// yielded-to when another of the deployments was put to sleep for it, so pass
// every deployment that held the cards: the one that gave a card up held it.
func (s *Store) ResidencySwitchCounts(deploymentIDs []uint, firstID uint) (map[uint]ResidencySwitches, error) {
	out := map[uint]ResidencySwitches{}
	if len(deploymentIDs) == 0 {
		return out, nil
	}
	var events []InstanceEvent
	if err := residencyOwnEventsQuery(s.reader(), deploymentIDs, firstID).Find(&events).Error; err != nil {
		return nil, err
	}
	for _, ev := range events {
		var data switchEventData
		_ = json.Unmarshal([]byte(ev.Data), &data)
		counts := out[ev.DeploymentID]
		switch {
		case ev.Code == residencyAutoWake:
			counts.Wakes++
		case ev.Code == residencyAutoSleep && data.Trigger == residencyTriggerPreempt:
			counts.Yielded++
			if data.ForDeployment != 0 && data.ForDeployment != ev.DeploymentID {
				waiting := out[data.ForDeployment]
				waiting.YieldedTo++
				out[data.ForDeployment] = waiting
			}
		}
		out[ev.DeploymentID] = counts
	}
	return out, nil
}

// ResidencyDeployment is what the timeline names a deployment by.
type ResidencyDeployment struct {
	ID           uint
	Name         string
	ServiceLevel string
}

// ResidencyDeployments names the deployments by id, including ones deleted
// since (their name is kept for the usage reports); a deployment that is
// neither has no entry.
func (s *Store) ResidencyDeployments(ids []uint) (map[uint]ResidencyDeployment, error) {
	out := map[uint]ResidencyDeployment{}
	if len(ids) == 0 {
		return out, nil
	}
	var live []Deployment
	if err := s.reader().Select("id", "name", "service_level").Where("id IN ?", ids).Find(&live).Error; err != nil {
		return nil, err
	}
	for _, d := range live {
		out[d.ID] = ResidencyDeployment{ID: d.ID, Name: d.Name, ServiceLevel: d.ServiceLevel}
	}
	var gone []DeletedDeployment
	if err := s.reader().Where("id IN ?", ids).Find(&gone).Error; err != nil {
		return nil, err
	}
	for _, d := range gone {
		if _, ok := out[d.ID]; !ok {
			out[d.ID] = ResidencyDeployment{ID: d.ID, Name: d.Name, ServiceLevel: ServiceLevelResident}
		}
	}
	return out, nil
}
