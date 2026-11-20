//go:build linux && dqlite

package app

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

const receivedModelLogoOriginDirectory = ".model-logo-origin"

// Only the content-addressed, flat logo names used by the API can carry an
// origin marker. Older and nested material paths remain source-unknown.
func receivedModelLogoOriginName(name, digest string) bool {
	if path.Dir(name) != "model-logos" || !strings.HasPrefix(path.Base(name), digest+".") {
		return false
	}
	switch strings.TrimPrefix(path.Base(name), digest) {
	case ".png", ".jpg", ".gif", ".webp":
		return true
	default:
		return false
	}
}

// The stage is the inode this invocation just published. Never derive a
// marker from an existing final name: a retry cannot establish its origin.
func recordReceivedModelLogoOrigin(root *os.Root, stage, final string) error {
	marker := path.Join("model-logos", receivedModelLogoOriginDirectory, path.Base(final))
	if err := ensureMaterialParent(root, marker); err != nil {
		return err
	}
	dir, err := openPlainMaterialDirectory(root, path.Dir(marker))
	if err != nil {
		return err
	}
	info, err := dir.Stat(".")
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("model logo origin directory is not private")
	}
	stageInfo, err := root.Lstat(stage)
	if err != nil || !stageInfo.Mode().IsRegular() {
		return errors.New("received model logo stage is not a regular file")
	}
	finalInfo, err := root.Lstat(final)
	if err != nil || !os.SameFile(stageInfo, finalInfo) {
		return errors.New("received model logo changed before origin marker")
	}
	if err := root.Link(stage, marker); err != nil {
		return fmt.Errorf("record received model logo origin: %w", err)
	}
	// If the published name changed while the marker was linked, withdraw the
	// diagnostic proof. No marker is ever used as deletion authorization.
	finalInfo, err = root.Lstat(final)
	if err != nil || !os.SameFile(stageInfo, finalInfo) {
		return errors.Join(errors.New("received model logo changed after origin marker"), root.Remove(marker), syncRootDir(root, path.Dir(marker)))
	}
	if err := syncRootDir(root, path.Dir(marker)); err != nil {
		return errors.Join(err, root.Remove(marker), syncRootDir(root, path.Dir(marker)))
	}
	return nil
}
