package store

import "gorm.io/gorm"

// deploymentSleepHeldColumn is the column migration 186 added to deployments
// and 191 dropped, once each instance said whether its own sleep is held.
type deploymentSleepHeldColumn struct {
	SleepHeld bool `gorm:"not null;default:false"`
}

func (deploymentSleepHeldColumn) TableName() string { return "deployments" }

// instanceSleepHeldColumn is instances.sleep_held as migration 191 adds it.
type instanceSleepHeldColumn struct {
	SleepHeld bool `gorm:"not null;default:false"`
}

func (instanceSleepHeldColumn) TableName() string { return "instances" }

// moveSleepHeldToInstances makes a held sleep a fact of each instance. A
// deployment held before keeps every instance ordered asleep held (its
// desired state's desired_sleep above 0), and the deployment's column goes,
// so the hold has one source.
func moveSleepHeldToInstances(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Instance{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&instanceSleepHeldColumn{}, "SleepHeld") {
		if err := db.Migrator().AddColumn(&instanceSleepHeldColumn{}, "SleepHeld"); err != nil {
			return err
		}
	}
	if !db.Migrator().HasTable(&Deployment{}) {
		return nil
	}
	present, err := tableHasColumn(db, "deployments", "sleep_held")
	if err != nil || !present {
		return err
	}
	if ok, err := tableHasColumn(db, "instances", "spec"); err != nil {
		return err
	} else if ok {
		if err := db.Exec(`UPDATE instances SET sleep_held = 1
			WHERE deployment_id IN (SELECT id FROM deployments WHERE sleep_held = 1)
			AND json_valid(spec) AND COALESCE(json_extract(spec, '$.desired_sleep'), 0) > 0`).Error; err != nil {
			return err
		}
	}
	return dropColumns(db, "deployments", "sleep_held")
}
