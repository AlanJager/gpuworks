package store

import (
	"encoding/json"
	"time"
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

// TookPartInYields says the deployment gave its cards up to another, or was
// given another's, over the window: it shared cards then.
func (c ResidencySwitches) TookPartInYields() bool { return c.Yielded > 0 || c.YieldedTo > 0 }

// ResidencyDeployment is what the timeline names a deployment by.
type ResidencyDeployment struct {
	ID   uint
	Name string
	// Yield is its yield policy; empty for a deleted deployment, whose policy
	// is not kept.
	Yield string
	// Deleted: it no longer exists, and Name is the one it left behind.
	Deleted bool
	// Since is when it was created (zero for a deleted one, whose creation is
	// not kept); Until is when it was deleted (zero while it exists, and for a
	// deployment deleted before deletions were recorded).
	Since, Until time.Time
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
	if err := s.reader().Select(append([]string{"id", "name", "created_at"}, yieldPolicyColumns...)).Where("id IN ?", ids).Find(&live).Error; err != nil {
		return nil, err
	}
	for _, d := range live {
		out[d.ID] = ResidencyDeployment{ID: d.ID, Name: d.Name, Yield: d.Policy().Yield, Since: d.CreatedAt.UTC()}
	}
	var gone []DeletedDeployment
	if err := s.reader().Where("id IN ?", ids).Find(&gone).Error; err != nil {
		return nil, err
	}
	for _, d := range gone {
		if _, ok := out[d.ID]; !ok {
			out[d.ID] = ResidencyDeployment{ID: d.ID, Name: d.Name, Deleted: true, Until: d.DeletedAt.UTC()}
		}
	}
	return out, nil
}
