package store

import "gorm.io/gorm"

// splitNetworkTestRunScopeDetail is migration 114. scope_detail held a
// label run's key=value and a nodes run's node list, which nodes already
// holds; the label becomes label_key and label_value. A label key cannot
// contain "=", so the first "=" separates key from value. A fresh database
// already has the new shape and no scope_detail, so nothing happens there.
func splitNetworkTestRunScopeDetail(db *gorm.DB) error {
	if !db.Migrator().HasTable("network_test_runs") {
		return nil
	}
	for _, column := range []string{"label_key", "label_value"} {
		present, err := tableHasColumn(db, "network_test_runs", column)
		if err != nil {
			return err
		}
		if !present {
			if err := db.Exec(`ALTER TABLE network_test_runs ADD COLUMN "` + column + `" text`).Error; err != nil {
				return err
			}
		}
	}
	hasDetail, err := tableHasColumn(db, "network_test_runs", "scope_detail")
	if err != nil || !hasDetail {
		return err
	}
	// A row whose label columns are already filled keeps them.
	if err := db.Exec(`UPDATE network_test_runs
		SET label_key = substr(scope_detail, 1, instr(scope_detail, '=') - 1),
			label_value = substr(scope_detail, instr(scope_detail, '=') + 1)
		WHERE scope = 'label' AND instr(scope_detail, '=') > 0
			AND COALESCE(label_key, '') = '' AND COALESCE(label_value, '') = ''`).Error; err != nil {
		return err
	}
	return dropColumns(db, "network_test_runs", "scope_detail")
}
