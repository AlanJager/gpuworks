//go:build linux

package pluginexec

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
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

func TestSystemdsDelegateMarkDelegatesAnyCgroup(t *testing.T) {
	c := fakeCgroupfs(t, "acme")
	dir := filepath.Join(c.Root, c.Parent)
	if delegated(dir, 0, "") {
		t.Fatal("an unmarked cgroup was treated as delegated")
	}
	if err := unix.Setxattr(dir, "user.delegate", []byte("1"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
			t.Skipf("the temp filesystem keeps no user xattrs: %v", err)
		}
		t.Fatal(err)
	}
	// systemd's mark is enough for root and for a cgroup that is not the
	// server's unit.
	if !delegated(dir, 0, "") {
		t.Fatal("a cgroup systemd marked delegated was not treated as delegated")
	}
}
