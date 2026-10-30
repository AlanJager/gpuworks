package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// heldFixture is a sleepable deployment with running instances that take
// sleep orders, named by uuid.
func heldFixture(t *testing.T, s *Store, uuids ...string) *Deployment {
	t.Helper()
	dep, err := s.CreateDeployment(&Deployment{Name: "held", ServedName: "held", Engine: "vllm", Sleepable: true, State: DeployActive, Replicas: len(uuids)})
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range uuids {
		raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true})
		if _, err := s.CreateInstance(&Instance{UUID: uuid, DeploymentID: dep.ID, State: proto.InstRunning, DesiredStateJSON: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	return dep
}

// heldOf is each named instance's hold and sleep intent.
func heldOf(t *testing.T, s *Store, uuids ...string) map[string][2]int {
	t.Helper()
	out := map[string][2]int{}
	for _, uuid := range uuids {
		in, err := s.InstanceByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		held := 0
		if in.SleepHeld {
			held = 1
		}
		out[uuid] = [2]int{held, in.SleepIntent()}
	}
	return out
}

func wantHeld(t *testing.T, step string, got map[string][2]int, want map[string][2]int) {
	t.Helper()
	for uuid, w := range want {
		if got[uuid] != w {
			t.Errorf("%s: %s {held, intent} = %v, want %v", step, uuid, got[uuid], w)
		}
	}
}

// A held sleep holds exactly the instances it orders, a sleep the platform
// chose neither sets nor lifts a hold, and a wake lifts exactly the holds of
// the instances it wakes.
func TestSleepHeldIsPerInstance(t *testing.T) {
	s := newTestStore(t)
	dep := heldFixture(t, s, "a", "b", "c")
	all := []string{"a", "b", "c"}

	// The operator sleeps a and c; the arbiter sleeps b.
	if err := s.OrderInstancesSleepHeld(dep.ID, []string{"a", "c"}, 2, proto.SleepModeWait, true); err != nil {
		t.Fatal(err)
	}
	if err := s.OrderInstancesSleep(dep.ID, []string{"b"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	wantHeld(t, "operator a,c + arbiter b", heldOf(t, s, all...), map[string][2]int{"a": {1, 2}, "b": {0, 1}, "c": {1, 2}})

	// The arbiter putting a held sleeper to sleep again leaves its hold.
	if err := s.OrderInstancesSleep(dep.ID, []string{"a"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	wantHeld(t, "arbiter re-sleeps a", heldOf(t, s, "a"), map[string][2]int{"a": {1, 1}})

	// The operator wakes a alone: b and c are as they were.
	if err := s.OrderInstancesSleep(dep.ID, []string{"a"}, 0, ""); err != nil {
		t.Fatal(err)
	}
	wantHeld(t, "wake a", heldOf(t, s, all...), map[string][2]int{"a": {0, 0}, "b": {0, 1}, "c": {1, 2}})

	// A deployment-wide sleep holds every instance, b too; a deployment-wide
	// wake lifts every hold.
	if err := s.OrderDeploymentSleep(dep.ID, 2, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	wantHeld(t, "deployment sleep", heldOf(t, s, all...), map[string][2]int{"a": {1, 2}, "b": {1, 2}, "c": {1, 2}})
	if err := s.SetDeploymentSleep(dep.ID, 0); err != nil {
		t.Fatal(err)
	}
	wantHeld(t, "deployment wake", heldOf(t, s, all...), map[string][2]int{"a": {0, 0}, "b": {0, 0}, "c": {0, 0}})
}

// A wake that waits for room lifts the hold of the instances it leaves
// waiting, and only theirs.
func TestAWaitingWakeLiftsTheHoldOfItsInstances(t *testing.T) {
	s := newTestStore(t)
	dep := heldFixture(t, s, "w-1", "w-2")
	if err := s.OrderDeploymentSleep(dep.ID, 2, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWakeWaiting(dep.ID, []string{"w-1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantHeld(t, "waiting wake", heldOf(t, s, "w-1", "w-2"), map[string][2]int{"w-1": {0, 2}, "w-2": {1, 2}})
	in, _ := s.InstanceByUUID("w-1")
	if in.WakeWaitingSince == nil {
		t.Fatalf("instance = %+v: the wait did not start", in)
	}
}

// An instance started while its deployment is under a deployment-wide
// sleep starts asleep and held; one started after the wake starts awake and
// unheld.
func TestANewInstanceTakesTheDeploymentWideHold(t *testing.T) {
	s := newTestStore(t)
	dep := heldFixture(t, s, "n-1")
	if err := s.OrderDeploymentSleep(dep.ID, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	create := func(uuid string) {
		t.Helper()
		raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true})
		if _, err := s.CreateInstance(&Instance{UUID: uuid, DeploymentID: dep.ID, State: proto.InstRunning, DesiredStateJSON: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	create("n-2")
	wantHeld(t, "started under the sleep", heldOf(t, s, "n-2"), map[string][2]int{"n-2": {1, 1}})
	if err := s.SetDeploymentSleep(dep.ID, 0); err != nil {
		t.Fatal(err)
	}
	create("n-3")
	wantHeld(t, "started after the wake", heldOf(t, s, "n-2", "n-3"), map[string][2]int{"n-2": {0, 0}, "n-3": {0, 0}})
}

// A held sleep the agent failed to carry out, rolled back awake, is no
// longer held: a later sleep by the arbiter must not read as the operator's.
func TestARolledBackHeldSleepIsNoLongerHeld(t *testing.T) {
	s := newTestStore(t)
	dep := heldFixture(t, s, "r-1")
	if err := s.OrderInstancesSleepHeld(dep.ID, []string{"r-1"}, 1, proto.SleepModeWait, true); err != nil {
		t.Fatal(err)
	}
	target := 1
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: "r-1", State: proto.InstRunning, Phase: proto.PhaseSleep, Message: "sleep failed", SleepTarget: &target}); err != nil {
		t.Fatal(err)
	}
	wantHeld(t, "rolled back", heldOf(t, s, "r-1"), map[string][2]int{"r-1": {0, 0}})
}

func TestWakesOnRequestIsPerSleeper(t *testing.T) {
	dep := &Deployment{Sleepable: true}
	held, free := Instance{SleepHeld: true}, Instance{}
	switch {
	case dep.WakesOnRequest(&held):
		t.Fatal("a held sleeper wakes on request")
	case !dep.WakesOnRequest(&free):
		t.Fatal("the platform's sleeper does not wake on request")
	case (&Deployment{}).WakesOnRequest(&free):
		t.Fatal("a deployment that cannot sleep wakes on request")
	case (*Deployment)(nil).WakesOnRequest(&free), dep.WakesOnRequest(nil):
		t.Fatal("nothing wakes on request")
	}
}

// openSleepHeld190 is a database as migration 186 left the held-sleep flag:
// on the deployment, with instances whose desired state says their sleep.
func openSleepHeld190(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db := openSchedulingCenterLegacy(t, name)
	if err := db.Exec(`ALTER TABLE instances ADD COLUMN spec TEXT`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{migrationByVersion(t, 186)}); err != nil {
		t.Fatal(err)
	}
	asleep := func(level int) string {
		raw, _ := json.Marshal(proto.InstanceDesiredState{DesiredState: "running", DesiredSleep: level})
		return string(raw)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		// 5 is held: its sleepers stay held, its awake one and one with no
		// desired state do not.
		{`INSERT INTO deployments (id, name, desired_state, sleepable, replicas, sleep_held) VALUES (5, 'held', 'active', 1, 3, 1)`, nil},
		{`INSERT INTO instances (id, uuid, deployment_id, state, sleep_level, spec) VALUES (51, 'h-asleep', 5, 'running', 2, ?)`, []any{asleep(2)}},
		{`INSERT INTO instances (id, uuid, deployment_id, state, sleep_level, spec) VALUES (52, 'h-awake', 5, 'running', 0, ?)`, []any{asleep(0)}},
		{`INSERT INTO instances (id, uuid, deployment_id, state, sleep_level, spec) VALUES (53, 'h-pending', 5, 'pending', 0, '')`, nil},
		// 6 sleeps by the platform's choice: nothing of it is held.
		{`INSERT INTO deployments (id, name, desired_state, sleepable, replicas, sleep_held) VALUES (6, 'free', 'active', 1, 1, 0)`, nil},
		{`INSERT INTO instances (id, uuid, deployment_id, state, sleep_level, spec) VALUES (61, 'f-asleep', 6, 'running', 1, ?)`, []any{asleep(1)}},
	} {
		if err := db.Exec(stmt.sql, stmt.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func readInstanceHolds(t *testing.T, db *gorm.DB) map[int]bool {
	t.Helper()
	var rows []struct {
		ID        int
		SleepHeld bool
	}
	if err := db.Raw(`SELECT id, sleep_held FROM instances ORDER BY id`).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[int]bool{}
	for _, r := range rows {
		out[r.ID] = r.SleepHeld
	}
	return out
}

func TestInstanceSleepHeldMigration(t *testing.T) {
	m := migrationByName(t, "instance_sleep_held")
	if m.Version != 191 {
		t.Fatalf("version = %d, want 191", m.Version)
	}
	ms := []migration{m}
	want := map[int]bool{1: false, 51: true, 52: false, 53: false, 61: false}
	check := func(db *gorm.DB, step string) {
		t.Helper()
		if ok, _ := tableHasColumn(db, "instances", "sleep_held"); !ok {
			t.Fatalf("%s: instances.sleep_held missing", step)
		}
		if ok, _ := tableHasColumn(db, "deployments", "sleep_held"); ok {
			t.Fatalf("%s: deployments.sleep_held still there", step)
		}
		got := readInstanceHolds(t, db)
		for id, w := range want {
			if got[id] != w {
				t.Errorf("%s: instance %d held = %v, want %v", step, id, got[id], w)
			}
		}
	}

	db := openSleepHeld190(t, "sleep-held-191")
	if err := migrateDB(db, ms); err != nil {
		t.Fatal(err)
	}
	check(db, "migrated")
	var marker SchemaMigration
	if err := db.First(&marker, 191).Error; err != nil || marker.Name != "instance_sleep_held" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 191").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}
	check(db, "rerun")

	rollback := openSleepHeld190(t, "sleep-held-191-rollback")
	boom := errors.New("injected failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	if ok, _ := tableHasColumn(rollback, "instances", "sleep_held"); ok {
		t.Fatal("the failed migration left instances.sleep_held behind")
	}
	if ok, _ := tableHasColumn(rollback, "deployments", "sleep_held"); !ok {
		t.Fatal("the failed migration dropped deployments.sleep_held")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = 191").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, ms); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	check(rollback, "after the restart")
}

func TestFreshDatabaseHoldsSleepOnInstancesOnly(t *testing.T) {
	s := newTestStore(t)
	if ok, _ := tableHasColumn(s.db, "deployments", "sleep_held"); ok {
		t.Error("deployments.sleep_held exists on a fresh database")
	}
	if ok, _ := tableHasColumn(s.db, "instances", "sleep_held"); !ok {
		t.Error("instances.sleep_held missing on a fresh database")
	}
}
