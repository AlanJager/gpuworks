package engine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
)

// hygonUdockerMounts supplies one complete configuration-directory view to
// either PRoot launcher. Binding only individual files could leave other native
// IDs visible in the guest. Keep the ownership ledger outside this view; PRoot
// bindings do not provide Docker's read-only mount or device-cgroup isolation.
func hygonUdockerMounts(instanceDir, snapshot string, config []byte) ([]agentruntime.Mount, error) {
	name := filepath.Base(snapshot)
	view := filepath.Join(instanceDir, "vdev-view")
	docker := filepath.Join(view, "docker")
	logs := filepath.Join(instanceDir, "hylog")
	if err := hygonViewDirectory(view, 0o755, name, "docker"); err != nil {
		return nil, err
	}
	if err := hygonViewDirectory(docker, 0o755, name); err != nil {
		return nil, err
	}
	for _, path := range []string{filepath.Join(view, name), filepath.Join(docker, name)} {
		if info, err := os.Lstat(path); os.IsNotExist(err) {
			if err := writePrivateFile(path, config, 0o644); err != nil {
				return nil, fmt.Errorf("write config %s: %w", path, err)
			}
		} else {
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("config %s is not a regular file", path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read config %s: %w", path, err)
			}
			if !bytes.Equal(data, config) {
				return nil, fmt.Errorf("config %s differs from the verified snapshot", path)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				return nil, err
			}
		}
	}
	// Native logging needs an existing writable directory; udocker does not
	// implement Docker --tmpfs. Each instance gets its own sticky directory.
	if err := hygonViewDirectory(logs, 0o777|os.ModeSticky); err != nil {
		return nil, err
	}
	return []agentruntime.Mount{
		{Source: view, Target: "/etc/vdev", ReadOnly: true},
		{Source: logs, Target: "/var/log/hylog"},
	}, nil
}

func hygonViewDirectory(path string, mode os.FileMode, allowed ...string) error {
	if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", path)
	}
	if len(allowed) != 0 {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			found := false
			for _, name := range allowed {
				found = found || entry.Name() == name
			}
			if !found {
				return fmt.Errorf("unexpected entry %s in config view %s", entry.Name(), path)
			}
		}
	}
	// Mkdir applies the agent's umask. Explicitly preserve traversal/read
	// permissions and the log directory's sticky world-write permission.
	return os.Chmod(path, mode)
}
