package app

import (
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

func TestDiagnosticsReportsTheServersDirectoriesOnce(t *testing.T) {
	install := t.TempDir()
	data := filepath.Join(install, "data")
	cfg := Config{DataDir: data, ConfigPath: filepath.Join(install, "server.yaml")}
	if got := installBackupsDir(cfg); got != "" {
		t.Fatalf("backups before any upgrade = %q, want none", got)
	}
	if err := os.MkdirAll(filepath.Join(install, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	backups := installBackupsDir(cfg)
	if backups != filepath.Join(install, "backups") {
		t.Fatalf("backups = %q", backups)
	}
	if got := installBackupsDir(Config{DataDir: data}); got != "" {
		t.Fatalf("backups without server.yaml = %q", got)
	}

	tmp := t.TempDir()
	t.Setenv("SQLITE_TMPDIR", tmp)
	// The log directory is the data directory here: it is reported once.
	src := diagnosticsSources{dataDir: data, logFile: filepath.Join(data, "gpuworks-server.log"), backupsDir: backups}
	got := src.ServerDirs()
	want := []diagnostics.ServerDir{{Kind: "data", Path: data}, {Kind: "backups", Path: backups}, {Kind: "sqlite_tmp", Path: tmp}}
	if len(got) != len(want) {
		t.Fatalf("dirs = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dirs = %+v, want %+v", got, want)
		}
	}
}
