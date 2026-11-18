//go:build linux

package pluginexec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDelegatedNeedsAWritableSubtreeAndTheServersOwnUnit(t *testing.T) {
	c := fakeCgroupfs(t, "acme") // parent system.slice/gpuworks.service
	dir := filepath.Join(c.Root, c.Parent)
	if !delegated(dir, 1000, "gpuworks.service") {
		t.Fatal("the service user's own writable unit cgroup is delegated to it")
	}
	// A writable cgroup of another unit or a session scope belongs to the
	// user without being delegated to the server.
	if delegated(dir, 1000, "other.service") || delegated(dir, 1000, "") {
		t.Fatal("a cgroup that is not the server's unit was treated as delegated")
	}
	// Root writes any cgroup; without systemd's delegate mark the cgroup
	// is systemd's, not the server's.
	if delegated(dir, 0, "gpuworks.service") {
		t.Fatal("root treated an unmarked cgroup as delegated")
	}
	if err := os.Remove(filepath.Join(dir, "cgroup.subtree_control")); err != nil {
		t.Fatal(err)
	}
	if delegated(dir, 1000, "gpuworks.service") {
		t.Fatal("a cgroup without a writable subtree_control is not delegated")
	}
}
