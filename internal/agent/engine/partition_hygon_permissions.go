package engine

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The native helper normalizes the instance leaf and snapshot, but PRoot's
// runtime UID also needs search permission on these two agent-owned parents.
// Leave agent state, its other ancestors, and private files untouched. Unlike
// PrepareTraversal, this must reject managed symlinks and never repair /root.
func prepareHygonUdockerParents(stateDir string) error {
	parent, err := os.Open(stateDir)
	if err != nil {
		return err
	}
	defer parent.Close()
	for _, name := range []string{partitionStateRoot, "hygon-instances"} {
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("create %s: %w", name, err)
		}
		// Anchor each component to the previously opened directory. No path
		// replacement or symlink can redirect the permission change elsewhere.
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open managed directory %s: %w", name, err)
		}
		directory := os.NewFile(uintptr(fd), name)
		defer directory.Close()
		info, err := directory.Stat()
		if err != nil {
			return err
		}
		uid, _, ok := agentruntime.FileOwner(info)
		if !ok || uid != os.Geteuid() || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("managed directory %s must belong to the agent and not be writable by group or others", name)
		}
		// Search alone exposes neither listing nor writes (0700 -> 0711).
		if err := directory.Chmod(info.Mode() | 0o111); err != nil {
			return fmt.Errorf("grant search on %s: %w", name, err)
		}
		parent = directory
	}
	return nil
}
