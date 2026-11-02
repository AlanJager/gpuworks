package store

import "gorm.io/gorm"

// inferenceSpecDisplayProfileColumn is the column migration 68 added to
// inference_specs and 197 dropped: a three-way "general / compatible /
// optimized" label the template page combined with the spec's GPU target
// into a name. It named many sibling specs alike, so specs now carry
// human-written names instead.
type inferenceSpecDisplayProfileColumn struct {
	DisplayProfile string `gorm:"size:16"`
}

func (inferenceSpecDisplayProfileColumn) TableName() string { return "inference_specs" }

// replaceInferenceSpecDisplayProfile adds the display name columns and drops
// display_profile. Existing rows get empty names: built-in specs are written
// again from the embedded catalog on startup, and a user spec without a name
// shows its spec name until it is edited.
func replaceInferenceSpecDisplayProfile(db *gorm.DB) error {
	if !db.Migrator().HasTable(&InferenceSpecRow{}) {
		return nil
	}
	for _, column := range []string{"DisplayNameZHCN", "DisplayNameEN"} {
		if db.Migrator().HasColumn(&InferenceSpecRow{}, column) {
			continue
		}
		if err := db.Migrator().AddColumn(&InferenceSpecRow{}, column); err != nil {
			return err
		}
	}
	if !db.Migrator().HasColumn(&inferenceSpecDisplayProfileColumn{}, "DisplayProfile") {
		return nil
	}
	return dropColumn(db, "inference_specs", "display_profile")
}
