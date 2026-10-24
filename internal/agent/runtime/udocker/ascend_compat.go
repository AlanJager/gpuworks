package udocker

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/runveil"
	"gpuworks.ai/gpuworks/internal/assets"
)

// SHA256 of the qualified ARM64 CANN 8.5.1 TSD. Unknown/fixed libraries are
// deliberately not interposed; qualifications belong to library bytes, not tags.
const affectedAscendTSD = "4f6648daad462247e23782946b1a10b3d224d42d99aa2f468bd6b45c2a08676a"

const ascendCompatibilityTarget = "/opt/gpuworks/runveil/libascend-compat.so"

func (d *Driver) ascendRunveilCompatibility(spec runtime.WorkloadSpec, env map[string]string, binds []runveil.Bind) ([]runveil.Bind, error) {
	if !isAscendSpec(spec) {
		return binds, nil
	}
	data, err := assets.AscendRunveilCompatibility()
	if err != nil {
		return nil, fmt.Errorf("read Ascend runveil compatibility: %w", err)
	}
	delete(env, "GPUWORKS_ASCEND_TSD_FILE")
	if len(data) != 0 {
		identity, err := qualifiedTSDIdentity(filepath.Join(d.containerDir(spec.Name), "ROOT"), binds, affectedAscendTSD)
		if err != nil {
			return nil, err
		}
		if identity == "" {
			data = nil
		} else {
			env["GPUWORKS_ASCEND_TSD_FILE"] = identity
		}
	}
	return d.ascendCompatibilityBinds(spec.Name, env, binds, data)
}

func (d *Driver) ascendCompatibilityBinds(name string, env map[string]string, binds []runveil.Bind, library []byte) ([]runveil.Bind, error) {
	// DCMI otherwise forks systemd-detect-virt; when the image lacks it,
	// exit() in the failed child can deadlock on inherited CANN dump locks.
	marker := filepath.Join(d.containerDir(name), "gpuworks-ascend-container")
	if err := writeCompatibilityFile(marker, nil); err != nil {
		return nil, err
	}
	binds = append(binds, runveil.Bind{Host: marker, Guest: "/.dockerenv"})
	if len(library) == 0 {
		return binds, nil
	}
	file := filepath.Join(d.containerDir(name), "gpuworks-ascend-compat.so")
	if err := writeCompatibilityFile(file, library); err != nil {
		return nil, err
	}
	binds = append(binds, runveil.Bind{Host: file, Guest: ascendCompatibilityTarget})
	preload, explicit := env["LD_PRELOAD"]
	if !explicit {
		preload, _ = d.imageEnvValue(name, "LD_PRELOAD")
	}
	if strings.TrimSpace(preload) == "" {
		env["LD_PRELOAD"] = ascendCompatibilityTarget
	} else {
		env["LD_PRELOAD"] = ascendCompatibilityTarget + ":" + preload
	}
	return binds, nil
}

// Files belong to the disposable container directory, never the shared image
// or host. Unique temporary names also make concurrent exec preparation safe.
func writeCompatibilityFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o644 {
		if previous, err := os.ReadFile(path); err == nil && bytes.Equal(previous, data) {
			return nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ascend-compat-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func qualifiedTSDIdentity(root string, binds []runveil.Bind, digest string) (string, error) {
	for _, guest := range []string{
		"/usr/local/Ascend/ascend-toolkit/latest/lib64/libtsdclient.so",
		"/usr/local/Ascend/cann-8.5.1/lib64/libtsdclient.so",
		"/usr/local/Ascend/cann-8.5.1/aarch64-linux/lib64/libtsdclient.so",
	} {
		path := runveil.ResolveGuestPath(root, binds, guest)
		if path == "" {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("inspect Ascend TSD: %w", err)
		}
		before, err := file.Stat()
		if err != nil {
			file.Close()
			return "", err
		}
		if !before.Mode().IsRegular() || before.Size() > 64<<20 {
			file.Close()
			continue
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		after, statErr := file.Stat()
		file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if statErr != nil {
			return "", statErr
		}
		if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return "", fmt.Errorf("Ascend TSD changed while being inspected")
		}
		if fmt.Sprintf("%x", hash.Sum(nil)) != digest {
			continue
		}
		st, ok := after.Sys().(*syscall.Stat_t)
		if !ok {
			return "", fmt.Errorf("Ascend TSD file identity unavailable")
		}
		return fmt.Sprintf("%d:%d:%d:%d", uint64(st.Dev), st.Ino, after.Size(), after.ModTime().UnixNano()), nil
	}
	return "", nil
}
