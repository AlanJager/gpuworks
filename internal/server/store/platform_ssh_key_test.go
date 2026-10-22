package store

import (
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

const testPlatformPEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nplatform-secret\n-----END OPENSSH PRIVATE KEY-----\n"

func platformKeyStore(t *testing.T) *Store {
	t.Helper()
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("platform-key-test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	return st
}

func TestPlatformSSHKeyIsEncryptedAndReplacedInPlace(t *testing.T) {
	st := platformKeyStore(t)
	if _, _, err := st.PlatformSSHKey(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before any key err = %v, want ErrNotFound", err)
	}
	row, err := st.SetPlatformSSHKey(testPlatformPEM, "ssh-ed25519 AAAA gpuworks@a", "SHA256:first")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.PrivateKeyCipher, "platform-secret") {
		t.Fatalf("private key stored in plaintext: %q", row.PrivateKeyCipher)
	}
	got, pem, err := st.PlatformSSHKey()
	if err != nil || pem != testPlatformPEM || got.PublicKey != "ssh-ed25519 AAAA gpuworks@a" || got.Fingerprint != "SHA256:first" || got.CreatedAt.IsZero() {
		t.Fatalf("round trip row=%+v pem=%q err=%v", got, pem, err)
	}

	if _, err := st.SetPlatformSSHKey(strings.Replace(testPlatformPEM, "platform-secret", "second", 1), "ssh-ed25519 BBBB gpuworks@a", "SHA256:second"); err != nil {
		t.Fatal(err)
	}
	var rows int64
	if err := st.DB().Model(&PlatformSSHKey{}).Count(&rows).Error; err != nil || rows != 1 {
		t.Fatalf("rows after replace = %d err=%v", rows, err)
	}
	got, pem, _ = st.PlatformSSHKey()
	if !strings.Contains(pem, "second") || got.Fingerprint != "SHA256:second" {
		t.Fatalf("after replace row=%+v pem=%q", got, pem)
	}

	if _, err := st.SetPlatformSSHKey("", "ssh-ed25519 CCCC", "SHA256:x"); err == nil {
		t.Fatal("empty private key accepted")
	}
}

func TestPlatformSSHKeyNeedsEncryption(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	if _, err := st.SetPlatformSSHKey(testPlatformPEM, "ssh-ed25519 AAAA", "SHA256:x"); err == nil || !strings.Contains(err.Error(), "encryption") {
		t.Fatalf("set without secretbox err = %v", err)
	}
	if _, _, err := st.PlatformSSHKey(); err == nil || !strings.Contains(err.Error(), "encryption") {
		t.Fatalf("get without secretbox err = %v", err)
	}
}

var platformKeyMigration = migration{Version: 128, Name: "platform_ssh_keys", Apply: func(db *gorm.DB) error {
	return db.AutoMigrate(&PlatformSSHKey{})
}}

// beforePlatformKeys rewinds a migrated database to migration 127.
func beforePlatformKeys(t *testing.T) *gorm.DB {
	t.Helper()
	db := platformKeyStore(t).DB()
	if err := db.Migrator().DropTable(&PlatformSSHKey{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("version = ?", 128).Delete(&SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPlatformSSHKeyMigrationCreatesTheTableOnce(t *testing.T) {
	db := beforePlatformKeys(t)
	if err := migrateDB(db, []migration{platformKeyMigration}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"id", "private_key_cipher", "public_key", "fingerprint", "created_at"} {
		if !db.Migrator().HasColumn(&PlatformSSHKey{}, column) {
			t.Fatalf("platform_ssh_keys lacks %s", column)
		}
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", 128, "platform_ssh_keys").Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers = %d err=%v", markers, err)
	}
	// Rerunning is a no-op: the marker is already there.
	if err := migrateDB(db, []migration{platformKeyMigration}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 128).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers after rerun = %d err=%v", markers, err)
	}
}

func TestPlatformSSHKeyMigrationRollsBackWithItsMarker(t *testing.T) {
	db := beforePlatformKeys(t)
	boom := errors.New("injected")
	failing := migration{Version: 128, Name: "platform_ssh_keys", Apply: func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(&PlatformSSHKey{}); err != nil {
			return err
		}
		return boom
	}}
	if err := migrateDB(db, []migration{failing}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if db.Migrator().HasTable(&PlatformSSHKey{}) {
		t.Fatal("table survived a failed migration")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 128).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("markers = %d err=%v", markers, err)
	}
	if err := migrateDB(db, []migration{platformKeyMigration}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable(&PlatformSSHKey{}) {
		t.Fatal("rerun did not create the table")
	}
}
