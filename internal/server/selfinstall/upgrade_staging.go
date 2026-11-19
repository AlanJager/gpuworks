package selfinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// upgradeStagingName is the private directory a command-line upgrade of a
// server in the shared application root downloads and unpacks the release in.
const upgradeStagingName = ".upgrade-staging"

// upgradeStaging is where a command-line upgrade keeps the release it fetched
// and the binary it unpacked until that binary replaces the installed one.
//
// In a dedicated tree that is tmp/, as it always was: the service account owns
// the whole tree, binary included. In the shared application root tmp/ is the
// service account's too, but the binary is root's: root asks the staged binary
// for its release and then runs it (sudo gpuworks upgrade, supervisor hooks
// that run before the service as root). A binary staged in tmp/ could be
// replaced or rewritten by the service account before it is moved into place,
// and would keep that account as its owner afterwards. There the release is
// staged in a directory of the shared root itself that only this run's user can
// enter (same filesystem, so the binary still moves into place with a rename),
// and the binary is checked against what the archive held before it moves.
type upgradeStaging struct {
	dir string
	// private is the identity of the private directory; nil for tmp/.
	private os.FileInfo
	// staged is the unpacked binary and sum the sha256 of the bytes the
	// archive held for it.
	staged string
	sum    string
}

func (l Layout) upgradeStagingDir() string { return filepath.Join(l.Dir, upgradeStagingName) }

// upgradeStagingDirectory returns the directory this upgrade stages its
// release in, creating it on first use.
func (i *Installer) upgradeStagingDirectory() (string, error) {
	if i.staging != nil {
		return i.staging.dir, nil
	}
	if !i.installedInSharedRoot() {
		if err := mkdirAll(i.layout.TmpDir(), dirMode); err != nil {
			return "", err
		}
		i.staging = &upgradeStaging{dir: i.layout.TmpDir()}
		return i.staging.dir, nil
	}
	dir := i.layout.upgradeStagingDir()
	// A run that died before its cleanup leaves the directory behind; one
	// that is not this run's own kind (a private directory of root or this
	// operator) was put there by someone else, and is left for the operator.
	if info, err := lstatOwned(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !trustedWriteOwner(info) {
			return "", fmt.Errorf("refusing to upgrade: %s is not a private directory of root; check who created it and remove it", dir)
		}
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("remove the staging directory a previous upgrade left: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the private staging directory %s: %w; a server in the shared application root %s is upgraded as root (for example `sudo %s upgrade`)",
			dir, err, i.layout.Dir, i.layout.Binary())
	}
	info, err := lstatOwned(dir)
	if err != nil {
		return "", err
	}
	// Mkdir applies the umask; the directory must be exactly private.
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
		if info, err = lstatOwned(dir); err != nil {
			return "", err
		}
	}
	i.staging = &upgradeStaging{dir: dir, private: info}
	return dir, nil
}

// stageUpgradeBinary unpacks the binary from archive into the staging
// directory and remembers what the archive held.
func (i *Installer) stageUpgradeBinary(archive string) (string, error) {
	dir, err := i.upgradeStagingDirectory()
	if err != nil {
		return "", err
	}
	staged := filepath.Join(dir, binaryName+".new")
	sum, err := extractBinarySum(archive, staged)
	if err != nil {
		return "", err
	}
	i.staging.staged, i.staging.sum = staged, sum
	return staged, nil
}

// removeUpgradeStaging deletes what the upgrade staged; on success the binary
// has already been moved into place.
func (i *Installer) removeUpgradeStaging() {
	if i.staging == nil {
		_ = os.Remove(filepath.Join(i.layout.TmpDir(), binaryName+".new"))
		return
	}
	if i.staging.private == nil {
		_ = os.Remove(filepath.Join(i.staging.dir, binaryName+".new"))
	} else if current, err := lstatOwned(i.staging.dir); err == nil && sameInode(i.staging.private, current) {
		_ = os.RemoveAll(i.staging.dir)
	}
	i.staging = nil
}

// installStagedBinary moves the staged binary over the installed one. In the
// shared application root it first checks that the staging directory is still
// the private one this run created and that the binary is what the archive
// held, makes it root's and 0755, and makes the installed binary root's again
// after the move, before anything starts it.
func (i *Installer) installStagedBinary(ctx context.Context, staged string) error {
	target := i.layout.Binary()
	if i.staging == nil || i.staging.private == nil {
		return replaceBinary(staged, target)
	}
	if err := i.staging.verify(staged); err != nil {
		return fmt.Errorf("refusing to install the staged binary: %w", err)
	}
	mode := fmt.Sprintf("%04o", binaryMode)
	for _, cmd := range []Command{
		{Path: "chown", Args: []string{"-h", "0:0", staged}, Sudo: true},
		{Path: "chmod", Args: []string{mode, staged}, Sudo: true},
	} {
		if err := i.deps.Run.Run(ctx, cmd); err != nil {
			return err
		}
	}
	if err := os.Rename(staged, target); err != nil {
		return err
	}
	return i.rootOwnSharedBinary(ctx)
}

// verify checks the private staging directory and the staged binary in it.
func (s *upgradeStaging) verify(staged string) error {
	current, err := lstatOwned(s.dir)
	if err != nil {
		return err
	}
	if !sameInode(s.private, current) || !current.IsDir() || current.Mode().Perm() != 0o700 || !trustedWriteOwner(current) {
		return fmt.Errorf("the private staging directory %s was replaced or opened up", s.dir)
	}
	if filepath.Clean(staged) != s.staged || s.sum == "" {
		return fmt.Errorf("%s is not the binary this upgrade unpacked", staged)
	}
	f, err := os.OpenFile(staged, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || linkCount(info) > 1 {
		return fmt.Errorf("%s is not a regular file with a single link", staged)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != s.sum {
		return fmt.Errorf("%s changed after it was unpacked (sha256 %s, the archive held %s)", staged, got, s.sum)
	}
	return nil
}
