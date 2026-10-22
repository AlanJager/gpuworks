package udocker

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// ensureStateTraversal lets the dedicated runtime user reach the patched PRoot
// and the managed directories below the agent state directory. It runs on
// every provisioning check, not only the first, so a state directory reset
// after provisioning is repaired before the next launch.
func (d *Driver) ensureStateTraversal() error {
	if d.runUser == "" || d.runUID == os.Geteuid() {
		return nil
	}
	owner := runtime.MountOwner{UID: d.runUID, GID: d.runGID, Username: d.runUser}
	if err := runtime.PrepareTraversal(filepath.Dir(d.udockerDir), owner); err != nil {
		return fmt.Errorf("prepare runtime state access: %w", err)
	}
	return nil
}

// warnUntraversableAncestors explains, once per provisioning, why launches
// will fail when an operator directory above the state directory is closed to
// the runtime user. It is not fatal: image management and the runveil
// troubleshooting commands run as the agent and keep working.
func (d *Driver) warnUntraversableAncestors() {
	if d.runUser == "" || d.runUID == os.Geteuid() {
		return
	}
	owner := runtime.MountOwner{UID: d.runUID, GID: d.runGID, Username: d.runUser}
	if err := runtime.CheckAncestorTraversal(filepath.Dir(filepath.Dir(d.udockerDir)), owner); err != nil {
		slog.Warn("runtime user cannot reach the agent state directory; workloads will fail to launch until the operator opens it", "user", d.runUser, "err", err)
	}
}
