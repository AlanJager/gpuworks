package provision

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testSSHKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestTOFUHostKeyCallbackRecordsAndRejectsChanges(t *testing.T) {
	store := newMemoryHostKeyStore()
	first, second := testSSHKey(t), testSSHKey(t)
	callback := tofuHostKeyCallback(store, "host.example:22")

	if err := callback("host.example", nil, first); err != nil {
		t.Fatalf("first key: %v", err)
	}
	if err := callback("host.example", nil, first); err != nil {
		t.Fatalf("same key: %v", err)
	}
	if err := callback("host.example", nil, second); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("changed key error = %v", err)
	}
}

func TestFileHostKeyStorePersistsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ssh-hostkeys.json")
	store, err := NewFileHostKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key := testSSHKey(t)
	fingerprint := hostKeyFingerprint(key)
	if err := store.Put("host.example:22", fingerprint); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.Get("host.example:22"); !ok || got != fingerprint {
		t.Fatalf("stored fingerprint = %q/%v", got, ok)
	}
	reloaded, err := NewFileHostKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.Get("host.example:22"); !ok || got != fingerprint {
		t.Fatalf("reloaded fingerprint = %q/%v", got, ok)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("host-key store mode = %o, want 600", info.Mode().Perm())
	}
}

func TestNewFileHostKeyStoreRejectsMalformedData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh-hostkeys.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileHostKeyStore(path); err == nil {
		t.Fatal("malformed store should be rejected")
	}
}
