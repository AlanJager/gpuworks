package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAutoDLTraversalGuards(t *testing.T) {
	root := stateDir(t, 0o710)
	owner := MountOwner{UID: 1000, GID: 1000}
	for _, tc := range []struct {
		name, host, path string
		uid              int
		owner            MountOwner
	}{
		{"other provider", "gpu-node", root, 0, owner},
		{"non-root agent", "autodl-container-abc", root, 1000, owner},
		{"root runtime", "autodl-container-abc", root, 0, MountOwner{UID: 0}},
		{"outside root", "autodl-container-abc", filepath.Dir(root), 0, owner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := prepareAutoDLRootTraversal(tc.path, tc.owner, tc.host, root, tc.uid); err != nil {
				t.Fatal(err)
			}
			assertDirMode(t, root, 0o710)
		})
	}
}

// Run as root as well as the normal unit suite to exercise actual permission
// changes without touching the host's /root: sudo go test ./internal/agent/runtime.
func TestAutoDLTraversalRepairsRootOnly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to verify root-owned directory repair")
	}
	for _, mode := range []os.FileMode{0o700, 0o710, 0o750, 0o711} {
		root := stateDir(t, mode)
		child := filepath.Join(root, "state")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatal(err)
		}
		owner := MountOwner{UID: 1000, GID: 1000}
		for i := 0; i < 2; i++ {
			if err := prepareAutoDLRootTraversal(child, owner, "autodl-container-abc", root, 0); err != nil {
				t.Fatal(err)
			}
			assertDirMode(t, root, mode|0o001)
			assertDirMode(t, child, 0o700)
			assertGroup(t, root, os.Getegid())
		}
	}
	root := stateDir(t, 0o700)
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := prepareAutoDLRootTraversal(link, MountOwner{UID: 1000, GID: 1000}, "autodl-container-abc", link, 0); err != nil {
		t.Fatal(err)
	}
	assertDirMode(t, root, 0o700)
}
