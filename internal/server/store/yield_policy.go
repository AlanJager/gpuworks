package store

import (
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

// Policy is the deployment's yield policy as its columns state it,
// normalized; the policy a row from before the columns reads as is never.
func (d *Deployment) Policy() servicelevel.Policy {
	return servicelevel.Policy{
		Yield: d.YieldPolicy, AutoSleep: d.AutoSleep, IdleSec: d.IdleSec, SleepLevel: d.SleepLevelPref,
		MinServeSec: d.MinServeSec, Unfinished: d.Unfinished, UnfinishedWaitSec: d.UnfinishedWaitSec, HoldMaxSec: d.HoldMaxSec,
	}.Normalize()
}

// SetPolicy states p in the deployment's columns (not saved), normalized.
func (d *Deployment) SetPolicy(p servicelevel.Policy) {
	p = p.Normalize()
	d.YieldPolicy, d.AutoSleep, d.IdleSec, d.SleepLevelPref = p.Yield, p.AutoSleep, p.IdleSec, p.SleepLevel
	d.MinServeSec, d.Unfinished, d.UnfinishedWaitSec, d.HoldMaxSec = p.MinServeSec, p.Unfinished, p.UnfinishedWaitSec, p.HoldMaxSec
}

// Preset is the service level the deployment's policy stands for, or custom.
func (d *Deployment) Preset() string { return servicelevel.PresetOf(d.Policy()) }

// yieldPolicyColumns are the columns of the yield policy, for a read that
// selects only some of a deployment's.
var yieldPolicyColumns = []string{
	"yield_policy", "auto_sleep", "idle_sec", "sleep_level_pref", "min_serve_sec", "unfinished", "unfinished_wait_sec", "hold_max_sec",
}

// SetDeploymentYieldPolicy stores the deployment's yield policy, normalized.
func (s *Store) SetDeploymentYieldPolicy(id uint, p servicelevel.Policy) error {
	p = p.Normalize()
	res := s.db.Model(&Deployment{}).Where("id = ?", id).Updates(map[string]any{
		"yield_policy": p.Yield, "auto_sleep": p.AutoSleep, "idle_sec": p.IdleSec, "sleep_level_pref": p.SleepLevel,
		"min_serve_sec": p.MinServeSec, "unfinished": p.Unfinished, "unfinished_wait_sec": p.UnfinishedWaitSec, "hold_max_sec": p.HoldMaxSec,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.Bus.PublishChange("deployment", uintKey(id), "yield-policy")
	return nil
}

// SharingDeployments are the active deployments that share cards by yield
// policy: every policy but never. The residency arbiter reads only these.
func (s *Store) SharingDeployments() ([]Deployment, error) {
	var out []Deployment
	err := s.reader().Where("desired_state = ? AND yield_policy NOT IN ?", DeployActive, []string{"", servicelevel.Never}).
		Order("id").Find(&out).Error
	return out, err
}

// AutoSleepDeployments is every active, sleepable deployment with auto sleep
// on, for the automation to check.
func (s *Store) AutoSleepDeployments() ([]Deployment, error) {
	var out []Deployment
	err := s.reader().Where("auto_sleep = ? AND sleepable = ? AND desired_state = ?", true, true, DeployActive).Order("id").Find(&out).Error
	return out, err
}

// deploymentServiceColumns are the columns migrations 180 and 184 added to
// deployments and 187 dropped, once the yield policy columns said the same:
// the old idle sleep (after how long, at which level) and the service level.
type deploymentServiceColumns struct {
	IdleSleepAfterSec int    `gorm:"not null;default:0"`
	IdleSleepLevel    int    `gorm:"not null;default:2"`
	ServiceLevel      string `gorm:"size:16;not null;default:'resident'"`
}

func (deploymentServiceColumns) TableName() string { return "deployments" }

// addDeploymentIdleSleep adds the per-deployment idle sleep setting. An
// existing deployment keeps idle sleep off, as a new one does.
func addDeploymentIdleSleep(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) {
		return nil
	}
	for _, column := range []string{"IdleSleepAfterSec", "IdleSleepLevel"} {
		if db.Migrator().HasColumn(&deploymentServiceColumns{}, column) {
			continue
		}
		if err := db.Migrator().AddColumn(&deploymentServiceColumns{}, column); err != nil {
			return err
		}
	}
	return nil
}

// addDeploymentServiceLevel adds the service level and the held-sleep flag.
// An existing deployment reads as resident, and one asleep now (ordered
// deployment-wide, or with a replica at a sleep level) is held: before the
// flag nothing woke a sleeper on request, so none is woken by one after it.
func addDeploymentServiceLevel(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) {
		return nil
	}
	added := false
	if !db.Migrator().HasColumn(&deploymentServiceColumns{}, "ServiceLevel") {
		if err := db.Migrator().AddColumn(&deploymentServiceColumns{}, "ServiceLevel"); err != nil {
			return err
		}
	}
	if !db.Migrator().HasColumn(&Deployment{}, "SleepHeld") {
		if err := db.Migrator().AddColumn(&Deployment{}, "SleepHeld"); err != nil {
			return err
		}
		added = true
	}
	if !added {
		return nil
	}
	held := `UPDATE deployments SET sleep_held = 1 WHERE desired_sleep > 0`
	if db.Migrator().HasTable(&Instance{}) && db.Migrator().HasColumn(&Instance{}, "SleepLevel") {
		held += ` OR id IN (SELECT deployment_id FROM instances WHERE sleep_level > 0)`
	}
	if !db.Migrator().HasColumn(&Deployment{}, "DesiredSleep") {
		return nil
	}
	return db.Exec(held).Error
}

// presetColumns are what migration 187 writes for each service level: the
// preset's parameters as they were when the migration was written (the
// presets of package servicelevel may change later; a migration may not).
var presetColumns = []struct {
	level                                  string
	yield                                  string
	idleSec, minServeSec, waitSec, holdSec int
	unfinished, sleepLevel                 string
}{
	{"resident", "never", 0, 0, 0, 0, "", ""},
	{"online", "idle", 300, 0, 0, 120, "", "auto"},
	{"shared", "timeslice", 60, 120, 30, 600, "wait", "auto"},
	{"offline", "anytime", 0, 0, 0, 1800, "abort", "auto"},
}

// addDeploymentYieldPolicy replaces the service level and the idle sleep
// with the yield policy and its parameters (docs/explanation/
// service-levels.md). Each service level becomes the policy and parameters
// it stood for. A deployment with idle sleep on keeps it as auto sleep, at
// the idle time and level it had (at least 30 seconds), which makes it a
// custom policy: the idle time then also serves its yields. The old columns
// go, so the policy has one source.
func addDeploymentYieldPolicy(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) {
		return nil
	}
	for _, column := range []string{"YieldPolicy", "AutoSleep", "IdleSec", "SleepLevelPref", "MinServeSec", "Unfinished", "UnfinishedWaitSec", "HoldMaxSec"} {
		if db.Migrator().HasColumn(&Deployment{}, column) {
			continue
		}
		if err := db.Migrator().AddColumn(&Deployment{}, column); err != nil {
			return err
		}
	}
	if db.Migrator().HasColumn(&deploymentServiceColumns{}, "service_level") {
		for _, p := range presetColumns {
			err := db.Exec(`UPDATE deployments SET yield_policy = ?, idle_sec = ?, sleep_level_pref = ?, min_serve_sec = ?,
				unfinished = ?, unfinished_wait_sec = ?, hold_max_sec = ? WHERE service_level = ?`,
				p.yield, p.idleSec, p.sleepLevel, p.minServeSec, p.unfinished, p.waitSec, p.holdSec, p.level).Error
			if err != nil {
				return err
			}
		}
		// A level the table does not know reads as resident, as it always did.
		if err := db.Exec(`UPDATE deployments SET yield_policy = 'never', idle_sec = 0, sleep_level_pref = '', min_serve_sec = 0,
			unfinished = '', unfinished_wait_sec = 0, hold_max_sec = 0
			WHERE service_level NOT IN ('resident', 'online', 'shared', 'offline')`).Error; err != nil {
			return err
		}
	}
	if db.Migrator().HasColumn(&deploymentServiceColumns{}, "idle_sleep_after_sec") {
		levelOf := `'l2'`
		if db.Migrator().HasColumn(&deploymentServiceColumns{}, "idle_sleep_level") {
			levelOf = `CASE idle_sleep_level WHEN 1 THEN 'l1' ELSE 'l2' END`
		}
		err := db.Exec(`UPDATE deployments SET auto_sleep = 1, idle_sec = MAX(idle_sleep_after_sec, 30),
			sleep_level_pref = ` + levelOf + `,
			hold_max_sec = CASE WHEN hold_max_sec = 0 THEN 120 ELSE hold_max_sec END
			WHERE idle_sleep_after_sec > 0`).Error
		if err != nil {
			return err
		}
	}
	for _, column := range []string{"service_level", "idle_sleep_after_sec", "idle_sleep_level"} {
		if db.Migrator().HasColumn(&deploymentServiceColumns{}, column) {
			if err := dropColumn(db, "deployments", column); err != nil {
				return err
			}
		}
	}
	return nil
}
