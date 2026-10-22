package store

import (
	"fmt"

	"gorm.io/gorm"
)

// Dropping a column in a migration.
//
// Not through gorm's migrator: on this SQLite driver DropColumn rebuilds
// the table, and measured against nodes it came back with 0 of its 7 named
// indexes while the column itself was still there — and it returned no
// error for either (TestDropColumnVariants). A migration that trusted it
// would leave the schema unchanged and every node query in a 1000-node
// fleet scanning, with nothing functional to show for it.
//
// SQLite has had a real ALTER TABLE DROP COLUMN since 3.35: it rewrites the
// rows in place, keeps every index, and refuses outright if an index or
// constraint still names the column. That refusal is the useful one — it
// means a migration must drop such an index first and say so.

// dropColumn removes a column and verifies the table's indexes survived.
// table is the real table name; column is the column's own name, not the Go
// field's.
func dropColumn(db *gorm.DB, table, column string) error {
	before, err := indexDefinitions(db, table)
	if err != nil {
		return err
	}
	if err := db.Exec("ALTER TABLE " + quoteIdentifier(table) + " DROP COLUMN " + quoteIdentifier(column)).Error; err != nil {
		return fmt.Errorf("drop %s.%s: %w", table, column, err)
	}
	after, err := indexDefinitions(db, table)
	if err != nil {
		return err
	}
	// A guard rather than an expectation: if a future driver goes back to
	// rebuilding the table, the migration fails here instead of quietly
	// shipping a fleet-wide scan.
	if len(after) != len(before) {
		return fmt.Errorf("dropping %s.%s left %d of %d indexes; the driver rebuilt the table", table, column, len(after), len(before))
	}
	return nil
}

// indexDefinitions is each named index on the table, with the statement
// that creates it. Indexes SQLite generates for a UNIQUE column have no
// statement of their own and belong to the table definition, so they are
// not listed here.
func indexDefinitions(db *gorm.DB, table string) (map[string]string, error) {
	var rows []struct {
		Name string `gorm:"column:name"`
		SQL  string `gorm:"column:sql"`
	}
	if err := db.Raw(
		"SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL",
		table,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	defs := make(map[string]string, len(rows))
	for _, row := range rows {
		defs[row.Name] = row.SQL
	}
	return defs, nil
}

// quoteIdentifier wraps a table or column name for SQLite. Every caller is
// a migration naming a column of this repository's own schema, so a name
// containing a quote would be a bug here, not input.
func quoteIdentifier(name string) string {
	return "`" + name + "`"
}
