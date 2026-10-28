package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDeploymentServiceLevelMigration(t *testing.T) {
	m := migrationByVersion(t, 184)
	ms := []migration{m}
	db := openSchedulingCenterLegacy(t, "service-level")
	// Deployment 1's instance is at sleep level 2 (an operator slept a
	// replica); 2 is ordered asleep deployment-wide; 3 is awake.
	for _, stmt := range []string{
		`ALTER TABLE deployments ADD COLUMN desired_sleep INTEGER NOT NULL DEFAULT 0`,
		`INSERT INTO deployments (id, name, desired_state, sleepable, replicas, desired_sleep) VALUES (2, 'ordered', 'active', 1, 1, 1)`,
		`INSERT INTO deployments (id, name, desired_state, sleepable, replicas, desired_sleep) VALUES (3, 'awake', 'active', 1, 1, 0)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"ServiceLevel", "SleepHeld"} {
		if !db.Migrator().HasColumn(&Deployment{}, column) {
			t.Fatalf("deployments.%s missing", column)
		}
	}
	// An existing deployment is resident and holds no sleep: nothing about
	// it changes.
	var dep struct {
		ServiceLevel string
		SleepHeld    bool
	}
	for id, wantHeld := range map[int]bool{1: true, 2: true, 3: false} {
		if err := db.Raw(`SELECT service_level, sleep_held FROM deployments WHERE id = ?`, id).Scan(&dep).Error; err != nil ||
			dep.ServiceLevel != ServiceLevelResident || dep.SleepHeld != wantHeld {
			t.Fatalf("existing deployment %d = %+v err=%v, want resident held=%v: a sleep from before the upgrade is not woken by a request", id, dep, err, wantHeld)
		}
	}
	var marker SchemaMigration
	if err := db.First(&marker, 184).Error; err != nil {
		t.Fatalf("marker: %v", err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 184").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := openSchedulingCenterLegacy(t, "service-level-rollback")
	want := errors.New("injected failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Deployment{}, "ServiceLevel") || rollback.Migrator().HasColumn(&Deployment{}, "SleepHeld") {
		t.Fatal("columns survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = 184").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, ms); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
}

func TestSleepHeldFollowsWhoOrderedTheSleep(t *testing.T) {
	s := newTestStore(t)
	dep := &Deployment{Name: "held", Engine: "vllm", Sleepable: true, State: DeployActive, Replicas: 2}
	if _, err := s.CreateDeployment(dep); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"i-1", "i-2"} {
		if err := s.db.Create(&Instance{UUID: uuid, DeploymentID: dep.ID, State: proto.InstRunning}).Error; err != nil {
			t.Fatal(err)
		}
	}
	held := func() bool {
		t.Helper()
		got, err := s.GetDeployment(dep.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.SleepHeld
	}

	if err := s.OrderDeploymentSleepHeld(dep.ID, 1, proto.SleepModeWait, true); err != nil || !held() {
		t.Fatalf("an operator's sleep is held: held=%v err=%v", held(), err)
	}
	if err := s.SetDeploymentSleep(dep.ID, 0); err != nil || held() {
		t.Fatalf("a wake lifts the hold: held=%v err=%v", held(), err)
	}
	if err := s.OrderDeploymentSleep(dep.ID, 2, proto.SleepModeWait); err != nil || held() {
		t.Fatalf("the platform's sleep is not held: held=%v err=%v", held(), err)
	}
	if err := s.OrderInstancesSleepHeld(dep.ID, []string{"i-1"}, 1, proto.SleepModeWait, true); err != nil || !held() {
		t.Fatalf("an operator's replica sleep holds: held=%v err=%v", held(), err)
	}
	// The platform putting another replica to sleep does not lift the
	// operator's hold.
	if err := s.OrderInstancesSleep(dep.ID, []string{"i-2"}, 1, proto.SleepModeWait); err != nil || !held() {
		t.Fatalf("a platform sleep lifted the hold: held=%v err=%v", held(), err)
	}
	if err := s.OrderInstancesSleep(dep.ID, []string{"i-1"}, 0, ""); err != nil || held() {
		t.Fatalf("a replica wake lifts the hold: held=%v err=%v", held(), err)
	}
}

func TestSharingDeploymentsSkipResidentAndStopped(t *testing.T) {
	s := newTestStore(t)
	mk := func(name, level string, state DeploymentState) uint {
		t.Helper()
		dep := &Deployment{Name: name, Engine: "vllm", Sleepable: true, State: state}
		if _, err := s.CreateDeployment(dep); err != nil {
			t.Fatal(err)
		}
		if level != ServiceLevelResident {
			if err := s.SetDeploymentServiceLevel(dep.ID, level); err != nil {
				t.Fatal(err)
			}
		}
		return dep.ID
	}
	mk("resident", ServiceLevelResident, DeployActive)
	online := mk("online", ServiceLevelOnline, DeployActive)
	offline := mk("offline", ServiceLevelOffline, DeployActive)
	mk("stopped", ServiceLevelShared, DeployStopped)
	got, err := s.SharingDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != online || got[1].ID != offline {
		t.Fatalf("sharing = %+v", got)
	}
	if err := s.SetDeploymentServiceLevel(9999, ServiceLevelOnline); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing deployment err = %v", err)
	}
}
