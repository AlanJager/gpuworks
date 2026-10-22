package selfinstall

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// writeFileAtomic replaces path in one step: readers either see the old file or
// the new one, never a half-written one. The temporary file is created next to
// the target so the rename stays within one filesystem.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// The tree is handed to the service group at the end of an install, so the
// first install into a fresh /opt happens while the operator cannot write
// there yet. These helpers write through the tree with a `sudo tee` / `sudo
// cp` fallback: the direct write works on every later run, the fallback covers
// the bootstrap. An operator without sudo gets the original error.

// canSudo reports whether a failed direct write has anywhere to fall back to.
func (i *Installer) canSudo() bool {
	return i.deps.Euid() == 0 || i.deps.hasBinary("sudo")
}

// writeInstalled stores content at path, falling back to sudo tee.
func (i *Installer) writeInstalled(ctx context.Context, path string, content []byte, mode os.FileMode) error {
	if err := writeFileAtomic(path, content, mode); err == nil {
		return nil
	} else if !i.canSudo() {
		return err
	}
	if err := i.deps.Run.Run(ctx, Command{Path: "tee", Args: []string{path}, Sudo: true,
		Stdin: bytes.NewReader(content), Stdout: io.Discard}); err != nil {
		return err
	}
	return i.deps.Run.Run(ctx, Command{Path: "chmod", Args: []string{fmt.Sprintf("%04o", mode), path}, Sudo: true})
}

// copyInstalled copies src to dst, falling back to a sudo-staged rename. A
// direct `sudo cp` would die on ETXTBSY whenever the running service keeps
// the target mapped; staging a temporary file and renaming it over the target
// is exactly what copyFileAtomic does when it can write.
func (i *Installer) copyInstalled(ctx context.Context, src, dst string, mode os.FileMode) error {
	if err := copyFileAtomic(src, dst, mode); err == nil {
		return nil
	} else if !i.canSudo() {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".sudo-tmp"
	if err := i.deps.Run.Run(ctx, Command{Path: "tee", Args: []string{tmp}, Sudo: true, Stdin: in, Stdout: io.Discard}); err != nil {
		return err
	}
	if err := i.deps.Run.Run(ctx, Command{Path: "chmod", Args: []string{fmt.Sprintf("%04o", mode), tmp}, Sudo: true}); err != nil {
		return err
	}
	return i.deps.Run.Run(ctx, Command{Path: "mv", Args: []string{tmp, dst}, Sudo: true})
}

// copyFileAtomic copies src to dst through a temporary file, so a failure never
// leaves a partial program where the service expects one. The copy preserves
// only mode, which is what an installed binary needs.
func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// fileExists reports whether path exists as a regular file or directory.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// mkdirAll creates dir with the given mode, reported with the path for context.
func mkdirAll(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}
