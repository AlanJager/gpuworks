package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

const testHandle = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// signInTicketsDB is a database at the shape before migration 253:
// user_identities without its per-source index, and no sign_in_tickets.
func signInTicketsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := userIdentitiesDB(t)
	if err := migrateDB(db, []migration{migrationByName(t, "user_identities_and_disabled")}); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasIndex(&UserIdentity{}, "idx_user_identities_source") {
		// Migration 252 runs AutoMigrate on today's entity; drop the index
		// so the table has the shape 252 first gave it.
		if err := db.Migrator().DropIndex(&UserIdentity{}, "idx_user_identities_source"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestSignInTicketsMigrationAddsTheTableAndIndex(t *testing.T) {
	db := signInTicketsDB(t)
	m := migrationByName(t, "sign_in_tickets")
	if m.Version != 253 || LatestMigration() < m.Version {
		t.Fatalf("sign_in_tickets is migration %d, want 253 at or before the head (%d)", m.Version, LatestMigration())
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("sign_in_tickets") {
		t.Fatal("sign_in_tickets is missing")
	}
	for _, col := range []string{"TokenHash", "UserID", "IdentityID", "Source", "Handle", "CreatedAt", "ExpiresAt"} {
		if !db.Migrator().HasColumn(&SignInTicket{}, col) {
			t.Fatalf("sign_in_tickets.%s is missing", col)
		}
	}
	if !db.Migrator().HasIndex(&UserIdentity{}, "idx_user_identities_source") {
		t.Fatal("idx_user_identities_source is missing")
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d err=%v", markers, err)
	}
}

func TestSignInTicketsMigrationRollsBackItsStepAndMarker(t *testing.T) {
	db := signInTicketsDB(t)
	m := migrationByName(t, "sign_in_tickets")
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
	if db.Migrator().HasTable("sign_in_tickets") {
		t.Fatal("sign_in_tickets survived the rollback")
	}
	if db.Migrator().HasIndex(&UserIdentity{}, "idx_user_identities_source") {
		t.Fatal("idx_user_identities_source survived the rollback")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("markers=%d err=%v, want 0", markers, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("the retried migration failed: %v", err)
	}
}

func linkedUser(t *testing.T, st *Store, username, source, subject string) (*User, *UserIdentity) {
	t.Helper()
	u, link, err := st.CreateExternalUser(username, source, subject)
	if err != nil {
		t.Fatal(err)
	}
	return u, link
}

// A ticket signs its account in once, only with its handle; a wrong handle,
// a second use and an expired ticket are all IDENTITY_TICKET_INVALID, and a
// ticket tried with the wrong handle is spent.
func TestSignInTicketsAreBoundOneTimeAndShortLived(t *testing.T) {
	st := openUserIdentitiesStore(t)
	u, link := linkedUser(t, st, "alice", "acme", "corp:42")

	ticket, err := st.IssueSignInTicket(u.ID, link.ID, "acme", testHandle, time.Minute)
	if err != nil || len(ticket) < 40 {
		t.Fatalf("ticket=%q err=%v", ticket, err)
	}
	var stored SignInTicket
	if err := st.DB().First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.TokenHash == ticket || strings.Contains(stored.TokenHash, ticket) {
		t.Fatal("the ticket is stored in clear")
	}
	got, source, err := st.RedeemSignInTicket(ticket, testHandle)
	if err != nil || got.ID != u.ID || source != "acme" {
		t.Fatalf("redeem = %+v %q %v", got, source, err)
	}
	var again SignInTicket
	if err := st.DB().First(&again).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("the redeemed ticket is still stored: %v", err)
	}
	var signedIn UserIdentity
	if err := st.DB().First(&signedIn, link.ID).Error; err != nil || signedIn.LastSignInAt == nil {
		t.Fatalf("last_sign_in_at not set: %+v %v", signedIn, err)
	}
	if _, _, err := st.RedeemSignInTicket(ticket, testHandle); !errors.Is(err, ErrSignInTicketInvalid) {
		t.Fatalf("a second redemption = %v, want ErrSignInTicketInvalid", err)
	}

	stolen, err := st.IssueSignInTicket(u.ID, link.ID, "acme", testHandle, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("f", 64)
	if _, _, err := st.RedeemSignInTicket(stolen, other); !errors.Is(err, ErrSignInTicketInvalid) {
		t.Fatalf("redemption with another browser's handle = %v", err)
	}
	if _, _, err := st.RedeemSignInTicket(stolen, testHandle); !errors.Is(err, ErrSignInTicketInvalid) {
		t.Fatalf("a ticket tried in another browser was not spent: %v", err)
	}

	expired, err := st.IssueSignInTicket(u.ID, link.ID, "acme", testHandle, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&SignInTicket{}).Where("1 = 1").Update("expires_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemSignInTicket(expired, testHandle); !errors.Is(err, ErrSignInTicketInvalid) {
		t.Fatalf("an expired ticket = %v", err)
	}
	if _, _, err := st.RedeemSignInTicket("", testHandle); !errors.Is(err, ErrSignInTicketInvalid) {
		t.Fatalf("an empty ticket = %v", err)
	}
}

// A disabled account's ticket is refused as disabled; issuing sweeps the
// expired tickets, so the table holds only live ones.
func TestSignInTicketsRefuseDisabledAccountsAndAreSwept(t *testing.T) {
	st := openUserIdentitiesStore(t)
	u, link := linkedUser(t, st, "bob", "acme", "corp:7")
	ticket, err := st.IssueSignInTicket(u.ID, link.ID, "acme", testHandle, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DisableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemSignInTicket(ticket, testHandle); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("a disabled account's ticket = %v, want ErrUserDisabled", err)
	}
	if _, err := st.EnableUser(u.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.IssueSignInTicket(u.ID, link.ID, "acme", testHandle, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DB().Model(&SignInTicket{}).Where("1 = 1").Update("expires_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.IssueSignInTicket(u.ID, link.ID, "acme", testHandle, time.Minute); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := st.DB().Model(&SignInTicket{}).Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("tickets after a sweep = %d %v, want 1", n, err)
	}
}

// An external account is created with role user and no password, never by
// matching a username; a taken username or a taken identity creates nothing.
func TestExternalAccountsAreCreatedLinkedAndPaged(t *testing.T) {
	st := openUserIdentitiesStore(t)
	u, _ := linkedUser(t, st, "carol", "acme", "corp:1")
	if u.Role != "user" || u.PasswordHash != "" || u.LocalAdmin {
		t.Fatalf("external user = %+v", u)
	}
	if _, err := st.AuthenticateUser("carol", ""); err == nil {
		t.Fatal("an external account signed in with an empty password")
	}
	if _, _, err := st.CreateExternalUser("admin", "acme", "corp:2"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("a taken username = %v", err)
	}
	if _, _, err := st.LinkedAccount("acme", "corp:2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused account left a link: %v", err)
	}
	if _, _, err := st.CreateExternalUser("carol2", "acme", "corp:1"); !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("a taken identity = %v", err)
	}
	if err := st.RenameUser(u.ID, "admin"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("rename to a taken name = %v", err)
	}
	if err := st.RenameUser(u.ID, "carol.c"); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 5; i++ {
		linkedUser(t, st, fmt.Sprintf("user%d", i), "acme", fmt.Sprintf("corp:%d", i))
	}
	linkedUser(t, st, "elsewhere", "other", "x:1")
	var seen []string
	cursor := uint(0)
	for page := 0; ; page++ {
		rows, next, err := st.AccountsBySource("acme", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen = append(seen, r.Subject)
		}
		if next == 0 {
			break
		}
		if page > 5 {
			t.Fatal("paging does not end")
		}
		cursor = next
	}
	if strings.Join(seen, ",") != "corp:1,corp:2,corp:3,corp:4,corp:5" {
		t.Fatalf("pages = %v", seen)
	}
	if _, _, err := st.AccountsBySource("acme", 0, 0); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("limit 0 = %v", err)
	}
}

// Revoking a user's sessions ends them and nobody else's.
func TestRevokeUserSessionsEndsOnlyThatUsersSessions(t *testing.T) {
	st := openUserIdentitiesStore(t)
	u, _ := linkedUser(t, st, "dave", "acme", "corp:9")
	if _, _, err := st.CreateManagementSession(u, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	var admin User
	if err := st.DB().Where("username = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateManagementSession(&admin, time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	n, err := st.RevokeUserSessions(u.ID)
	if err != nil || n != 1 {
		t.Fatalf("revoked %d %v, want 1", n, err)
	}
	var left int64
	if err := st.DB().Model(&ManagementSession{}).Count(&left).Error; err != nil || left != 1 {
		t.Fatalf("sessions left = %d %v, want the administrator's", left, err)
	}
}

// Two sign-ins of the same new identity that both pass the reads meet the
// unique indexes: one creates the account, the other gets a typed error,
// never a raw constraint failure.
func TestConcurrentExternalSignUpsMeetTheIndexesAsTypedErrors(t *testing.T) {
	st := openUserIdentitiesStore(t)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, _, err := st.CreateExternalUser("same", "acme", "corp:same")
			errs <- err
		}()
	}
	created := 0
	for i := 0; i < 8; i++ {
		switch err := <-errs; {
		case err == nil:
			created++
		case errors.Is(err, ErrIdentityTaken), errors.Is(err, ErrUsernameTaken):
		default:
			t.Fatalf("a racing sign-up failed with %v", err)
		}
	}
	if created != 1 {
		t.Fatalf("created %d accounts, want 1", created)
	}
}
