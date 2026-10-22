package expose

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreConfigRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())
	cfg := &Config{Revision: "rev-1", Provider: ProviderNamed, Desired: DesiredEnabled,
		TokenFile: "/etc/gpuworks/token", Hostname: "gpu.example.com"}
	if err := store.WriteConfig(cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	got, err := store.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if got.Revision != cfg.Revision || got.Provider != cfg.Provider || got.Hostname != cfg.Hostname {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.UpdatedAt.IsZero() || got.UpdatedAt.Location() != time.UTC {
		t.Errorf("UpdatedAt = %v, want a UTC timestamp", got.UpdatedAt)
	}
	// The wire shape is the contract another gpuworks process and a human read.
	raw, err := os.ReadFile(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"revision": "rev-1"`, `"provider": "cloudflare-named"`, `"token_file"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config JSON is missing %s:\n%s", want, raw)
		}
	}
	// An atomic write leaves no temporary neighbour behind.
	entries, err := os.ReadDir(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestStoreRejectsInvalidConfig(t *testing.T) {
	store := NewStore(t.TempDir())
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"no revision", &Config{Provider: ProviderQuick, Desired: DesiredEnabled}, "revision"},
		{"unknown provider", &Config{Revision: "r", Provider: "ngrok", Desired: DesiredEnabled}, "unknown provider"},
		{"unknown desired", &Config{Revision: "r", Provider: ProviderQuick, Desired: "maybe"}, "invalid desired"},
		{"named without a token", &Config{Revision: "r", Provider: ProviderNamed, Desired: DesiredEnabled}, "token file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.WriteConfig(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("WriteConfig = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestStoreRemoveLocalKeepsWhatIsReusable covers what `rm` owns: the local
// configuration, the observation, the isolation directory and the credential
// this tool stored — while the binary cache, the log, and a token file the
// operator named are left alone.
func TestStoreRemoveLocalKeepsWhatIsReusable(t *testing.T) {
	store := NewStore(t.TempDir())
	managedToken, err := store.WriteToken("managed-secret")
	if err != nil {
		t.Fatal(err)
	}
	foreignToken := filepath.Join(t.TempDir(), "my-token")
	if err := os.WriteFile(foreignToken, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteConfig(&Config{Revision: "r", Provider: ProviderNamed, Desired: DesiredEnabled, TokenFile: managedToken}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteStatus(&Status{Revision: "r", State: StateReady, URL: "https://x.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store.Root(), "run", "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(store.BinDir(), "cloudflared-2026.9.1-amd64")
	if err := os.MkdirAll(store.BinDir(), 0o2775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(store.LogPath()), 0o2775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.LogPath(), []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.RemoveLocal(managedToken); err != nil {
		t.Fatalf("RemoveLocal: %v", err)
	}
	for _, gone := range []string{store.ConfigPath(), store.StatusPath(), managedToken} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should be gone", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "run")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the per-run isolation directory should be cleared")
	}
	for _, kept := range []string{cached, store.LogPath(), foreignToken} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should have been kept: %v", kept, err)
		}
	}
}

// TestStoreRemoveLocalDoesNotDeleteTheOperatorsTokenFile: a token the operator
// named with --token-file is theirs, and removing our configuration is not a
// reason to delete their credential.
func TestStoreRemoveLocalDoesNotDeleteTheOperatorsTokenFile(t *testing.T) {
	store := NewStore(t.TempDir())
	foreign := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(foreign, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveLocal(foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("the operator's token file must survive: %v", err)
	}
}

func TestStoreReadStatusToleratesGarbage(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.StatusPath(), []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.ReadStatus()
	if err != nil {
		t.Fatalf("a truncated observation must not be an error: %v", err)
	}
	if st != nil {
		t.Fatalf("status = %+v, want nothing rather than a half-read snapshot", st)
	}
}

func TestStoreLocks(t *testing.T) {
	store := NewStore(t.TempDir())
	release, err := store.Lock(time.Second)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, err := store.Lock(50 * time.Millisecond); err == nil {
		t.Fatal("the transaction lock must exclude a second holder")
	}
	release()
	release() // releasing twice is harmless

	// The hosting lock is held for a server's whole life, so a second server
	// must be told rather than made to wait.
	hosting, err := store.SupervisorLock()
	if err != nil {
		t.Fatalf("SupervisorLock: %v", err)
	}
	if !store.SupervisorHeld() {
		t.Error("SupervisorHeld must report a held lock")
	}
	if _, err := store.SupervisorLock(); err == nil {
		t.Fatal("a second host must not be granted")
	}
	hosting()
	if store.SupervisorHeld() {
		t.Error("SupervisorHeld must report a released lock")
	}
}

func TestResolveDataDirPrecedence(t *testing.T) {
	if dir, how := ResolveDataDir("/tmp/explicit"); dir != "/tmp/explicit" || how != "--data" {
		t.Errorf("explicit = %q (%s), want --data to win", dir, how)
	}
	// The environment is not a source.
	t.Setenv("GPUWORKS_DATA_DIR", "/tmp/from-env")
	// Without --data the answer comes from an installation's server.yaml, and
	// only a machine that has never installed gpuworks falls back to the
	// development default. Both are correct; which one applies depends on the
	// host, so the assertion is that the discovery names its rule and finds
	// something rather than failing.
	dir, how := ResolveDataDir("")
	if dir == "/tmp/from-env" {
		t.Fatal("the environment chose the data dir")
	}
	if dir == "" {
		t.Fatal("discovery returned nothing")
	}
	switch how {
	case "the development default", "the default installation's server.yaml", "server.yaml next to the binary":
	default:
		t.Errorf("unexpected discovery rule %q", how)
	}
}

func TestNewRevisionIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		rev := NewRevision()
		if rev == "" || seen[rev] {
			t.Fatalf("revision %q is empty or repeated", rev)
		}
		seen[rev] = true
	}
}
