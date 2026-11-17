package pluginexec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// controllers are the cgroup v2 controllers a plugin's limits use.
var controllers = []string{"memory", "cpu", "pids"}

// MoveToCgroup creates <root>/<parent>/plugins/<id>, writes the limits it
// has controllers for and moves pid into it. It does nothing and reports
// moved=false when the parent cgroup is not delegated to this user (its
// subtree_control is not writable). Limits whose files are missing (the
// controller is not enabled) are skipped and named in the returned
// warnings; failing to move pid is an error. Everything is best effort for
// the caller: a plugin still starts outside a cgroup.
func MoveToCgroup(c *Cgroup, id string, pid int) (moved bool, warnings []string, err error) {
	parent := filepath.Join(c.Root, c.Parent)
	if !writable(filepath.Join(parent, "cgroup.subtree_control")) {
		return false, nil, nil
	}
	plugins := filepath.Join(parent, "plugins")
	if err := os.Mkdir(plugins, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return false, nil, fmt.Errorf("cgroup: %w", err)
	}
	// Children get the controllers only if both levels enable them. The
	// server's own cgroup refuses while it holds processes (cgroup v2 has
	// no internal processes), so a failure here leaves the limits off.
	for _, dir := range []string{parent, plugins} {
		for _, ctl := range controllers {
			if err := writeExisting(filepath.Join(dir, "cgroup.subtree_control"), "+"+ctl); err != nil {
				warnings = append(warnings, fmt.Sprintf("enable %s in %s: %v", ctl, dir, err))
			}
		}
	}
	dir := c.Dir(id)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return false, warnings, fmt.Errorf("cgroup: %w", err)
	}
	for _, l := range []struct {
		file  string
		value int64
	}{
		{"memory.max", c.MemoryMax}, {"memory.high", c.MemoryHigh},
		{"cpu.weight", int64(c.CPUWeight)}, {"pids.max", int64(c.PidsMax)},
	} {
		if l.value <= 0 {
			continue
		}
		if err := writeExisting(filepath.Join(dir, l.file), strconv.FormatInt(l.value, 10)); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", l.file, err))
		}
	}
	if err := writeExisting(filepath.Join(dir, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		return false, warnings, fmt.Errorf("cgroup: moving the plugin: %w", err)
	}
	return true, warnings, nil
}

// RemoveCgroup removes a plugin's cgroup directory after its process
// exited; cgroupfs removes an empty cgroup with rmdir.
func RemoveCgroup(c *Cgroup, id string) error {
	err := os.Remove(c.Dir(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// writable reports whether path can be opened for writing.
func writable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// writeExisting writes value to a cgroupfs file, which already exists for
// every enabled controller and cannot be created.
func writeExisting(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(value)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
