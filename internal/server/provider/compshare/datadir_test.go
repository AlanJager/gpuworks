package compshare

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostKeysPersistInTheDataDirOnlyOnceGiven(t *testing.T) {
	d := New()
	if err := d.hostKeys.Put("10.0.0.1:22", "SHA256:memory"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := d.UseDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := d.hostKeys.Put("10.0.0.2:22", "SHA256:disk"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "compshare-hostkeys.json"))
	if err != nil || !strings.Contains(string(raw), "SHA256:disk") {
		t.Fatalf("host keys file = %s, %v", raw, err)
	}
	again := New()
	if err := again.UseDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if fp, ok := again.hostKeys.Get("10.0.0.2:22"); !ok || fp != "SHA256:disk" {
		t.Fatalf("restart lost the trusted key: %q %v", fp, ok)
	}
	if err := os.WriteFile(filepath.Join(dir, "compshare-hostkeys.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := New().UseDataDir(dir); err == nil {
		t.Fatal("a damaged host key file was accepted")
	}
}
