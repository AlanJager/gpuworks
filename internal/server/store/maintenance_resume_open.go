package store

import "gorm.io/gorm"

// The field cluster already recorded version 136. The marker has to stay in
// this executable or an established directory is refused at startup.
const (
	maintenanceResumeOpenNoDeleteTrigger = `CREATE TRIGGER maintenance_resume_open_no_delete
		BEFORE DELETE ON maintenance_resume_open
		BEGIN SELECT RAISE(ABORT, 'maintenance resume open must not be deleted'); END`
	maintenanceResumeOpenNoUpdateTrigger = `CREATE TRIGGER maintenance_resume_open_no_update
		BEFORE UPDATE ON maintenance_resume_open
		BEGIN SELECT RAISE(ABORT, 'maintenance resume open is immutable'); END`
	maintenanceResumeOpenNoReplaceTrigger = `CREATE TRIGGER maintenance_resume_open_no_replace
		BEFORE INSERT ON maintenance_resume_open
		WHEN EXISTS (SELECT 1 FROM maintenance_resume_open WHERE id = 1)
		BEGIN SELECT RAISE(ABORT, 'maintenance resume open already exists'); END`
)

func migrateMaintenanceResumeOpen(db *gorm.DB) error {
	if err := db.Exec(`CREATE TABLE maintenance_resume_open (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		run_id TEXT NOT NULL CHECK (length(run_id) > 0),
		install_dir TEXT NOT NULL CHECK (length(install_dir) > 0),
		binary_sha256 TEXT NOT NULL CHECK (length(binary_sha256) = 64),
		serving_revision INTEGER NOT NULL CHECK (serving_revision > 0),
		prior_phase TEXT NOT NULL CHECK (prior_phase = 'drained'),
		prior_revision INTEGER NOT NULL CHECK (prior_revision = serving_revision + 3),
		open_revision INTEGER NOT NULL CHECK (open_revision = prior_revision + 1),
		member1_id INTEGER NOT NULL CHECK (member1_id > 0),
		member1_key TEXT NOT NULL CHECK (length(member1_key) > 0),
		member2_id INTEGER NOT NULL CHECK (member2_id > member1_id),
		member2_key TEXT NOT NULL CHECK (length(member2_key) > 0),
		member3_id INTEGER NOT NULL CHECK (member3_id > member2_id),
		member3_key TEXT NOT NULL CHECK (length(member3_key) > 0)
	)`).Error; err != nil {
		return err
	}
	if err := db.Exec(maintenanceResumeOpenNoDeleteTrigger).Error; err != nil {
		return err
	}
	if err := db.Exec(maintenanceResumeOpenNoUpdateTrigger).Error; err != nil {
		return err
	}
	return db.Exec(maintenanceResumeOpenNoReplaceTrigger).Error
}

// schemaTrigger returns the text of a trigger this executable's migrations
// create. A copy of a database replays that text, never the source's, so a
// trigger of the same name with another body cannot bring SQL with it.
func schemaTrigger(name string) (string, bool) {
	if sql, ok := maintenancePhaseTrigger(name); ok {
		return sql, true
	}
	switch name {
	case "maintenance_resume_open_no_delete":
		return maintenanceResumeOpenNoDeleteTrigger, true
	case "maintenance_resume_open_no_update":
		return maintenanceResumeOpenNoUpdateTrigger, true
	case "maintenance_resume_open_no_replace":
		return maintenanceResumeOpenNoReplaceTrigger, true
	}
	return "", false
}
