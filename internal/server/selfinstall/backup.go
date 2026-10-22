package selfinstall

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Archive member names. A backup holds exactly what an upgrade must be able to
// put back: the database (with its write-ahead log), the key that decrypts the
// credentials stored in it, and the binary that was running.
const (
	backupDataPrefix = "data/"
	backupBinPrefix  = "bin/"
)

// backupMembers lists the files a snapshot takes from the installation, in the
// order they are written.
func (i *Installer) backupMembers() []string {
	members := []string{
		filepath.Join(i.layout.DataDir, "gpuworks.db"),
		filepath.Join(i.layout.DataDir, "gpuworks.db-wal"),
		filepath.Join(i.layout.DataDir, "gpuworks.db-shm"),
		filepath.Join(i.layout.DataDir, "master.key"),
		i.layout.ConfigPath(),
		i.layout.Binary(),
	}
	return members
}

// createBackup archives the current state before an upgrade replaces it and
// returns the archive path.
func (i *Installer) createBackup(ctx context.Context, releaseID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := mkdirAll(i.layout.BackupsDir, dirMode); err != nil {
		return "", err
	}
	at := i.deps.now()
	path := i.layout.BackupPath(releaseID, at)
	if err := i.writeBackup(path, at); err != nil {
		return "", err
	}
	return path, nil
}

// backupEntry is one file a snapshot takes and the name it carries inside.
type backupEntry struct {
	path, name string
}

// writeBackup streams the installation's member files into a tar.gz.
func (i *Installer) writeBackup(path string, at time.Time) error {
	members := i.backupMembers()
	entries := make([]backupEntry, 0, len(members))
	for _, member := range members {
		entries = append(entries, backupEntry{path: member, name: archiveName(i.layout, member)})
	}
	return i.writeBackupEntries(path, at, entries)
}

// writeBackupEntries writes a snapshot through a temporary file so an
// interrupted backup never looks like a usable one.
func (i *Installer) writeBackupEntries(path string, at time.Time, entries []backupEntry) error {
	tmp, err := os.CreateTemp(i.layout.BackupsDir, filepath.Base(path)+".part*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	// 0664 like the rest of the tree: the operator restores this snapshot
	// without sudo.
	if err := tmp.Chmod(0o664); err != nil {
		tmp.Close()
		return err
	}
	gz := gzip.NewWriter(tmp)
	writer := tar.NewWriter(gz)
	for _, entry := range entries {
		if err := addToArchive(writer, entry.path, entry.name, at); err != nil {
			writer.Close()
			gz.Close()
			tmp.Close()
			return err
		}
	}
	if err := writer.Close(); err != nil {
		gz.Close()
		tmp.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// archiveName maps an installed file to the name it carries inside the archive.
func archiveName(layout Layout, member string) string {
	switch member {
	case layout.ConfigPath():
		return serverconfig.FileName
	case layout.Binary():
		return binaryPath()
	default:
		return backupDataPrefix + filepath.Base(member)
	}
}

func binaryPath() string { return backupBinPrefix + binaryName }

// addToArchive writes one member. A missing optional file (a database that has
// never been checkpointed has no -wal, an installation made by hand has no
// manifest) is skipped rather than failing the backup; a missing required file
// is reported by the caller's restore expectations, not here.
func addToArchive(writer *tar.Writer, path, name string, at time.Time) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	header.ModTime = at
	header.Mode = int64(info.Mode().Perm())
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(writer, file)
	return err
}

// restoreBackup puts a snapshot back. The write-ahead log is removed first:
// restoring a database while a stale -wal from the newer version is present
// would leave sqlite replaying changes that belong to a schema it no longer
// has.
func (i *Installer) restoreBackup(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	defer gz.Close()

	if err := i.discardDatabaseLog(); err != nil {
		return err
	}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		target, err := restoreTarget(i.layout, header.Name)
		if err != nil {
			return err
		}
		mode := header.FileInfo().Mode().Perm()
		if mode == 0 {
			// The snapshot's own files always carry a mode; a missing one is a
			// hand-built archive, which gets the conservative defaults.
			mode = 0o600
			if filepath.Base(target) == binaryName {
				mode = binaryMode
			}
		}
		if err := writeRestored(reader, target, mode); err != nil {
			return err
		}
	}
}

// restoreTarget maps an archive member to where it belongs. A member that
// belongs nowhere is an error: silently ignoring it would restore a snapshot
// that is not the one that was taken.
func restoreTarget(layout Layout, name string) (string, error) {
	name = filepath.Clean(name)
	switch {
	case name == serverconfig.FileName:
		return layout.ConfigPath(), nil
	case name == backupBinPrefix+binaryName:
		return layout.Binary(), nil
	case strings.HasPrefix(name, backupDataPrefix):
		base := filepath.Base(name)
		if base != name[len(backupDataPrefix):] {
			return "", fmt.Errorf("backup member %q escapes the data directory", name)
		}
		return filepath.Join(layout.DataDir, base), nil
	default:
		return "", fmt.Errorf("backup member %q does not belong to an installation", name)
	}
}

// writeRestored writes one archive member, keeping the mode it had when the
// snapshot was taken. A restored installation must end up with the exact
// permissions it was running with — 0664 in the shared tree — otherwise a rollback would lock the service account out of its
// own database.
func writeRestored(reader io.Reader, target string, mode os.FileMode) error {
	if err := mkdirAll(filepath.Dir(target), dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".restore*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, io.LimitReader(reader, maxExtractedBinaryBytes)); err != nil {
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
	return os.Rename(tmpName, target)
}

// discardDatabaseLog removes the write-ahead log of the database that is about
// to be replaced.
func (i *Installer) discardDatabaseLog() error {
	for _, suffix := range []string{"-wal", "-shm"} {
		path := filepath.Join(i.layout.DataDir, "gpuworks.db"+suffix)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// pruneBackups keeps the newest keep snapshots. Backup names start with the
// release id and end with a UTC timestamp, so ordering by name orders by age.
func (i *Installer) pruneBackups(keep int) {
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(i.layout.BackupsDir)
	if err != nil {
		return
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tar.gz") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) <= keep {
		return
	}
	slices.Sort(names)
	slices.Reverse(names)
	for _, name := range names[keep:] {
		_ = os.Remove(filepath.Join(i.layout.BackupsDir, name))
	}
}
