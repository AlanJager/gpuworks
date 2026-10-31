package store

import (
	"slices"
	"sync"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// Policy is the deployment's yield policy as its columns state it,
// normalized; the policy a row from before the columns reads as is never.
func (d *Deployment) Policy() yieldpolicy.Policy {
	return yieldpolicy.Policy{
		Yield: d.YieldPolicy, IdleSec: d.IdleSec, SleepLevel: d.SleepLevelPref,
		MaxServeSec: d.MaxServeSec, Unfinished: d.Unfinished, UnfinishedWaitSec: d.UnfinishedWaitSec, HoldMaxSec: d.HoldMaxSec,
	}.Normalize()
}

// SetPolicy states p in the deployment's columns (not saved), normalized.
func (d *Deployment) SetPolicy(p yieldpolicy.Policy) {
	p = p.Normalize()
	d.YieldPolicy, d.IdleSec, d.SleepLevelPref = p.Yield, p.IdleSec, p.SleepLevel
	d.MaxServeSec, d.Unfinished, d.UnfinishedWaitSec, d.HoldMaxSec = p.MaxServeSec, p.Unfinished, p.UnfinishedWaitSec, p.HoldMaxSec
}

// yieldPolicyColumns are the columns of the yield policy, for a read that
// selects only some of a deployment's.
var yieldPolicyColumns = []string{
	"yield_policy", "idle_sec", "sleep_level_pref", "max_serve_sec", "unfinished", "unfinished_wait_sec", "hold_max_sec",
}

// SetDeploymentYieldPolicy stores the deployment's yield policy, normalized.
func (s *Store) SetDeploymentYieldPolicy(id uint, p yieldpolicy.Policy) error {
	p = p.Normalize()
	res := s.db.Model(&Deployment{}).Where("id = ?", id).Updates(map[string]any{
		"yield_policy": p.Yield, "idle_sec": p.IdleSec, "sleep_level_pref": p.SleepLevel,
		"max_serve_sec": p.MaxServeSec, "unfinished": p.Unfinished, "unfinished_wait_sec": p.UnfinishedWaitSec, "hold_max_sec": p.HoldMaxSec,
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
// policy: every policy but never. The residency arbiter reads only these, and
// placement reads them on every node it plans, so the rows are kept until
// the bus reports a deployment change or sharingTTL passes, whichever is
// first; the copy handed out is the caller's.
func (s *Store) SharingDeployments() ([]Deployment, error) {
	var generation uint64
	if s.Bus != nil {
		generation = s.Bus.Generation("deployment")
	}
	c := &s.sharing
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && c.generation == generation && time.Since(c.at) < sharingTTL {
		return slices.Clone(c.rows), nil
	}
	var out []Deployment
	err := s.reader().Where("desired_state = ? AND yield_policy NOT IN ?", DeployActive, []string{"", yieldpolicy.Never}).
		Order("id").Find(&out).Error
	if err != nil {
		return nil, err
	}
	c.rows, c.generation, c.at, c.ok = out, generation, time.Now().UTC(), true
	return slices.Clone(out), nil
}

// sharingTTL bounds how long SharingDeployments keeps its rows even with no
// deployment change reported: a write that published nothing is seen this
// late at most.
const sharingTTL = 5 * time.Second

// sharingCache is SharingDeployments' copy of its rows.
type sharingCache struct {
	mu         sync.Mutex
	rows       []Deployment
	generation uint64
	at         time.Time
	ok         bool
}

// deploymentServiceColumns are the columns migrations 182 and 186 added to
// deployments and 189 dropped, once the yield policy columns said the same:
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

// addDeploymentServiceLevel adds the service level and the held-sleep flag
// (191 moved the flag to the instances).
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
	if !db.Migrator().HasColumn(&deploymentSleepHeldColumn{}, "SleepHeld") {
		if err := db.Migrator().AddColumn(&deploymentSleepHeldColumn{}, "SleepHeld"); err != nil {
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

// presetColumns are what migration 189 writes for each service level: the
// preset's parameters as they were when the migration was written (the
// defaults of package yieldpolicy may change later; a migration may not).
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
		if db.Migrator().HasColumn(&deploymentYieldColumns{}, column) {
			continue
		}
		if err := db.Migrator().AddColumn(&deploymentYieldColumns{}, column); err != nil {
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

// deploymentYieldColumns are the yield policy columns as migration 189 added
// them; 190 dropped auto_sleep and min_serve_sec.
type deploymentYieldColumns struct {
	YieldPolicy       string `gorm:"size:16;not null;default:'never'"`
	AutoSleep         bool   `gorm:"not null;default:false"`
	IdleSec           int    `gorm:"not null;default:0"`
	SleepLevelPref    string `gorm:"size:8;not null;default:''"`
	MinServeSec       int    `gorm:"not null;default:0"`
	Unfinished        string `gorm:"size:16;not null;default:''"`
	UnfinishedWaitSec int    `gorm:"not null;default:0"`
	HoldMaxSec        int    `gorm:"not null;default:0"`
}

func (deploymentYieldColumns) TableName() string { return "deployments" }

// reshapeDeploymentYieldPolicy gives the yield policy the parameters every
// policy but never now shares (docs/explanation/service-levels.md):
//   - Auto sleep goes: a deployment sleeps for a waiter only. One that had
//     it keeps its policy, and a never one with it stays never.
//   - A timeslice deployment's least turn becomes its turn (max_serve_sec),
//     at least 30 seconds, and its idle time is kept within it.
//   - An anytime deployment gets the idle time it had built in, 30 seconds.
//   - Waiting without bound for unfinished requests becomes waiting the
//     longest that may be stated, 600 seconds.
//   - Idle times under 15 seconds cannot be, and holds over an hour neither.
func reshapeDeploymentYieldPolicy(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&Deployment{}, "MaxServeSec") {
		if err := db.Migrator().AddColumn(&Deployment{}, "MaxServeSec"); err != nil {
			return err
		}
	}
	steps := []string{
		`UPDATE deployments SET idle_sec = 0, sleep_level_pref = '', hold_max_sec = 0 WHERE yield_policy = 'never'`,
		`UPDATE deployments SET idle_sec = 30 WHERE yield_policy = 'anytime' AND idle_sec = 0`,
		`UPDATE deployments SET unfinished = 'wait', unfinished_wait_sec = 600 WHERE unfinished = 'wait_unbounded'`,
	}
	if db.Migrator().HasColumn(&deploymentYieldColumns{}, "min_serve_sec") {
		steps = append(steps, `UPDATE deployments SET max_serve_sec = MIN(MAX(min_serve_sec, 30), 86400) WHERE yield_policy = 'timeslice'`)
	}
	steps = append(steps,
		`UPDATE deployments SET max_serve_sec = 120 WHERE yield_policy = 'timeslice' AND max_serve_sec = 0`,
		`UPDATE deployments SET idle_sec = MAX(idle_sec, 15) WHERE yield_policy IN ('idle', 'timeslice', 'anytime')`,
		`UPDATE deployments SET idle_sec = max_serve_sec WHERE yield_policy = 'timeslice' AND idle_sec > max_serve_sec`,
		`UPDATE deployments SET hold_max_sec = 3600 WHERE hold_max_sec > 3600`,
	)
	for _, step := range steps {
		if err := db.Exec(step).Error; err != nil {
			return err
		}
	}
	return dropColumns(db, "deployments", "auto_sleep", "min_serve_sec")
}
