package selfinstall

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// A failed alias unlink must never replace a config that another writer
// recreated. Link publication is atomic and fails with EEXIST for any target,
// including a symlink, rather than following or overwriting it.
func (i *Installer) restoreSharedConfigAbsent(ctx context.Context, raw []byte, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := i.layout.sharedConfigFile()
	file, err := os.CreateTemp(filepath.Dir(path), ".server-config-recover-*")
	if err == nil {
		defer os.Remove(file.Name())
		if err := file.Chmod(mode); err != nil {
			file.Close()
			return err
		}
		if _, err := file.Write(raw); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Link(file.Name(), path)
	}
	if !i.canSudo() {
		return err
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("native private configuration recovery is not writable; run the whole uninstall with sudo: %w", err)
	}
	return i.withPrivilegedStage(ctx, path, func(stage, target, rename string) error {
		return i.deps.Run.Run(ctx, Command{Path: "sh", Args: []string{"-c", privilegedStageContentScript + `ln -T -- content "$2"
`, "gpuworks-config-restore", stage, target, fmt.Sprintf("%04o", mode.Perm()), strconv.Itoa(len(raw)), rename}, Sudo: true, Stdin: bytes.NewReader(raw)})
	})
}
