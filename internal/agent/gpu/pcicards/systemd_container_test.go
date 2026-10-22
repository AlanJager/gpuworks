package pcicards

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSystemdContainerDoesNotDescribeTheHostsBus(t *testing.T) {
	restoreMarkers, restoreCgroup := containerMarkers, procOneCgroup
	t.Cleanup(func() { containerMarkers, procOneCgroup = restoreMarkers, restoreCgroup })
	for _, name := range containerEnv {
		t.Setenv(name, "")
	}
	root := t.TempDir()
	// Redirect the production list without adding the marker under test.
	containerMarkers = make([]string, len(restoreMarkers))
	for i, path := range restoreMarkers {
		containerMarkers[i] = filepath.Join(root, path)
	}
	procOneCgroup = filepath.Join(root, "cgroup")
	if err := os.WriteFile(procOneCgroup, []byte("0::/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !(Scanner{}).hostOwnsThisBus() {
		t.Fatal("without container evidence, the host must retain its PCI inventory")
	}
	marker := filepath.Join(root, "run/systemd/container")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if (Scanner{}).hostOwnsThisBus() {
		t.Fatal("a systemd-marked container must not claim the host's PCI inventory")
	}
	if cards, unknown := (Scanner{}).Scan(); cards != nil || unknown != nil {
		t.Fatalf("inside a container: cards=%+v unknown=%+v, want nothing claimed", cards, unknown)
	}
}
