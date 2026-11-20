//go:build linux

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// The kernel exit status cannot be reconstructed after the parent has reaped
// the child. These private records retain that otherwise unavailable fact.
type clusterInstallProcessOwner struct {
	clusterInstallPID
	Supervisor     clusterInstallPID
	Boot           string
	Nonce          string
	Kind           string
	ExecutableHash string
}

type clusterInstallProcessExit struct {
	Owner  clusterInstallProcessOwner
	Status *uint32
}

func clusterInstallBootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if len(value) != 36 || !clusterInstallHex(strings.ReplaceAll(value, "-", ""), 32) {
		return "", errors.New("invalid kernel boot identity")
	}
	return value, nil
}

func clusterInstallHex(value string, length int) bool {
	_, err := hex.DecodeString(value)
	return len(value) == length && value == strings.ToLower(value) && err == nil
}

func clusterInstallExecutableHash(req clusterInstallNodeRequest) (string, error) {
	f, err := os.Open(req.Dir + "/gpuworks")
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func clusterInstallPrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("process record directory is not private and owned")
	}
	return nil
}

func clusterInstallReadPrivate(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	return clusterInstallReadPrivateFile(f)
}

func clusterInstallReadPrivateFile(f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	invalid := func() error {
		reason := "process record is not a bounded private owned regular file"
		if ok {
			reason = fmt.Sprintf("%s (mode=%#o uid=%d links=%d size=%d)", reason, info.Mode(), stat.Uid, stat.Nlink, info.Size())
		}
		return &os.PathError{Op: "read", Path: f.Name(), Err: errors.New(reason)}
	}
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Geteuid()) || info.Size() <= 0 || info.Size() > 8192 {
		return nil, invalid()
	}
	if stat.Nlink == 0 {
		// The publisher can unlink a completed pending marker after this reader
		// opens it but before fstat. No detached inode bytes are accepted. Only
		// a validated private inode whose namespace is now absent is ENOENT;
		// a replacement, hard link or unsafe attributes still fail closed.
		if _, err := os.Lstat(f.Name()); errors.Is(err, os.ErrNotExist) {
			return nil, &os.PathError{Op: "read", Path: f.Name(), Err: os.ErrNotExist}
		}
		return nil, invalid()
	}
	if stat.Nlink != 1 {
		return nil, invalid()
	}
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || len(raw) != int(info.Size()) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("process record %q changed during read", f.Name())
	}
	return raw, nil
}

func clusterInstallDecodeRecord(raw []byte, target any) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return errors.New("invalid process record JSON")
	}
	// Only our writer's canonical representation is accepted: missing fields,
	// duplicate fields and unknown keys must not turn a missing status into zero.
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(raw, canonical) {
		return errors.New("noncanonical process record")
	}
	return nil
}

func clusterInstallPublishRecord(path string, raw []byte, replace bool) error {
	return clusterInstallPublishRecordSync(path, raw, replace, clusterInstallSyncRecordDir)
}

func clusterInstallPublishRecordSync(path string, raw []byte, replace bool, syncDirectory func(string) error) error {
	if err := clusterInstallPrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if replace {
		if _, err := os.Lstat(path); err == nil {
			if _, err := clusterInstallReadPrivate(path); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".process-record-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if replace {
		err = os.Rename(f.Name(), path)
	} else {
		err = unix.Renameat2(unix.AT_FDCWD, f.Name(), unix.AT_FDCWD, path, unix.RENAME_NOREPLACE)
	}
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func clusterInstallSyncRecordDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func clusterInstallOwnerPath(req clusterInstallNodeRequest, nonce, suffix string) string {
	return req.Dir + "/processes/" + nonce + "." + suffix + ".json"
}

func clusterInstallReadOwner(req clusterInstallNodeRequest, kind string, record clusterInstallPID) (clusterInstallProcessOwner, error) {
	var owner clusterInstallProcessOwner
	if err := clusterInstallPrivateDir(req.Dir + "/processes"); err != nil {
		return owner, fmt.Errorf("owned supervisor record unavailable: %w", err)
	}
	nonce, err := clusterInstallReadPrivate(req.Dir + "/" + kind + ".launch")
	if err != nil || !clusterInstallHex(string(nonce), 32) {
		return owner, errors.New("owned process has no valid supervisor launch identity")
	}
	raw, err := clusterInstallReadPrivate(clusterInstallOwnerPath(req, string(nonce), "owner"))
	if err != nil {
		return owner, err
	}
	if err := clusterInstallDecodeRecord(raw, &owner); err != nil {
		return owner, err
	}
	boot, err := clusterInstallBootID()
	if err != nil {
		return owner, err
	}
	executable, err := clusterInstallExecutableHash(req)
	if err != nil {
		return owner, err
	}
	if owner.clusterInstallPID != record || owner.Kind != kind || owner.Nonce != string(nonce) || owner.Boot != boot || owner.ExecutableHash != executable || owner.Supervisor.PID <= 1 || owner.Supervisor.Start == "" {
		return owner, errors.New("owned supervisor identity differs from the pinned process")
	}
	return owner, nil
}

func clusterInstallReadExit(req clusterInstallNodeRequest, owner clusterInstallProcessOwner) (bool, error) {
	raw, err := clusterInstallReadPrivate(clusterInstallOwnerPath(req, owner.Nonce, "exit"))
	if errors.Is(err, os.ErrNotExist) {
		start, alive, identityErr := clusterInstallProcessIdentity(owner.Supervisor.PID)
		if identityErr != nil {
			return false, identityErr
		}
		if !alive || start != owner.Supervisor.Start {
			// Publication may race the first ENOENT read. After parent death no
			// further publication can occur, so re-read before declaring unknown.
			raw, err = clusterInstallReadPrivate(clusterInstallOwnerPath(req, owner.Nonce, "exit"))
			if errors.Is(err, os.ErrNotExist) {
				return false, errors.New("owned supervisor exited without a terminal wait status")
			}
		} else {
			return false, nil
		}
	}
	if err != nil {
		return false, err
	}
	pending, pendingErr := clusterInstallReadPrivate(clusterInstallOwnerPath(req, owner.Nonce, "pending"))
	if pendingErr == nil {
		var recorded clusterInstallProcessOwner
		if err := clusterInstallDecodeRecord(pending, &recorded); err != nil || recorded != owner {
			return false, errors.New("invalid terminal publication pending marker")
		}
		start, alive, err := clusterInstallProcessIdentity(owner.Supervisor.PID)
		if err != nil {
			return false, err
		}
		if alive && start == owner.Supervisor.Start {
			return false, nil
		}
		// Parent death may race marker removal, just as it can race the receipt.
		if _, err := clusterInstallReadPrivate(clusterInstallOwnerPath(req, owner.Nonce, "pending")); !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("owned supervisor did not complete durable terminal publication")
		}
	} else if !errors.Is(pendingErr, os.ErrNotExist) {
		return false, pendingErr
	}
	var receipt clusterInstallProcessExit
	if err := clusterInstallDecodeRecord(raw, &receipt); err != nil {
		return false, err
	}
	if receipt.Owner != owner || receipt.Status == nil {
		return false, errors.New("terminal wait status is missing or belongs to another launch")
	}
	status := syscall.WaitStatus(*receipt.Status)
	if status == 0 {
		return true, nil
	}
	if *receipt.Status > 0xffff {
		return false, errors.New("invalid terminal kernel wait status")
	}
	if status.Signaled() {
		return true, fmt.Errorf("owned process terminated by signal %d; logs retained", status.Signal())
	}
	if status.Exited() && status.ExitStatus() != 0 {
		return true, fmt.Errorf("owned process exit status %d is not a clean exit; logs retained", status.ExitStatus())
	}
	return false, errors.New("terminal status does not prove a completed process exit")
}

func clusterInstallPreviousExits(req clusterInstallNodeRequest) error {
	for _, kind := range []string{"serve", "join"} {
		raw, err := clusterInstallReadPrivate(req.Dir + "/" + kind + ".pid")
		if errors.Is(err, os.ErrNotExist) {
			if _, err := os.Lstat(req.Dir + "/" + kind + ".launch"); err == nil {
				return errors.New("previous supervised launch lost its PID record")
			}
			continue
		}
		if err != nil {
			return err
		}
		var record clusterInstallPID
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		owner, err := clusterInstallReadOwner(req, kind, record)
		if err != nil {
			return err
		}
		complete, err := clusterInstallReadExit(req, owner)
		if !complete {
			return errors.Join(errors.New("previous owned supervisor has not proved exit; logs retained"), err)
		}
		parentFD, pinErr := clusterInstallPinSupervisor(req, owner)
		if pinErr != nil {
			return pinErr
		}
		if parentFD >= 0 {
			parents := []unix.PollFd{{Fd: int32(parentFD), Events: unix.POLLIN}}
			_, pollErr := unix.Poll(parents, 0)
			unix.Close(parentFD)
			if pollErr != nil {
				return pollErr
			}
			if parents[0].Revents&unix.POLLIN == 0 {
				return errors.New("previous owned supervisor has not finished exiting")
			}
		}
		// An explicit start may follow an intentional fault. Preserve that
		// generation's receipt; failed Stop never advances to Start automatically.
	}
	return nil
}
