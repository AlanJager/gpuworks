package app

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBootstrapAdminPasswordPrefersConfigThenEnvThenFile(t *testing.T) {
	dataDir := t.TempDir()
	if err := installcred.WriteInitialCredentials(dataDir, "admin", "from-file"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPUWORKS_ADMIN_PASSWORD", " from-env ")
	got, source := bootstrapAdminPassword(Config{DataDir: dataDir, AdminPassword: "from-config"})
	if got != "from-config" || source != "config" {
		t.Fatalf("bootstrapAdminPassword = (%q, %q), want the configured password", got, source)
	}
	got, source = bootstrapAdminPassword(Config{DataDir: dataDir})
	if got != "from-env" || source != "env" {
		t.Fatalf("bootstrapAdminPassword = (%q, %q), want the environment password", got, source)
	}
	t.Setenv("GPUWORKS_ADMIN_PASSWORD", "")
	got, source = bootstrapAdminPassword(Config{DataDir: dataDir})
	if got != "from-file" || source != "file" {
		t.Fatalf("bootstrapAdminPassword = (%q, %q), want the recorded password", got, source)
	}
	got, source = bootstrapAdminPassword(Config{DataDir: t.TempDir()})
	if got != "" || source != "generated" {
		t.Fatalf("bootstrapAdminPassword = (%q, %q), want a generated password", got, source)
	}
}

// TestEnsureAdminUsesInstallCredentials walks the install → first start handoff:
// install records the password, the server picks it up, and the operator can log
// in with it.
func TestEnsureAdminUsesInstallCredentials(t *testing.T) {
	dataDir := t.TempDir()
	if err := installcred.WriteInitialCredentials(dataDir, "admin", "install-chosen-password"); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dataDir, "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	password, _ := bootstrapAdminPassword(Config{DataDir: dataDir, AdminUser: "admin"})
	if _, initial, created, err := st.EnsureAdmin("admin", password); err != nil || !created {
		t.Fatalf("EnsureAdmin created=%v err=%v", created, err)
	} else if initial != "install-chosen-password" {
		t.Fatalf("initial password = %q, want the install-provided one", initial)
	}
	if _, ok := st.VerifyUser("admin", "install-chosen-password"); !ok {
		t.Fatal("the recorded password does not authenticate")
	}

	// A second start must not touch the existing account or its record.
	if err := installcred.WriteInitialCredentials(dataDir, "admin", "install-chosen-password"); err != nil {
		t.Fatal(err)
	}
	if _, _, created, err := st.EnsureAdmin("admin", "install-chosen-password"); err != nil || created {
		t.Fatalf("EnsureAdmin on an existing account created=%v err=%v", created, err)
	}
}
