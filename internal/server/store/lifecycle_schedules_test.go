package store

import (
	"errors"
	"testing"
	"time"
)

// A new or edited order has not fired: its first occurrence is the next one,
// not one already past. The fired mark only moves forward.
func TestLifecycleScheduleFiredMarkStartsAtWriteAndOnlyMovesForward(t *testing.T) {
	st := newTestStore(t)
	dep, err := st.CreateDeployment(&Deployment{Name: "d", ServedName: "d", State: DeployActive, Sleepable: true})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Second)
	sc := &LifecycleSchedule{DeploymentID: dep.ID, Action: ScheduleActionSleep, Weekdays: 127, Minute: 21 * 60, SleepLevel: 2, Enabled: true}
	if err := st.CreateLifecycleSchedule(sc); err != nil {
		t.Fatal(err)
	}
	got, err := st.LifecycleSchedule(dep.ID, sc.ID)
	if err != nil || got.LastFiredAt == nil || got.LastFiredAt.Before(before) {
		t.Fatalf("new order = %+v err=%v", got, err)
	}
	later := time.Now().UTC().Add(time.Hour)
	if err := st.MarkLifecycleScheduleFired(sc.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkLifecycleScheduleFired(sc.ID, later.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LifecycleSchedule(dep.ID, sc.ID)
	if !got.LastFiredAt.Equal(later) {
		t.Fatalf("fired mark = %v, want %v (never moved back)", got.LastFiredAt, later)
	}
	sc.Minute = 22 * 60
	if err := st.UpdateLifecycleSchedule(sc); err != nil {
		t.Fatal(err)
	}
	got, _ = st.LifecycleSchedule(dep.ID, sc.ID)
	if got.Minute != 22*60 || got.LastFiredAt.After(later) {
		t.Fatalf("edited order = %+v", got)
	}
	if err := st.UpdateLifecycleSchedule(&LifecycleSchedule{ID: sc.ID, DeploymentID: dep.ID + 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("edit through another deployment = %v", err)
	}
}

// Only enabled orders of active deployments are evaluated, and a deleted
// deployment takes its schedule with it.
func TestEnabledLifecycleSchedulesAndDeploymentDeletion(t *testing.T) {
	st := newTestStore(t)
	active, _ := st.CreateDeployment(&Deployment{Name: "a", ServedName: "a", State: DeployActive})
	stopped, _ := st.CreateDeployment(&Deployment{Name: "s", ServedName: "s", State: DeployStopped})
	for _, sc := range []*LifecycleSchedule{
		{DeploymentID: active.ID, Action: ScheduleActionWake, Weekdays: 1, Minute: 1, Enabled: true},
		{DeploymentID: active.ID, Action: ScheduleActionWake, Weekdays: 1, Minute: 2, Enabled: false},
		{DeploymentID: stopped.ID, Action: ScheduleActionWake, Weekdays: 1, Minute: 3, Enabled: true},
	} {
		if err := st.CreateLifecycleSchedule(sc); err != nil {
			t.Fatal(err)
		}
	}
	enabled, err := st.EnabledLifecycleSchedules()
	if err != nil || len(enabled) != 1 || enabled[0].Minute != 1 {
		t.Fatalf("enabled = %+v err=%v", enabled, err)
	}
	if err := st.DeleteDeployment(active.ID); err != nil {
		t.Fatal(err)
	}
	left, err := st.LifecycleSchedules(active.ID)
	if err != nil || len(left) != 0 {
		t.Fatalf("schedule of a deleted deployment = %+v err=%v", left, err)
	}
}

// A wake waits from its first mark; asking again does not extend it; any
// sleep or wake order for the instance ends the wait.
func TestWakeWaitingIsEndedByTheNextOrder(t *testing.T) {
	st := newTestStore(t)
	dep, _ := st.CreateDeployment(&Deployment{Name: "w", ServedName: "w", State: DeployActive, Sleepable: true})
	for _, uuid := range []string{"w-1", "w-2"} {
		if _, err := st.CreateInstance(&Instance{UUID: uuid, DeploymentID: dep.ID, State: "running", SleepLevel: 1, DesiredStateJSON: `{"uuid":"` + uuid + `","desired_sleep":1}`}); err != nil {
			t.Fatal(err)
		}
	}
	first := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	if err := st.MarkWakeWaiting(dep.ID, []string{"w-1", "w-2"}, first); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkWakeWaiting(dep.ID, []string{"w-1"}, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	waiting, err := st.InstancesWakeWaiting()
	if err != nil || len(waiting) != 2 || !waiting[0].WakeWaitingSince.Equal(first) {
		t.Fatalf("waiting = %+v err=%v", waiting, err)
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"w-1"}, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.EndWakeWaiting(dep.ID, []string{"w-2"}); err != nil {
		t.Fatal(err)
	}
	if waiting, _ := st.InstancesWakeWaiting(); len(waiting) != 0 {
		t.Fatalf("still waiting after the order and the end: %+v", waiting)
	}
}
