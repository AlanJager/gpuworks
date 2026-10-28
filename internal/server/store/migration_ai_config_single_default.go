package store

import "gorm.io/gorm"

// addAIConfigSingleDefault makes "exactly one default while any service is
// stored" a property of the schema, not a comment.
//
// Two builds wrote defaults the code alone guaranteed: one marked the first
// service default and nothing else, so deleting it left every stored service
// unmarked, and the read path then fell back to the OLDEST row while the
// console listed the NEWEST first - the two disagreed about which provider the
// assistant used. A database written by either build may be in that state
// today, so the repair runs before the index: the newest service becomes the
// default when none is marked (that is the row the console shows first, so what
// the operator sees after an upgrade is what the assistant uses), and when
// several are marked the newest keeps the mark.
//
// The index is partial because unmarked rows are the normal case for every
// service that is not in use, and SQLite only allows the uniqueness that matters
// (at most one is_default = 1) with a WHERE clause. The writer pool is a single
// connection, so create, update and delete already serialize; the index is what
// keeps a future write path - or a manual UPDATE - from silently creating the
// second default that the old fallback existed to paper over.
func addAIConfigSingleDefault(db *gorm.DB) error {
	if !db.Migrator().HasTable(&AIConfig{}) {
		return nil
	}
	var rows []AIConfig
	if err := db.Order("id desc").Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		// Nothing stored: no default to repair, and the index still applies to
		// whatever is created later.
		return createAIConfigSingleDefaultIndex(db)
	}
	var marked []AIConfig
	for _, row := range rows {
		if row.IsDefault {
			marked = append(marked, row)
		}
	}
	keep := rows[0] // the newest
	switch {
	case len(marked) == 0:
		// No default: the assistant was falling back to the oldest row without
		// the console marking anything. Promote the newest, which is the one the
		// console lists first and the one a delete would have promoted.
		if err := db.Model(&AIConfig{}).Where("id = ?", keep.ID).Update("is_default", true).Error; err != nil {
			return err
		}
	case len(marked) == 1:
		keep = marked[0]
	}
	// Several marked: the newest keeps it, and the others lose it. The clears
	// come first so the unique index - already present on a re-run - never sees
	// two rows marked at once.
	if err := db.Model(&AIConfig{}).Where("is_default = ? AND id <> ?", true, keep.ID).Update("is_default", false).Error; err != nil {
		return err
	}
	return createAIConfigSingleDefaultIndex(db)
}

func createAIConfigSingleDefaultIndex(db *gorm.DB) error {
	return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_configs_single_default ON ai_configs (is_default) WHERE is_default = 1").Error
}
