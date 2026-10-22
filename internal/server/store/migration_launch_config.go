package store

import "gorm.io/gorm"

// dropImageCatalogLaunchConfig is migration 112. The image catalog carries no
// launch configuration and instances keep no image-defaults snapshot
// (ADR 0031). The deployment's override column loses its RunSpec-era name.
func dropImageCatalogLaunchConfig(db *gorm.DB) error {
	if err := dropColumns(db, "image_versions", "env", "docker_args", "run_command", "model_mount_path", "extra_mounts", "entrypoint_display", "cmd_display", "inspect_synced", "command_mode"); err != nil {
		return err
	}
	if err := dropColumns(db, "instances", "image_defaults_snapshot", "run_spec_overrides"); err != nil {
		return err
	}
	hasOld, err := tableHasColumn(db, "deployments", "run_spec_overrides")
	if err != nil || !hasOld {
		return err
	}
	hasNew, err := tableHasColumn(db, "deployments", "container_overrides")
	if err != nil {
		return err
	}
	if !hasNew {
		return db.Exec(`ALTER TABLE deployments RENAME COLUMN run_spec_overrides TO container_overrides`).Error
	}
	// An upgrade that re-ran the baseline added the new column beside the
	// old one: keep whatever the old column held, then drop it.
	if err := db.Exec(`UPDATE deployments SET container_overrides = run_spec_overrides WHERE container_overrides IS NULL OR container_overrides = ''`).Error; err != nil {
		return err
	}
	return dropColumns(db, "deployments", "run_spec_overrides")
}

// tableHasColumn reports whether a SQLite table has the named column. A
// missing table has no columns.
func tableHasColumn(db *gorm.DB, table, column string) (bool, error) {
	var n int64
	err := db.Raw("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&n).Error
	return n > 0, err
}

// dropColumns drops each named column that exists, first dropping the indexes
// that cover it: SQLite refuses to drop an indexed column.
func dropColumns(db *gorm.DB, table string, columns ...string) error {
	for _, column := range columns {
		present, err := tableHasColumn(db, table, column)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		var indexes []string
		if err := db.Raw("SELECT DISTINCT il.name FROM pragma_index_list(?) AS il, pragma_index_info(il.name) AS ii WHERE ii.name = ? AND il.origin = 'c'", table, column).Scan(&indexes).Error; err != nil {
			return err
		}
		for _, index := range indexes {
			if err := db.Exec(`DROP INDEX IF EXISTS "` + index + `"`).Error; err != nil {
				return err
			}
		}
		if err := db.Exec(`ALTER TABLE "` + table + `" DROP COLUMN "` + column + `"`).Error; err != nil {
			return err
		}
	}
	return nil
}
