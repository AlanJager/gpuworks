package store

import "gorm.io/gorm"

// addInferenceSpecPDRolesManualCommandAndDockerArgs adds the layout a PD-only
// spec is written for, the manual command a spec may own and the docker run
// options its engine needs (InferenceSpecRow.PDRoles, .ManualCommand,
// .DockerArgs). Existing specs state none, so all three read as "".
func addInferenceSpecPDRolesManualCommandAndDockerArgs(db *gorm.DB) error {
	if !db.Migrator().HasTable(&InferenceSpecRow{}) {
		return nil
	}
	for _, field := range []string{"PDRoles", "ManualCommand", "DockerArgs"} {
		if db.Migrator().HasColumn(&InferenceSpecRow{}, field) {
			continue
		}
		if err := db.Migrator().AddColumn(&InferenceSpecRow{}, field); err != nil {
			return err
		}
	}
	return db.Exec("UPDATE inference_specs SET pd_roles = COALESCE(pd_roles, ''), manual_command = COALESCE(manual_command, ''), docker_args = COALESCE(docker_args, '')").Error
}
