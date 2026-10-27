package udocker

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

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
// troubleshooting commands run as the agent and keep working. The doctor
// reports the same condition through StateAccessProblem.
func (d *Driver) warnUntraversableAncestors() {
	if d.runUser == "" || d.runUID == os.Geteuid() {
		return
	}
	owner := runtime.MountOwner{UID: d.runUID, GID: d.runGID, Username: d.runUser}
	if _, blocked := d.untraversableAncestor(owner); blocked != nil {
		slog.Warn("runtime user cannot reach the agent state directory; workloads will fail to launch until the operator opens it", "user", d.runUser, "err", blocked)
	}
}

// untraversableAncestor is the one verdict on whether owner can reach the
// state directory: the first directory above it that owner cannot search.
func (d *Driver) untraversableAncestor(owner runtime.MountOwner) (stateDir string, blocked *runtime.TraversalError) {
	stateDir = filepath.Dir(d.udockerDir)
	err := runtime.CheckAncestorTraversal(filepath.Dir(stateDir), owner)
	if errors.As(err, &blocked) {
		return stateDir, blocked
	}
	return stateDir, nil
}

// lookupExistingUser finds the dedicated account without creating it; a test
// replaces it.
var lookupExistingUser = user.Lookup

// StateAccessProblem implements runtime.StateAccessReporter. Once the runtime
// is set up it judges the identity workloads actually run as; before that, or
// after a failed attempt, a root agent is expected to use the dedicated
// account, so it judges that one (an account not created yet counts as
// "other": no ownership, no group). A non-root agent runs workloads as itself,
// which needs no grant.
func (d *Driver) StateAccessProblem() (string, *runtime.TraversalError) {
	owner, ok := d.workloadOwner()
	if !ok {
		return "", nil
	}
	return d.untraversableAncestor(owner)
}

func (d *Driver) workloadOwner() (runtime.MountOwner, bool) {
	t := &d.track
	t.mu.Lock()
	done, name, uid, gid := t.done, t.user, t.uid, t.gid
	t.mu.Unlock()
	if done {
		return runtime.MountOwner{UID: uid, GID: gid, Username: name}, name != "" && uid != os.Geteuid()
	}
	if geteuid() != 0 {
		return runtime.MountOwner{}, false
	}
	owner := runtime.MountOwner{UID: -1, GID: -1, Username: dedicatedRunUser}
	if account, err := lookupExistingUser(dedicatedRunUser); err == nil {
		if uid, err := strconv.Atoi(account.Uid); err == nil {
			owner.UID = uid
		}
		if gid, err := strconv.Atoi(account.Gid); err == nil {
			owner.GID = gid
		}
	}
	return owner, true
}
