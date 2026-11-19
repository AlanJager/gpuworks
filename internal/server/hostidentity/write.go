package hostidentity

import (
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"strconv"
)

// ErrNotRoot is Write run by an account other than root.
var ErrNotRoot = errors.New("host-identity --write must run as root")

// fileMode and dirMode are the modes Write gives the file and requires of
// its directory: the service group may read the file, and nobody but root
// may change, replace or remove it.
const (
	fileMode = 0o440
	dirMode  = 0o755
)

// checkTarget refuses a path Write must not write: one that is not
// absolute and clean, so a relative path cannot land wherever the caller
// happens to stand.
func checkTarget(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("host identity path %q must be absolute and clean", path)
	}
	if filepath.Dir(path) == path {
		return fmt.Errorf("host identity path %q names no file", path)
	}
	return nil
}

// groupID resolves the group Write gives the file; empty is root's.
func groupID(group string) (int, error) {
	if group == "" {
		return 0, nil
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, fmt.Errorf("host identity group: %w", err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("host identity group %s: gid %q", group, g.Gid)
	}
	return gid, nil
}
