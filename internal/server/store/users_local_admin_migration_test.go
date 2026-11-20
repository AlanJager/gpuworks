package store

import (
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// usersOnlyDB is a database holding schema_migrations and a users table in
// the shape before migration 225, plus extraColumns.
func usersOnlyDB(t *testing.T, extraColumns string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	columns := "id INTEGER PRIMARY KEY, username TEXT, password_hash TEXT, role TEXT, created_at DATETIME, updated_at DATETIME"
	if extraColumns != "" {
		columns += ", " + extraColumns
	}
	if err := db.Exec("CREATE TABLE users (" + columns + ")").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func localAdmins(t *testing.T, db *gorm.DB) map[string]bool {
	t.Helper()
	var rows []struct {
		Username   string
		LocalAdmin bool
	}
	if err := db.Raw(`SELECT username, local_admin FROM users ORDER BY id`).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Username] = r.LocalAdmin
	}
	return out
}

// Every administrator an upgraded database holds becomes a local
// administrator; other users do not, and users added later start without
// the flag. Applying the change again changes nothing and leaves one marker.
func TestUsersLocalAdminMigrationBackfillsAdministrators(t *testing.T) {
	db := usersOnlyDB(t, "")
	if err := db.Exec(`INSERT INTO users (id, username, role) VALUES (1, 'admin', 'admin'), (2, 'alice', 'user'), (3, 'ops', 'admin')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "users_local_admin")
	if m.Version != 225 || LatestMigration() < m.Version {
		t.Fatalf("users_local_admin is migration %d, want 225 at or before the head (%d)", m.Version, LatestMigration())
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if got := localAdmins(t, db); len(got) != 3 || !got["admin"] || got["alice"] || !got["ops"] {
		t.Fatalf("local_admin after the migration = %v; want admin and ops only", got)
	}
	var col struct {
		NotNull   int    `gorm:"column:notnull"`
		DfltValue string `gorm:"column:dflt_value"`
	}
	if err := db.Raw(`SELECT "notnull", dflt_value FROM pragma_table_info('users') WHERE name = 'local_admin'`).Scan(&col).Error; err != nil || col.NotNull != 1 {
		t.Fatalf("local_admin column = %+v, error=%v; want NOT NULL", col, err)
	}
	if err := db.Exec(`INSERT INTO users (id, username, role) VALUES (4, 'bob', 'admin')`).Error; err != nil {
		t.Fatalf("a new row without the column must take the default: %v", err)
	}
	if got := localAdmins(t, db); got["bob"] {
		t.Fatal("an administrator added after the migration got the flag from the column default")
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("re-running the migration list: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d, error=%v", markers, err)
	}
}

// A database whose baseline already created the column from the current
// entity (a new database, or a legacy one the baseline caught up) still
// gets the backfill.
func TestUsersLocalAdminMigrationBackfillsWhenTheColumnExists(t *testing.T) {
	db := usersOnlyDB(t, "local_admin NUMERIC NOT NULL DEFAULT false")
	if err := db.Exec(`INSERT INTO users (id, username, role) VALUES (1, 'admin', 'admin'), (2, 'alice', 'user')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{migrationByName(t, "users_local_admin")}); err != nil {
		t.Fatal(err)
	}
	if got := localAdmins(t, db); !got["admin"] || got["alice"] {
		t.Fatalf("local_admin = %v; want admin only", got)
	}
}

// Schema, backfill and marker are one transaction: a failure leaves none of
// them, and the retry completes.
func TestUsersLocalAdminMigrationRollsBackAndCanRetry(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
	}{{"column added", ""}, {"column present", "local_admin NUMERIC NOT NULL DEFAULT false"}} {
		t.Run(tc.name, func(t *testing.T) {
			db := usersOnlyDB(t, tc.extra)
			if err := db.Exec(`INSERT INTO users (id, username, role) VALUES (1, 'admin', 'admin')`).Error; err != nil {
				t.Fatal(err)
			}
			m := migrationByName(t, "users_local_admin")
			if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_local_admin_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
				t.Fatal(err)
			}
			if err := migrateDB(db, []migration{m}); err == nil {
				t.Fatal("injected marker failure did not fail the migration")
			}
			if tc.extra == "" && db.Migrator().HasColumn("users", "local_admin") {
				t.Fatal("the column survived a failed migration")
			}
			if tc.extra != "" && localAdmins(t, db)["admin"] {
				t.Fatal("the backfill survived a failed migration")
			}
			var markers int64
			if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
				t.Fatalf("failed migration markers=%d, error=%v", markers, err)
			}
			if err := db.Exec(`DROP TRIGGER fail_local_admin_marker`).Error; err != nil {
				t.Fatal(err)
			}
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatalf("retry after the injected failure: %v", err)
			}
			if !localAdmins(t, db)["admin"] {
				t.Fatal("the retry did not backfill the administrator")
			}
		})
	}
}

// On a new database the bootstrapped administrator is a local
// administrator; a user created later is not. A password change from the
// console leaves the flag alone, and the offline recovery restores it for
// an administrator only.
func TestLocalAdminOnNewDatabaseAndPasswordRecovery(t *testing.T) {
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, created, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil || !created {
		t.Fatalf("EnsureAdmin = %v, %v", created, err)
	}
	if err := st.DB().Create(&User{Username: "ops", Role: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&User{Username: "alice", Role: "user"}).Error; err != nil {
		t.Fatal(err)
	}
	if got := localAdmins(t, st.DB()); !got["admin"] || got["ops"] || got["alice"] {
		t.Fatalf("local_admin on a new database = %v; want the bootstrapped admin only", got)
	}
	if err := st.SetUserPassword("ops", "ops-password-2026-x"); err != nil {
		t.Fatal(err)
	}
	if localAdmins(t, st.DB())["ops"] {
		t.Fatal("a password change from the console made a local administrator")
	}
	for _, user := range []string{"ops", "alice"} {
		if err := st.RecoverUserPassword(user, user+"-password-2026-y"); err != nil {
			t.Fatalf("recover %s: %v", user, err)
		}
	}
	if got := localAdmins(t, st.DB()); !got["ops"] || got["alice"] {
		t.Fatalf("local_admin after recovery = %v; want ops restored and alice unchanged", got)
	}
	if _, ok := st.VerifyUser("ops", "ops-password-2026-y"); !ok {
		t.Fatal("the recovered password does not log in")
	}
	if err := st.RecoverUserPassword("nobody", "nobody-password-2026"); err == nil {
		t.Fatal("recovering an unknown user succeeded")
	}
}
