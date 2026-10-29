package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

// withPreset states the preset's policy on d.
func withPreset(d *Deployment, preset string) *Deployment {
	p, _ := servicelevel.PresetPolicy(preset)
	d.SetPolicy(p)
	return d
}

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
	if !db.Migrator().HasColumn(&deploymentServiceColumns{}, "ServiceLevel") || !db.Migrator().HasColumn(&Deployment{}, "SleepHeld") {
		t.Fatal("deployments.service_level or sleep_held missing")
	}
	// An existing deployment is resident and holds no sleep: nothing about
	// it changes.
	var dep struct {
		ServiceLevel string
		SleepHeld    bool
	}
	for id, wantHeld := range map[int]bool{1: true, 2: true, 3: false} {
		if err := db.Raw(`SELECT service_level, sleep_held FROM deployments WHERE id = ?`, id).Scan(&dep).Error; err != nil ||
			dep.ServiceLevel != "resident" || dep.SleepHeld != wantHeld {
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
	if rollback.Migrator().HasColumn(&deploymentServiceColumns{}, "service_level") || rollback.Migrator().HasColumn(&Deployment{}, "SleepHeld") {
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

func TestSharingDeploymentsSkipNeverAndStopped(t *testing.T) {
	s := newTestStore(t)
	mk := func(name, preset string, state DeploymentState) uint {
		t.Helper()
		dep := withPreset(&Deployment{Name: name, Engine: "vllm", Sleepable: true, State: state}, preset)
		if _, err := s.CreateDeployment(dep); err != nil {
			t.Fatal(err)
		}
		return dep.ID
	}
	mk("resident", servicelevel.PresetResident, DeployActive)
	online := mk("online", servicelevel.PresetOnline, DeployActive)
	offline := mk("offline", servicelevel.PresetOffline, DeployActive)
	mk("stopped", servicelevel.PresetShared, DeployStopped)
	got, err := s.SharingDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != online || got[1].ID != offline {
		t.Fatalf("sharing = %+v", got)
	}
	if err := s.SetDeploymentYieldPolicy(9999, servicelevel.Policy{Yield: servicelevel.Idle}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing deployment err = %v", err)
	}
}

func TestDeploymentPolicyRoundTripsNormalized(t *testing.T) {
	s := newTestStore(t)
	dep := &Deployment{Name: "p", Engine: "vllm", Sleepable: true, State: DeployActive}
	if _, err := s.CreateDeployment(dep); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeployment(dep.ID)
	if err != nil || got.Policy().Yield != servicelevel.Never || got.Preset() != servicelevel.PresetResident {
		t.Fatalf("a new deployment is resident: %+v err=%v", got.Policy(), err)
	}
	// A policy stored with parameters that do not apply keeps none of them.
	custom := servicelevel.Policy{
		Yield: servicelevel.Anytime, AutoSleep: true, IdleSec: 45, SleepLevel: servicelevel.LevelL2, MinServeSec: 99,
		Unfinished: servicelevel.UnfinishedWaitUnbounded, UnfinishedWaitSec: 12, HoldMaxSec: 200,
	}
	if err := s.SetDeploymentYieldPolicy(dep.ID, custom); err != nil {
		t.Fatal(err)
	}
	want := servicelevel.Policy{
		Yield: servicelevel.Anytime, AutoSleep: true, IdleSec: 45, SleepLevel: servicelevel.LevelL2,
		Unfinished: servicelevel.UnfinishedWaitUnbounded, HoldMaxSec: 200,
	}
	got, err = s.GetDeployment(dep.ID)
	if err != nil || got.Policy() != want || got.Preset() != servicelevel.PresetCustom {
		t.Fatalf("policy = %+v preset=%s err=%v, want %+v custom", got.Policy(), got.Preset(), err, want)
	}
	preset, _ := servicelevel.PresetPolicy(servicelevel.PresetShared)
	if err := s.SetDeploymentYieldPolicy(dep.ID, preset); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetDeployment(dep.ID); got.Preset() != servicelevel.PresetShared || got.MinServeSec != 120 || got.AutoSleep {
		t.Fatalf("shared preset not stored: %+v", got)
	}
}

func TestAutoSleepDeploymentsAreActiveSleepableOnes(t *testing.T) {
	s := newTestStore(t)
	mk := func(name string, sleepable, auto bool, state DeploymentState) uint {
		t.Helper()
		dep := &Deployment{Name: name, Engine: "vllm", Sleepable: sleepable, State: state}
		dep.SetPolicy(servicelevel.Policy{Yield: servicelevel.Never, AutoSleep: auto, IdleSec: 60})
		if _, err := s.CreateDeployment(dep); err != nil {
			t.Fatal(err)
		}
		return dep.ID
	}
	on := mk("on", true, true, DeployActive)
	mk("off", true, false, DeployActive)
	mk("not sleepable", false, true, DeployActive)
	mk("stopped", true, true, DeployStopped)
	got, err := s.AutoSleepDeployments()
	if err != nil || len(got) != 1 || got[0].ID != on {
		t.Fatalf("auto sleep deployments = %+v err=%v", got, err)
	}
}

// legacyYieldRow is a deployment as migration 187 leaves it.
type legacyYieldRow struct {
	YieldPolicy       string
	AutoSleep         bool
	IdleSec           int
	SleepLevelPref    string
	MinServeSec       int
	Unfinished        string
	UnfinishedWaitSec int
	HoldMaxSec        int
}

func (r legacyYieldRow) policy() servicelevel.Policy {
	d := Deployment{
		YieldPolicy: r.YieldPolicy, AutoSleep: r.AutoSleep, IdleSec: r.IdleSec, SleepLevelPref: r.SleepLevelPref, MinServeSec: r.MinServeSec,
		Unfinished: r.Unfinished, UnfinishedWaitSec: r.UnfinishedWaitSec, HoldMaxSec: r.HoldMaxSec,
	}
	return d.Policy()
}

// openYieldPolicyLegacy is a database that ran migrations 180 to 186 (the
// gpu3 demo's): service levels and idle sleep on deployments, one row per
// case.
func openYieldPolicyLegacy(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db := openSchedulingCenterLegacy(t, name)
	if err := migrateDB(db, []migration{migrationByVersion(t, 180), migrationByVersion(t, 184)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id        int
		level     string
		idle, lvl int
	}{
		{10, "resident", 0, 2}, {11, "online", 0, 2}, {12, "shared", 0, 2}, {13, "offline", 0, 2},
		{14, "resident", 600, 1}, {15, "online", 600, 2}, {16, "offline", 20, 2}, {17, "gold", 0, 2}, {18, "shared", 90, 1},
	} {
		if err := db.Exec(`INSERT INTO deployments (id, name, desired_state, sleepable, replicas, service_level, idle_sleep_after_sec, idle_sleep_level)
			VALUES (?, ?, 'active', 1, 1, ?, ?, ?)`, c.id, "d", c.level, c.idle, c.lvl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func readYieldRows(t *testing.T, db *gorm.DB) map[int]servicelevel.Policy {
	t.Helper()
	var ids []int
	if err := db.Raw(`SELECT id FROM deployments ORDER BY id`).Scan(&ids).Error; err != nil {
		t.Fatal(err)
	}
	out := map[int]servicelevel.Policy{}
	for _, id := range ids {
		var row legacyYieldRow
		if err := db.Raw(`SELECT yield_policy, auto_sleep, idle_sec, sleep_level_pref, min_serve_sec, unfinished, unfinished_wait_sec, hold_max_sec
			FROM deployments WHERE id = ?`, id).Scan(&row).Error; err != nil {
			t.Fatal(err)
		}
		out[id] = row.policy()
	}
	return out
}

func TestDeploymentYieldPolicyMigrationConvertsServiceLevelsAndIdleSleep(t *testing.T) {
	m := migrationByVersion(t, 187)
	ms := []migration{m}
	db := openYieldPolicyLegacy(t, "yield-policy")
	if err := migrateDB(db, ms); err != nil {
		t.Fatal(err)
	}
	preset := func(name string) servicelevel.Policy { p, _ := servicelevel.PresetPolicy(name); return p.Normalize() }
	autoSleep := func(p servicelevel.Policy, idle int, level string, hold int) servicelevel.Policy {
		p.AutoSleep, p.IdleSec, p.SleepLevel = true, idle, level
		p.HoldMaxSec = hold
		return p.Normalize()
	}
	want := map[int]servicelevel.Policy{
		1:  preset("resident"), // the row the legacy fixture started with
		10: preset("resident"), 11: preset("online"), 12: preset("shared"), 13: preset("offline"),
		// Idle sleep stays as auto sleep at its time and level; an idle
		// sleeper kept the gateway's two-minute hold.
		14: autoSleep(preset("resident"), 600, servicelevel.LevelL1, 120),
		15: autoSleep(preset("online"), 600, servicelevel.LevelL2, 120),
		16: autoSleep(preset("offline"), 30, servicelevel.LevelL2, 1800),
		17: preset("resident"),
		18: autoSleep(preset("shared"), 90, servicelevel.LevelL1, 600),
	}
	got := readYieldRows(t, db)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("deployment %d = %+v, want %+v", id, got[id], w)
		}
	}
	for id, preset := range map[int]string{10: "resident", 11: "online", 12: "shared", 13: "offline", 14: "custom", 15: "custom", 16: "custom", 17: "resident", 18: "custom"} {
		if p := servicelevel.PresetOf(got[id]); p != preset {
			t.Errorf("deployment %d preset = %s, want %s", id, p, preset)
		}
	}
	for _, column := range []string{"service_level", "idle_sleep_after_sec", "idle_sleep_level"} {
		if db.Migrator().HasColumn(&deploymentServiceColumns{}, column) {
			t.Errorf("deployments.%s was not dropped: two sources of the policy", column)
		}
	}
	var marker SchemaMigration
	if err := db.First(&marker, 187).Error; err != nil {
		t.Fatalf("marker: %v", err)
	}

	// Running it again, with or without the marker, changes nothing.
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 187").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}
	for id, p := range readYieldRows(t, db) {
		if p != got[id] {
			t.Errorf("deployment %d changed on rerun: %+v -> %+v", id, got[id], p)
		}
	}

	// A failure after the work leaves neither the new columns, the dropped
	// ones nor the marker; the restart then converts.
	rollback := openYieldPolicyLegacy(t, "yield-policy-rollback")
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
	if rollback.Migrator().HasColumn(&Deployment{}, "yield_policy") || !rollback.Migrator().HasColumn(&deploymentServiceColumns{}, "service_level") ||
		!rollback.Migrator().HasColumn(&deploymentServiceColumns{}, "idle_sleep_after_sec") {
		t.Fatal("the failed migration left its schema change behind")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = 187").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, ms); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	for id, p := range readYieldRows(t, rollback) {
		if p != got[id] {
			t.Errorf("after the restart deployment %d = %+v, want %+v", id, p, got[id])
		}
	}
}

func TestFreshDatabaseHasOnlyTheYieldPolicyColumns(t *testing.T) {
	s := newTestStore(t)
	for _, column := range []string{"service_level", "idle_sleep_after_sec", "idle_sleep_level"} {
		if s.db.Migrator().HasColumn(&deploymentServiceColumns{}, column) {
			t.Errorf("deployments.%s exists on a fresh database", column)
		}
	}
	for _, column := range yieldPolicyColumns {
		if !s.db.Migrator().HasColumn(&Deployment{}, column) {
			t.Errorf("deployments.%s missing", column)
		}
	}
}
// Placement reads the sharing deployments for every node it plans, so they
// are kept between deployment changes: a change the bus reports is seen at
// once, a row edited behind the bus's back only after sharingTTL.
func TestSharingDeploymentsAreKeptUntilADeploymentChanges(t *testing.T) {
	s := newTestStore(t)
	dep, err := s.CreateDeployment(&Deployment{Name: "a", ServedName: "a", Engine: "vllm", Sleepable: true, State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	read := func() int {
		t.Helper()
		got, err := s.SharingDeployments()
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if read() != 0 {
		t.Fatal("a never deployment shares")
	}
	// Behind the bus: still the kept rows.
	if err := s.DB().Model(&Deployment{}).Where("id = ?", dep.ID).Update("yield_policy", "anytime").Error; err != nil {
		t.Fatal(err)
	}
	if read() != 0 {
		t.Fatal("rows re-read without a reported change")
	}
	s.Bus.PublishChange("deployment", uintKey(dep.ID), "test")
	if read() != 1 {
		t.Fatal("a reported deployment change was not seen")
	}
	// The copy is the caller's.
	got, _ := s.SharingDeployments()
	got[0].Name = "changed"
	if again, _ := s.SharingDeployments(); again[0].Name != "a" {
		t.Fatal("a caller's edit reached the kept rows")
	}
}
