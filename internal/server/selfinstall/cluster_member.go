package selfinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrClusterMember reports a command that acts on one server being run on an
// installation that is a member of a control-plane cluster.
var ErrClusterMember = errors.New("this installation is a control-plane cluster member")

// ClusterMember reports whether dataDir belongs to a control-plane cluster
// member: it holds the cluster's enrollment configuration, an unfinished
// join, or a database with a second member. It reads marker files only, so a
// build without the cluster runtime answers the same for such a directory.
func ClusterMember(dataDir string) (bool, error) {
	if dataDir == "" {
		return false, nil
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "cluster-enrollment", "config.json")); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	single, err := ordinarySingleMemberDatabase(filepath.Join(dataDir, "gpuworks.db"))
	if err != nil {
		return false, err
	}
	return !single, nil
}

// refuseClusterMember stops command before it changes anything when the
// installation is a cluster member. A member is run by the cluster's own
// service unit together with its database process, and the cluster's members
// have to stay on one release; these commands know neither.
func (i *Installer) refuseClusterMember(command string) error {
	member, err := ClusterMember(i.layout.DataDir)
	if err != nil {
		return fmt.Errorf("cannot tell whether %s is a control-plane cluster member: %w", i.layout.Dir, err)
	}
	if !member {
		return nil
	}
	return fmt.Errorf("%w (%s): `gpuworks %s` acts on one server only, and does not know the member's own service unit, its database process or the other members; nothing was changed. Follow the cluster procedure in the control-plane cluster documentation (explanation/control-plane-cluster)",
		ErrClusterMember, i.layout.DataDir, command)
}
