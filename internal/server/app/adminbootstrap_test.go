package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/installcred"
)

// newTestApp builds an App against a throwaway data dir and returns the log it
// wrote during bootstrap.
func newTestApp(t *testing.T, adminPassword string) (*App, string) {
	t.Helper()
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))

	dir := t.TempDir()
	cfg := Config{
		Addr:          "127.0.0.1:0",
		DataDir:       dir,
		DBPath:        filepath.Join(dir, "gpuworks.db"),
		AdminUser:     "admin",
		AdminPassword: adminPassword,
	}
	application, err := New(cfg, log)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	// New starts the proxy's route-table refresh goroutine outside of Run (see
	// Run's own defer a.proxy.Close()); this helper never calls Run, so it must
	// stop that goroutine itself or it leaks for the rest of the test binary -
	// ticking against this test's since-removed temp-dir database on every
	// later test and -count iteration.
	t.Cleanup(func() { application.proxy.Close() })
	return application, logs.String()
}

// The bootstrap log must distinguish a generated password from one the operator
// supplied. It previously derived that from EnsureAdmin's return value, which is
// the password it *used* — non-empty either way — so an explicitly configured
// password was still reported as generated, and operators reasonably concluded
// their setting had been ignored.
func TestAdminBootstrapLogDistinguishesConfiguredPassword(t *testing.T) {
	t.Run("configured password", func(t *testing.T) {
		_, out := newTestApp(t, "operator-chosen-password")

		if !strings.Contains(out, "configured password") {
			t.Errorf("log does not confirm the configured password was used:\n%s", out)
		}
		if strings.Contains(out, "generated password") {
			t.Errorf("log claims the password was generated despite one being configured:\n%s", out)
		}
		// The password itself must never reach the log.
		if strings.Contains(out, "operator-chosen-password") {
			t.Errorf("log leaked the admin password:\n%s", out)
		}
	})

	t.Run("generated password", func(t *testing.T) {
		application, out := newTestApp(t, "")

		if !strings.Contains(out, "generated password") {
			t.Errorf("log does not report that a password was generated:\n%s", out)
		}
		creds, err := installcred.ReadInitialCredentials(application.cfg.DataDir)
		if err != nil {
			t.Fatalf("read generated credentials: %v", err)
		}
		if err := auth.ValidatePassword(creds.Password); err != nil {
			t.Fatalf("generated credentials violate password policy: %v", err)
		}
		if strings.Contains(out, creds.Password) || strings.Contains(out, "password=") {
			t.Error("server log exposes the generated password")
		}
		info, err := os.Stat(installcred.CredentialsPath(application.cfg.DataDir))
		if err != nil || info.Mode().Perm()&0o007 != 0 || info.Mode().Perm()&0o600 != 0o600 {
			t.Fatalf("generated credentials file is not private: info=%v err=%v", info, err)
		}
		if _, ok := application.store.VerifyUser("admin", creds.Password); !ok {
			t.Error("private credentials file does not authenticate the generated admin")
		}
		if !strings.Contains(out, "pass --password to gpuworks install") {
			t.Errorf("log does not explain how to configure the initial password:\n%s", out)
		}
	})
}

// The bootstrap notice is for a first run only; restarting against an existing
// database must not repeat it.
func TestAdminBootstrapLogSilentOnSecondStart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr:          "127.0.0.1:0",
		DataDir:       dir,
		DBPath:        filepath.Join(dir, "gpuworks.db"),
		AdminUser:     "admin",
		AdminPassword: "",
	}

	var first strings.Builder
	firstApp, err := New(cfg, slog.New(slog.NewTextHandler(&first, nil)))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	// Neither start below runs through Run, so each leaks its proxy's
	// route-table goroutine (ticking forever against this test's temp-dir
	// database) unless closed here - see the same note in newTestApp above.
	t.Cleanup(func() { firstApp.proxy.Close() })
	if !strings.Contains(first.String(), "created admin user") {
		t.Fatalf("first start did not report creating the admin user:\n%s", first.String())
	}
	credentials, err := os.ReadFile(installcred.CredentialsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.String(), "password=") {
		t.Fatal("first start exposed the generated password in the log")
	}

	var second strings.Builder
	secondApp, err := New(cfg, slog.New(slog.NewTextHandler(&second, nil)))
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	t.Cleanup(func() { secondApp.proxy.Close() })
	if strings.Contains(second.String(), "created admin user") {
		t.Errorf("second start reported creating the admin user again:\n%s", second.String())
	}
	if strings.Contains(second.String(), "password=") {
		t.Errorf("second start repeated the generated password:\n%s", second.String())
	}
	current, err := os.ReadFile(installcred.CredentialsPath(dir))
	if err != nil || string(current) != string(credentials) {
		t.Fatalf("second start changed initial credentials: err=%v", err)
	}
}
