package runveil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strings"
)

// The file lists of udocker's `setup --nvidia` (config.py nvi_*_list).
var (
	nvidiaLibPrefixes = []string{
		"libOpenCL.", "libcuda.", "libnvcuvid.", "libnvidia-cfg.", "libnvidia-compiler.",
		"libnvidia-encode.", "libnvidia-fatbinaryloader.", "libnvidia-fbc.", "libnvidia-ifr.",
		"libnvidia-ml.", "libnvidia-opencl.", "libnvidia-ptxjitcompiler.", "libnvidia-tls.",
		"tls/libnvidia-tls.",
	}
	nvidiaEtcFiles = []string{"vulkan/icd.d/nvidia_icd.json", "OpenCL/vendors/nvidia.icd"}
	nvidiaBinFiles = []string{
		"nvidia-bug-report.sh", "nvidia-cuda-mps-control", "nvidia-cuda-mps-server",
		"nvidia-debugdump", "nvidia-installer", "nvidia-persistenced", "nvidia-settings",
		"nvidia-smi", "nvidia-uninstall", "nvidia-xconfig",
	}
	// nvidiaMainLibs identify a directory as holding the host driver.
	nvidiaMainLibs = []string{"libnvidia-cfg.so", "libcuda.so"}
	// hostEtcDir and hostBinDir are the sources of the ICD files and
	// utilities; tests point them at fixtures.
	hostEtcDir = "/etc"
	hostBinDir = "/usr/bin"
)

// nvidiaMarker is the file udocker's NvidiaMode leaves in the container
// directory once the driver files are in place.
const nvidiaMarker = "nvidia"

// multiarchDir is the Debian multiarch library directory for this build.
func multiarchDir() string {
	if goruntime.GOARCH == "arm64" {
		return "/usr/lib/aarch64-linux-gnu"
	}
	return "/usr/lib/x86_64-linux-gnu"
}

// HostNVIDIALibDirs finds the host directories holding the NVIDIA user-space
// driver, as NvidiaMode._find_host_dir does: the standard library directories,
// `ldconfig -p` output, then LD_LIBRARY_PATH. Directories are canonical.
func HostNVIDIALibDirs(ldconfigOutput, ldLibraryPath string) []string {
	found := map[string]bool{}
	fromPath := func(list string) {
		for _, dir := range strings.Split(list, ":") {
			if dir == "" {
				continue
			}
			for _, lib := range nvidiaMainLibs {
				if matches, _ := filepath.Glob(filepath.Join(dir, lib+"*")); len(matches) > 0 {
					if real, err := filepath.EvalSymlinks(dir); err == nil {
						found[real] = true
					}
				}
			}
		}
	}
	fromPath(multiarchDir() + ":/usr/lib64")
	for _, lib := range nvidiaMainLibs {
		re := regexp.MustCompile(`[ \t]` + regexp.QuoteMeta(lib) + `[^ ]* .* => (/.*)`)
		for _, line := range strings.Split(ldconfigOutput, "\n") {
			if m := re.FindStringSubmatch(line); m != nil {
				if real, err := filepath.EvalSymlinks(filepath.Dir(strings.TrimSpace(m[1]))); err == nil {
					found[real] = true
				}
			}
		}
	}
	fromPath(ldLibraryPath)
	dirs := make([]string, 0, len(found))
	for dir := range found {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// SetupNVIDIA makes the host NVIDIA user-space driver visible inside the
// container the way `udocker setup --nvidia` does: libraries are copied into
// the guest's library directory (symlinks recreated as symlinks), plus the
// ICD files and utilities, and a marker records completion.
//
// Like udocker it never fails a launch: a host without the driver, a guest
// without a library directory, or a guest that already ships any of these
// files is reported in the returned note and skipped. Unlike udocker, a
// container that already has the marker is a no-op instead of printing an
// "installation already exists" error on every start.
func SetupNVIDIA(containerDir string, hostLibDirs []string) (string, error) {
	marker := filepath.Join(containerDir, nvidiaMarker)
	if _, err := os.Stat(marker); err == nil {
		return "nvidia driver files already installed", nil
	}
	root := filepath.Join(containerDir, "ROOT")
	if len(hostLibDirs) == 0 {
		return "host nvidia libraries not found; skipped", nil
	}
	guestLibDir := ""
	for _, candidate := range []string{multiarchDir(), "/usr/lib64"} {
		if isDir(filepath.Join(root, candidate)) {
			guestLibDir = candidate
			break
		}
	}
	if guestLibDir == "" {
		return "no guest directory for nvidia libraries; skipped", nil
	}

	type copyItem struct{ src, dst string }
	var items []copyItem
	for _, dir := range hostLibDirs {
		for _, lib := range nvidiaLibPrefixes {
			matches, _ := filepath.Glob(filepath.Join(dir, lib+"*"))
			for _, match := range matches {
				rel := strings.TrimPrefix(match, dir)
				items = append(items, copyItem{match, filepath.Join(root, guestLibDir, rel)})
			}
		}
	}
	for _, name := range nvidiaEtcFiles {
		items = append(items, copyItem{filepath.Join(hostEtcDir, name), filepath.Join(root, "etc", name)})
	}
	for _, name := range nvidiaBinFiles {
		items = append(items, copyItem{filepath.Join(hostBinDir, name), filepath.Join(root, "usr", "bin", name)})
	}
	// udocker refuses the whole setup when any destination already exists.
	for _, item := range items {
		if _, err := os.Lstat(item.dst); err == nil {
			return fmt.Sprintf("guest already provides %s; nvidia setup skipped", strings.TrimPrefix(item.dst, root)), nil
		}
	}
	copied := 0
	for _, item := range items {
		ok, err := copyDriverFile(item.src, item.dst)
		if err != nil {
			return "", fmt.Errorf("install %s: %w", strings.TrimPrefix(item.dst, root), err)
		}
		if ok {
			copied++
		}
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("installed %d nvidia driver files into %s", copied, guestLibDir), nil
}

// copyDriverFile copies one host file (or recreates a symlink verbatim) and
// reports whether the source existed.
func copyDriverFile(src, dst string) (bool, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return false, err
		}
		return true, os.Symlink(target, dst)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return false, err
	}
	// udocker keeps the source mode and makes the copy owner-readable/writable.
	if err := tmp.Chmod(info.Mode().Perm() | 0o600); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), dst)
}
