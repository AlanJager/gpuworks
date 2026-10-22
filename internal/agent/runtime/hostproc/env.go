package hostproc

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	oci "gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// readyMarker sits next to an environment's ROOT once it has been unpacked and
// re-rooted; an environment directory without it is a create that did not
// finish and is rebuilt.
const readyMarker = ".gpuworks-env-ready"

// envName is the directory an image's environment package is unpacked into.
// It is keyed by the manifest, not the tag, so two tags of one package share
// one copy and a re-pushed tag gets a fresh one.
func envName(manifestID string) string {
	id := strings.TrimPrefix(manifestID, "sha256:")
	if len(id) > 16 {
		id = id[:16]
	}
	return "env-" + id
}

// ensureEnv returns the unpacked root of image's environment package,
// unpacking and re-rooting it on first use. Environments are read-only to the
// workloads that share them, so every instance of one package uses one copy.
func (d *Driver) ensureEnv(ctx context.Context, image string, onLine func(string)) (string, error) {
	d.envMu.Lock()
	defer d.envMu.Unlock()
	return d.ensureEnvLocked(ctx, image, onLine)
}

// ensureEnvLocked is ensureEnv for a caller that holds envMu.
func (d *Driver) ensureEnvLocked(ctx context.Context, image string, onLine func(string)) (string, error) {
	ref := oci.ParseRef(image)
	manifestID, err := d.oci.Layout.ImageID(ref)
	if err != nil || manifestID == "" {
		return "", fmt.Errorf("environment package %s is not pulled on this node", image)
	}
	name := envName(manifestID)
	dir := d.oci.Layout.ContainerDir(name)
	root := filepath.Join(dir, "ROOT")

	if _, err := os.Stat(filepath.Join(dir, readyMarker)); err == nil {
		return root, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("remove incomplete environment %s: %w", name, err)
	}
	if err := d.oci.Create(ctx, name, image, onLine); err != nil {
		return "", fmt.Errorf("unpack environment package %s: %w", image, err)
	}
	if err := reroot(root); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("re-root environment package %s: %w", image, err)
	}
	if err := os.WriteFile(filepath.Join(dir, readyMarker), []byte(image+"\n"), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return root, nil
}

// reroot makes an unpacked package work where it was unpacked. A package is
// built as a root filesystem, so its absolute paths mean paths inside it, the
// way they would in a container. Two kinds of file record such paths and are
// followed by the OS rather than by the launch: absolute symlinks (a venv's
// bin/python -> /python/bin/python3.12) and a Python venv's pyvenv.cfg, whose
// "home" names the base interpreter's directory. Both are prefixed with root.
func reroot(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) || strings.HasPrefix(target, root+string(filepath.Separator)) {
				return nil
			}
			tmp := path + ".gpuworks-reroot"
			_ = os.Remove(tmp)
			if err := os.Symlink(filepath.Join(root, target), tmp); err != nil {
				return err
			}
			return os.Rename(tmp, path)
		case entry.Type().IsRegular() && entry.Name() == "pyvenv.cfg":
			return rerootVenvConfig(root, path)
		}
		return nil
	})
}

func rerootVenvConfig(root, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out strings.Builder
	changed := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "home" {
			home := strings.TrimSpace(value)
			if filepath.IsAbs(home) && !strings.HasPrefix(home, root+string(filepath.Separator)) {
				line = "home = " + filepath.Join(root, home)
				changed = true
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}
