package selfinstall

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestRequireDrainedMaintenancePhaseAcceptsOrdinarySQLite(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "gpuworks.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE settings (k TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := requireDrainedMaintenancePhase(dir); err != nil {
		t.Fatal(err)
	}
}

func TestRequireDrainedMaintenancePhaseAcceptsNonDatabase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gpuworks.db"), []byte("not-sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireDrainedMaintenancePhase(dir); err != nil {
		t.Fatal(err)
	}
}
