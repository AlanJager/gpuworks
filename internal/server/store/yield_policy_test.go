package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

// withPolicy states the yield policy, with its defaults, on d.
func withPolicy(d *Deployment, yield string) *Deployment {
	p, _ := servicelevel.Parse(yield, servicelevel.Overrides{})
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
	mk := func(name, yield string, state DeploymentState) uint {
		t.Helper()
		dep := withPolicy(&Deployment{Name: name, Engine: "vllm", Sleepable: true, State: state}, yield)
		if _, err := s.CreateDeployment(dep); err != nil {
			t.Fatal(err)
		}
		return dep.ID
	}
	mk("never", servicelevel.Never, DeployActive)
	online := mk("idle", servicelevel.Idle, DeployActive)
	offline := mk("anytime", servicelevel.Anytime, DeployActive)
	mk("stopped", servicelevel.TimeSlice, DeployStopped)
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
	if err != nil || got.Policy() != (servicelevel.Policy{Yield: servicelevel.Never}) {
		t.Fatalf("a new deployment is never: %+v err=%v", got.Policy(), err)
	}
	// A policy stored with parameters that do not apply keeps none of them.
	stated := servicelevel.Policy{
		Yield: servicelevel.Anytime, IdleSec: 45, SleepLevel: servicelevel.LevelL2, MaxServeSec: 99,
		Unfinished: servicelevel.UnfinishedAbort, UnfinishedWaitSec: 12, HoldMaxSec: 200,
	}
	if err := s.SetDeploymentYieldPolicy(dep.ID, stated); err != nil {
		t.Fatal(err)
	}
	want := servicelevel.Policy{
		Yield: servicelevel.Anytime, IdleSec: 45, SleepLevel: servicelevel.LevelL2, Unfinished: servicelevel.UnfinishedAbort, HoldMaxSec: 200,
	}
	got, err = s.GetDeployment(dep.ID)
	if err != nil || got.Policy() != want || got.MaxServeSec != 0 || got.UnfinishedWaitSec != 0 {
		t.Fatalf("policy = %+v err=%v, want %+v", got.Policy(), err, want)
	}
	slice, _ := servicelevel.Parse(servicelevel.TimeSlice, servicelevel.Overrides{})
	if err := s.SetDeploymentYieldPolicy(dep.ID, slice); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetDeployment(dep.ID); got.Policy() != slice || got.MaxServeSec != 120 {
		t.Fatalf("timeslice not stored: %+v", got)
	}
}

// legacyYieldRow is a deployment's yield policy columns as migration 187
// leaves them.
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

func readLegacyYieldRows(t *testing.T, db *gorm.DB) map[int]legacyYieldRow {
	t.Helper()
	var ids []int
	if err := db.Raw(`SELECT id FROM deployments ORDER BY id`).Scan(&ids).Error; err != nil {
		t.Fatal(err)
	}
	out := map[int]legacyYieldRow{}
	for _, id := range ids {
		var row legacyYieldRow
		if err := db.Raw(`SELECT yield_policy, auto_sleep, idle_sec, sleep_level_pref, min_serve_sec, unfinished, unfinished_wait_sec, hold_max_sec
			FROM deployments WHERE id = ?`, id).Scan(&row).Error; err != nil {
			t.Fatal(err)
		}
		out[id] = row
	}
	return out
}

// The rows migration 187 makes of openYieldPolicyLegacy's.
var legacyYieldRows = map[int]legacyYieldRow{
	1:  {YieldPolicy: "never"}, // the row the legacy fixture started with
	10: {YieldPolicy: "never"},
	11: {YieldPolicy: "idle", IdleSec: 300, SleepLevelPref: "auto", HoldMaxSec: 120},
	12: {YieldPolicy: "timeslice", IdleSec: 60, SleepLevelPref: "auto", MinServeSec: 120, Unfinished: "wait", UnfinishedWaitSec: 30, HoldMaxSec: 600},
	13: {YieldPolicy: "anytime", SleepLevelPref: "auto", Unfinished: "abort", HoldMaxSec: 1800},
	// Idle sleep stays as auto sleep at its time and level; an idle
	// sleeper kept the gateway's two-minute hold.
	14: {YieldPolicy: "never", AutoSleep: true, IdleSec: 600, SleepLevelPref: "l1", HoldMaxSec: 120},
	15: {YieldPolicy: "idle", AutoSleep: true, IdleSec: 600, SleepLevelPref: "l2", HoldMaxSec: 120},
	16: {YieldPolicy: "anytime", AutoSleep: true, IdleSec: 30, SleepLevelPref: "l2", Unfinished: "abort", HoldMaxSec: 1800},
	17: {YieldPolicy: "never"},
	18: {YieldPolicy: "timeslice", AutoSleep: true, IdleSec: 90, SleepLevelPref: "l1", MinServeSec: 120, Unfinished: "wait", UnfinishedWaitSec: 30, HoldMaxSec: 600},
}

func TestDeploymentYieldPolicyMigrationConvertsServiceLevelsAndIdleSleep(t *testing.T) {
	m := migrationByVersion(t, 187)
	ms := []migration{m}
	db := openYieldPolicyLegacy(t, "yield-policy")
	if err := migrateDB(db, ms); err != nil {
		t.Fatal(err)
	}
	got := readLegacyYieldRows(t, db)
	for id, w := range legacyYieldRows {
		if got[id] != w {
			t.Errorf("deployment %d = %+v, want %+v", id, got[id], w)
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
	for id, p := range readLegacyYieldRows(t, db) {
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
	for id, p := range readLegacyYieldRows(t, rollback) {
		if p != got[id] {
			t.Errorf("after the restart deployment %d = %+v, want %+v", id, p, got[id])
		}
	}
}

// openYieldPolicy187 is openYieldPolicyLegacy after migration 187, with two
// more rows: a timeslice one with a short least turn and an idle time past
// it, and one that waits for its unfinished requests without bound.
func openYieldPolicy187(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db := openYieldPolicyLegacy(t, name)
	if err := migrateDB(db, []migration{migrationByVersion(t, 187)}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO deployments (id, name, desired_state, sleepable, replicas, yield_policy, idle_sec, sleep_level_pref, min_serve_sec, unfinished, unfinished_wait_sec, hold_max_sec)
			VALUES (19, 'd', 'active', 1, 1, 'timeslice', 60, 'auto', 10, 'wait_unbounded', 0, 600)`,
		`INSERT INTO deployments (id, name, desired_state, sleepable, replicas, yield_policy, idle_sec, sleep_level_pref, min_serve_sec, unfinished, unfinished_wait_sec, hold_max_sec)
			VALUES (20, 'd', 'active', 1, 1, 'anytime', 0, 'auto', 0, 'wait_unbounded', 0, 1800)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func readYieldRows(t *testing.T, db *gorm.DB) map[int]servicelevel.Policy {
	t.Helper()
	var rows []Deployment
	if err := db.Raw(`SELECT id, yield_policy, idle_sec, sleep_level_pref, max_serve_sec, unfinished, unfinished_wait_sec, hold_max_sec
		FROM deployments ORDER BY id`).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[int]servicelevel.Policy{}
	for _, d := range rows {
		out[int(d.ID)] = d.Policy()
	}
	return out
}

func TestDeploymentYieldPolicyReshapeMigration(t *testing.T) {
	m := migrationByVersion(t, 188)
	ms := []migration{m}
	db := openYieldPolicy187(t, "yield-reshape")
	if err := migrateDB(db, ms); err != nil {
		t.Fatal(err)
	}
	never := servicelevel.Policy{Yield: servicelevel.Never}
	want := map[int]servicelevel.Policy{
		1: never, 10: never, 17: never,
		11: {Yield: servicelevel.Idle, IdleSec: 300, SleepLevel: servicelevel.LevelAuto, HoldMaxSec: 120},
		// The least turn becomes the turn.
		12: {Yield: servicelevel.TimeSlice, IdleSec: 60, SleepLevel: servicelevel.LevelAuto, MaxServeSec: 120, Unfinished: servicelevel.UnfinishedWait, UnfinishedWaitSec: 30, HoldMaxSec: 600},
		// Anytime gets the idle time it had built in.
		13: {Yield: servicelevel.Anytime, IdleSec: 30, SleepLevel: servicelevel.LevelAuto, Unfinished: servicelevel.UnfinishedAbort, HoldMaxSec: 1800},
		// Auto sleep goes; a never deployment with it is plain never, the
		// others keep the idle time and level they had.
		14: never,
		15: {Yield: servicelevel.Idle, IdleSec: 600, SleepLevel: servicelevel.LevelL2, HoldMaxSec: 120},
		16: {Yield: servicelevel.Anytime, IdleSec: 30, SleepLevel: servicelevel.LevelL2, Unfinished: servicelevel.UnfinishedAbort, HoldMaxSec: 1800},
		18: {Yield: servicelevel.TimeSlice, IdleSec: 90, SleepLevel: servicelevel.LevelL1, MaxServeSec: 120, Unfinished: servicelevel.UnfinishedWait, UnfinishedWaitSec: 30, HoldMaxSec: 600},
		// A 10 s least turn is a 30 s turn, the idle time kept within it;
		// waiting without bound is waiting the longest that may be stated.
		19: {Yield: servicelevel.TimeSlice, IdleSec: 30, SleepLevel: servicelevel.LevelAuto, MaxServeSec: 30, Unfinished: servicelevel.UnfinishedWait, UnfinishedWaitSec: 600, HoldMaxSec: 600},
		20: {Yield: servicelevel.Anytime, IdleSec: 30, SleepLevel: servicelevel.LevelAuto, Unfinished: servicelevel.UnfinishedWait, UnfinishedWaitSec: 600, HoldMaxSec: 1800},
	}
	got := readYieldRows(t, db)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("deployment %d = %+v, want %+v", id, got[id], w)
		}
		if v := got[id].Validate(); v != nil {
			t.Errorf("deployment %d is not a valid policy: %+v", id, v)
		}
	}
	for _, column := range []string{"auto_sleep", "min_serve_sec"} {
		if ok, _ := tableHasColumn(db, "deployments", column); ok {
			t.Errorf("deployments.%s was not dropped", column)
		}
	}
	var marker SchemaMigration
	if err := db.First(&marker, 188).Error; err != nil {
		t.Fatalf("marker: %v", err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 188").Error; err != nil {
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

	rollback := openYieldPolicy187(t, "yield-reshape-rollback")
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
	if ok, _ := tableHasColumn(rollback, "deployments", "max_serve_sec"); ok {
		t.Fatal("the failed migration left max_serve_sec behind")
	}
	if ok, _ := tableHasColumn(rollback, "deployments", "auto_sleep"); !ok {
		t.Fatal("the failed migration dropped auto_sleep")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = 188").Count(&count).Error; err != nil || count != 0 {
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
	for _, column := range []string{"service_level", "idle_sleep_after_sec", "idle_sleep_level", "auto_sleep", "min_serve_sec"} {
		if ok, _ := tableHasColumn(s.db, "deployments", column); ok {
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

// A wake that waits for room lifts an operator's hold: the deployment is to
// be awake, so a request for it waits for the wake rather than being refused.
func TestAWaitingWakeLiftsTheHold(t *testing.T) {
	s := newTestStore(t)
	dep := &Deployment{Name: "held", Engine: "vllm", Sleepable: true, State: DeployActive, Replicas: 1}
	if _, err := s.CreateDeployment(dep); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&Instance{UUID: "h-1", DeploymentID: dep.ID, State: proto.InstRunning}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.OrderDeploymentSleepHeld(dep.ID, 2, proto.SleepModeWait, true); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWakeWaiting(dep.ID, []string{"h-1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeployment(dep.ID)
	if err != nil || got.SleepHeld {
		t.Fatalf("sleep_held = %v err=%v after a waiting wake, want false", got.SleepHeld, err)
	}
	in, _ := s.InstanceByUUID("h-1")
	if in.WakeWaitingSince == nil {
		t.Fatalf("instance = %+v: the wait did not start", in)
	}
}
