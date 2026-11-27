package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// userIdentitiesDB is a database with schema_migrations and a users table in
// the shape before migration 252: no disabled_at, no user_identities.
func userIdentitiesDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, password_hash TEXT, role TEXT, local_admin BOOLEAN NOT NULL DEFAULT 0, created_at DATETIME, updated_at DATETIME)").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func openUserIdentitiesStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open("file:"+t.Name()+"-open?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	return st
}

func insertUser(t *testing.T, st *Store, username, role string, localAdmin bool) *User {
	t.Helper()
	// The hash is copied from a row rather than by signing in: the caller may
	// have just disabled the administrator whose hash it is.
	var existing User
	if err := st.DB().First(&existing).Error; err != nil {
		t.Fatal(err)
	}
	u := &User{Username: username, PasswordHash: existing.PasswordHash, Role: role, LocalAdmin: localAdmin}
	if err := st.DB().Create(u).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

// The migration adds users.disabled_at and user_identities, changes nothing
// else, and re-applying it changes nothing and leaves one marker.
func TestUserIdentitiesMigrationAddsTheColumnAndTable(t *testing.T) {
	db := userIdentitiesDB(t)
	if err := db.Exec(`INSERT INTO users (id, username, role, local_admin) VALUES (1, 'admin', 'admin', 1)`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "user_identities_and_disabled")
	if m.Version != 252 || LatestMigration() < m.Version {
		t.Fatalf("user_identities_and_disabled is migration %d, want 252 at or before the head (%d)", m.Version, LatestMigration())
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&User{}, "DisabledAt") {
		t.Fatal("users.disabled_at is missing")
	}
	if !db.Migrator().HasTable("user_identities") {
		t.Fatal("user_identities is missing")
	}
	var disabled int64
	if err := db.Raw(`SELECT COUNT(*) FROM users WHERE disabled_at IS NOT NULL`).Scan(&disabled).Error; err != nil {
		t.Fatal(err)
	}
	if disabled != 0 {
		t.Fatalf("the migration disabled %d existing users", disabled)
	}
	// Applying it again leaves one marker and changes nothing.
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d err=%v", markers, err)
	}
}

// A failure after the schema change rolls back the change and the marker
// together: the next start repeats the whole step.
func TestUserIdentitiesMigrationRollsBackItsStepAndMarker(t *testing.T) {
	db := userIdentitiesDB(t)
	m := migrationByName(t, "user_identities_and_disabled")
	failing := m
	failing.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return fmt.Errorf("injected failure after the schema change")
	}
	if err := migrateDB(db, []migration{failing}); err == nil {
		t.Fatal("the injected failure did not fail the migration")
	}
	if db.Migrator().HasColumn(&User{}, "DisabledAt") {
		t.Fatal("users.disabled_at survived the rollback")
	}
	if db.Migrator().HasTable("user_identities") {
		t.Fatal("user_identities survived the rollback")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("markers=%d err=%v, want 0", markers, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("the retried migration failed: %v", err)
	}
}

func TestIdentitiesLinkFindAndUnlink(t *testing.T) {
	st := openUserIdentitiesStore(t)
	u := insertUser(t, st, "alice", "user", false)
	other := insertUser(t, st, "bob", "user", false)
	row, err := st.LinkIdentity(u.ID, "acme_oidc", "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	found, err := st.FindUserByIdentity("acme_oidc", "sub-1")
	if err != nil || found.ID != u.ID {
		t.Fatalf("find = %+v %v", found, err)
	}
	// A pair belongs to one account only, and is never found by a username.
	if _, err := st.LinkIdentity(other.ID, "acme_oidc", "sub-1"); !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("duplicate link = %v, want ErrIdentityTaken", err)
	}
	if _, err := st.FindUserByIdentity("acme_oidc", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find by username = %v, want ErrNotFound", err)
	}
	if _, err := st.LinkIdentity(u.ID, "Acme-OIDC", "sub-2"); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("invalid source = %v, want ErrInvalidIdentity", err)
	}
	// A link can only be removed through its own account.
	if err := st.UnlinkIdentity(other.ID, row.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlink through another user = %v, want ErrNotFound", err)
	}
	if err := st.UnlinkIdentity(u.ID, row.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ExternalIdentities(u.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("identities after unlink = %v %v", rows, err)
	}
}

func TestDisableDeletesSessionsAndRefusesEveryCredentialUntilEnabled(t *testing.T) {
	st := openUserIdentitiesStore(t)
	u := insertUser(t, st, "alice", "user", false)
	credential, _, err := st.CreateManagementSession(u, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.CreateAPIKey("alice-key", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreatePlatformToken(u.ID, "alice-token", "write", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.ResolveAPIKey(key); !ok {
		t.Fatal("the key did not verify before the disable")
	}
	if _, _, ok := st.VerifyPlatformToken(token); !ok {
		t.Fatal("the token did not verify before the disable")
	}

	if _, err := st.DisableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	// The session is gone at once.
	if _, _, err := st.ManagementSessionByHash(HashToken(credential)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled session = %v, want ErrNotFound", err)
	}
	// Sign-in says why.
	if _, err := st.AuthenticateUser("alice", "gpuworks-test-password-2026"); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("disabled sign-in = %v, want ErrUserDisabled", err)
	}
	// Cached credentials of this process were forgotten with the disable, so
	// these re-read and refuse rather than serving their TTL.
	if _, ok := st.ResolveAPIKey(key); ok {
		t.Fatal("a disabled user's API key still verifies")
	}
	if _, _, ok := st.VerifyPlatformToken(token); ok {
		t.Fatal("a disabled user's platform token still verifies")
	}
	// The data is kept: enabling restores everything.
	if _, err := st.EnableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticateUser("alice", "gpuworks-test-password-2026"); err != nil {
		t.Fatalf("enabled sign-in = %v", err)
	}
	if _, ok := st.ResolveAPIKey(key); !ok {
		t.Fatal("the key does not work after enabling")
	}
	if _, _, ok := st.VerifyPlatformToken(token); !ok {
		t.Fatal("the token does not work after enabling")
	}
}

func TestTheLastEnabledLocalAdministratorCannotBeDisabled(t *testing.T) {
	st := openUserIdentitiesStore(t)
	admin, err := st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DisableUser(admin.ID); !errors.Is(err, ErrLastLocalAdmin) {
		t.Fatalf("disable the only local admin = %v, want ErrLastLocalAdmin", err)
	}
	// With a second enabled local administrator, the first may go.
	second := insertUser(t, st, "admin2", "admin", true)
	if _, err := st.DisableUser(admin.ID); err != nil {
		t.Fatalf("disable with another local admin = %v", err)
	}
	// And the second is now the floor.
	if _, err := st.DisableUser(second.ID); !errors.Is(err, ErrLastLocalAdmin) {
		t.Fatalf("disable the remaining local admin = %v, want ErrLastLocalAdmin", err)
	}
	// An ordinary administrator is not the floor and may be disabled.
	plainAdmin := insertUser(t, st, "ops", "admin", false)
	if _, err := st.DisableUser(plainAdmin.ID); err != nil {
		t.Fatalf("disable an ordinary admin = %v", err)
	}
}
