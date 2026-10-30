package store

import (
	"encoding/json"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// Automation event codes and the trigger the residency arbiter gives its
// orders, as the automation writes them: the store cannot import it, so they
// are spelled here once, and a test in the app package
// (scheduling_catalogue_test.go) pins them to the automation's constants.
const (
	codeAutoSleep      = "AUTO_SLEEP"
	codeAutoWake       = "AUTO_WAKE"
	codeYieldedForWake = "YIELDED_FOR_WAKE"
	triggerPreempt     = "preempt"
)

// residencySwitchCodes are the codes a switch on the cards is recorded with:
// the automation's sleeps and wakes, and the sleep an operator's yielding
// wake ordered.
var residencySwitchCodes = []string{codeAutoSleep, codeAutoWake, codeYieldedForWake}

// IsYield reports whether an event records a yield: a deployment put to sleep
// so that another could have its cards. It is either the residency arbiter's
// preemption (an automatic sleep whose trigger is preempt) or an operator's
// yielding wake (YIELDED_FOR_WAKE, on the deployment that gave way). This is
// the one definition every count of yields (the overview, the timeline, the
// sharing stats) goes by.
func IsYield(ev InstanceEvent) bool {
	switch ev.Code {
	case codeYieldedForWake:
		return true
	case codeAutoSleep:
		var data struct {
			Trigger string `json:"trigger"`
		}
		return json.Unmarshal([]byte(ev.Data), &data) == nil && data.Trigger == triggerPreempt
	}
	return false
}

// IsAutoWake reports whether an event records a wake the platform ordered (a
// request, a resume, a schedule).
func IsAutoWake(ev InstanceEvent) bool { return ev.Code == codeAutoWake }

// EventForDeployment is the deployment an event's params say it acted for
// (for_deployment: the one a yield made room for), 0 when they name none. It
// is the one reader of that key.
func EventForDeployment(data string) uint {
	if data == "" {
		return 0
	}
	var params struct {
		ForDeployment uint `json:"for_deployment"`
	}
	if json.Unmarshal([]byte(data), &params) != nil {
		return 0
	}
	return params.ForDeployment
}

// ResidencySwitches counts, over a window, how often a deployment changed
// places with others on its cards.
type ResidencySwitches struct {
	// Yielded: it was put to sleep to make room for another deployment
	// (IsYield).
	Yielded int
	// YieldedTo: another deployment was put to sleep for it.
	YieldedTo int
	// Wakes: the platform woke it (a request, a resume, a schedule).
	Wakes int
}

// residencyOwnEventsQuery reads the sleeps and wakes recorded on some
// deployments from firstID on: a range of the (deployment_id, id) index per
// deployment. No kind is asked for, so that the planner reads by deployment
// rather than by every automation event of the window; these codes are
// written as automation events only.
func residencyOwnEventsQuery(db *gorm.DB, deploymentIDs []uint, firstID uint) *gorm.DB {
	return db.Model(&InstanceEvent{}).
		Where("deployment_id IN ? AND id >= ? AND code IN ?", deploymentIDs, firstID, residencySwitchCodes)
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
		counts := out[ev.DeploymentID]
		switch {
		case IsAutoWake(ev):
			counts.Wakes++
		case IsYield(ev):
			counts.Yielded++
			if to := EventForDeployment(ev.Data); to != 0 && to != ev.DeploymentID {
				waiting := out[to]
				waiting.YieldedTo++
				out[to] = waiting
			}
		}
		out[ev.DeploymentID] = counts
	}
	return out, nil
}

// ResidencyDeployment is what the timeline names a deployment by.
type ResidencyDeployment struct {
	ID   uint
	Name string
	// Yield is its yield policy.
	Yield string
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
	if err := s.reader().Select(append([]string{"id", "name"}, yieldPolicyColumns...)).Where("id IN ?", ids).Find(&live).Error; err != nil {
		return nil, err
	}
	for _, d := range live {
		out[d.ID] = ResidencyDeployment{ID: d.ID, Name: d.Name, Yield: d.Policy().Yield}
	}
	var gone []DeletedDeployment
	if err := s.reader().Where("id IN ?", ids).Find(&gone).Error; err != nil {
		return nil, err
	}
	for _, d := range gone {
		if _, ok := out[d.ID]; !ok {
			out[d.ID] = ResidencyDeployment{ID: d.ID, Name: d.Name, Yield: yieldpolicy.Never}
		}
	}
	return out, nil
}
