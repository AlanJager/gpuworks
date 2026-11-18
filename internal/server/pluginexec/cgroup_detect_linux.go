//go:build linux

package pluginexec

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DetectCgroup finds the server's cgroup v2 placement from
// /proc/self/mountinfo and /proc/self/cgroup and reports ok only when that
// cgroup is delegated to the server, so the server may create the plugins'
// cgroups under it. root is the cgroup2 mount, parent the server's service
// cgroup relative to it. unit is the server's systemd unit
// ("gpuworks.service").
func DetectCgroup(unit string) (root, parent string, ok bool) {
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", "", false
	}
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", "", false
	}
	root, parent, ok = parseCgroupPlace(mountinfo, self)
	if !ok || !delegated(filepath.Join(root, parent), os.Geteuid(), unit) {
		return "", "", false
	}
	return root, parent, true
}

// delegated reports whether the cgroup at dir is the server's to
// reorganise. Its subtree_control must be writable, and either systemd
// marked the cgroup delegated (the user.delegate or trusted.delegate
// attribute, systemd 251 and later), or the server runs as a non-root
// user in its own unit's cgroup: systemd hands a unit's cgroup to its
// User= only for Delegate=yes. A writable cgroup is not enough on its own:
// root can write any cgroup, and a desktop session's scopes belong to
// their user without being delegated to the processes in them.
func delegated(dir string, euid int, unit string) bool {
	if !writable(filepath.Join(dir, "cgroup.subtree_control")) {
		return false
	}
	for _, name := range []string{"user.delegate", "trusted.delegate"} {
		if _, err := unix.Getxattr(dir, name, nil); err == nil || errors.Is(err, unix.ERANGE) {
			return true
		}
	}
	return euid != 0 && unit != "" && filepath.Base(dir) == unit
}
