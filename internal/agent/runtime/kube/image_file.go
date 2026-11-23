package kube

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// CheckImageFile follows the same host paths and symlinks as nerdctl load.
func (d *Driver) CheckImageFile(ctx context.Context, path string) (int64, error) {
	out, err := HostExec(ctx, "stat", "-L", "-c", "%f %s", "--", path)
	if err != nil {
		return 0, fmt.Errorf("stat host image archive: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, fmt.Errorf("unexpected host image archive stat output: %q", out)
	}
	mode, modeErr := strconv.ParseUint(fields[0], 16, 32)
	size, sizeErr := strconv.ParseInt(fields[1], 10, 64)
	if modeErr != nil || sizeErr != nil || size < 0 {
		return 0, fmt.Errorf("invalid host image archive stat output: %q", out)
	}
	if mode&syscall.S_IFMT != syscall.S_IFREG {
		return 0, runtime.ErrImageFileNotRegular
	}
	return size, nil
}

var _ runtime.ImageFileChecker = (*Driver)(nil)

// Resolve absolute symlinks in the worker's mount namespace just as nerdctl
// does, then open beneath the worker root, including if a link changes.
func (d *Driver) OpenImageFile(ctx context.Context, path string) (*os.File, error) {
	resolved, err := HostExec(ctx, "readlink", "-m", "--", path)
	if err != nil {
		return nil, fmt.Errorf("resolve host archive: %w", err)
	}
	name := strings.TrimSuffix(string(resolved), "\n")
	if !filepath.IsAbs(name) {
		return nil, fmt.Errorf("host archive path is not absolute")
	}
	root, err := os.OpenRoot(firstNonEmpty(d.o.HostRoot, HostRoot))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return runtime.OpenRegularImageFile(root, strings.TrimPrefix(name, "/"))
}
