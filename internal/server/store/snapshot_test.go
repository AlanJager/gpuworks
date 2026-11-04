package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestSnapshotToCopiesTheLiveDatabase(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "live.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(SettingOrphanCleanup, "true"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "snapshot.db")
	if err := st.SnapshotTo(dest); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot mode before opening: %v/%v", info, err)
	}
	copyStore, err := Open(dest, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer copyStore.Close()
	if got := copyStore.GetSetting(SettingOrphanCleanup); got != "true" {
		t.Fatalf("snapshot setting = %q", got)
	}
	// The live database keeps working and later writes do not reach the copy.
	if err := st.SetSetting(SettingOrphanCleanup, "false"); err != nil {
		t.Fatal(err)
	}
	if got := copyStore.GetSetting(SettingOrphanCleanup); got != "true" {
		t.Fatalf("snapshot changed after the fact: %q", got)
	}
	if err := st.SnapshotTo(dest); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwriting a snapshot = %v", err)
	}
	if err := st.SnapshotTo(" "); err == nil {
		t.Fatal("an empty destination must be refused")
	}
}
