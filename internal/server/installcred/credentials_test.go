package installcred

import (
	"os"
	"testing"
)

// TestRoundTrip pins the install → server handoff: the file the installer
// writes is the file the server reads, 0600 because it holds a clear-text
// password.
func TestRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	if err := WriteInitialCredentials(dataDir, "admin", "s3cret-token"); err != nil {
		t.Fatal(err)
	}
	path := CredentialsPath(dataDir)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o664 {
		t.Fatalf("credentials mode = %v, want 0664 (the server reads it as the service account)", info.Mode().Perm())
	}
	creds, err := ReadInitialCredentials(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if creds.User != "admin" || creds.Password != "s3cret-token" {
		t.Fatalf("credentials = %+v", creds)
	}
	if creds.CreatedAt == "" {
		t.Fatal("CreatedAt is empty")
	}
}

func TestWriteRejectsIncompleteRecords(t *testing.T) {
	dataDir := t.TempDir()
	if err := WriteInitialCredentials(dataDir, "admin", ""); err == nil {
		t.Fatal("empty password accepted")
	}
	if err := WriteInitialCredentials(dataDir, "", "pw"); err == nil {
		t.Fatal("empty user accepted")
	}
	if _, err := os.Stat(CredentialsPath(dataDir)); err == nil {
		t.Fatal("a rejected record must not be written")
	}
}

// TestReadInitialCredentialsToleratesDamage pins that a hand-edited or
// truncated file degrades to "no credentials" instead of blocking startup.
func TestReadToleratesDamage(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := ReadInitialCredentials(dataDir); err == nil {
		t.Fatal("missing file must be an error")
	}
	for name, payload := range map[string]string{
		"truncated":       `{"user":"admin","passw`,
		"no-password":     `{"user":"admin"}`,
		"empty-password":  `{"user":"admin","password":""}`,
		"plain-text-file": "admin:hunter2\n",
	} {
		if err := os.WriteFile(CredentialsPath(dataDir), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadInitialCredentials(dataDir); err == nil {
			t.Fatalf("%s: read succeeded, want error", name)
		}
	}
}
