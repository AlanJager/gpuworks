package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ErrMaintenanceFrozen means a writer was refused while maintenance is frozen.
var ErrMaintenanceFrozen = errors.New("maintenance freeze rejected the write")

// ErrBusinessClosed means business traffic is not accepted yet.
var ErrBusinessClosed = errors.New("business traffic is closed")

// Maintenance freezes writers and remembers the checkpoint to restore.
type Maintenance struct {
	Frozen       bool
	Checkpoint   string
	BusinessOpen bool
}

// FreezeMaintenance copies the directory into checkpoint and rejects writers.
func FreezeMaintenance(dir, checkpoint string) (Maintenance, error) {
	if err := copyDirFiles(dir, checkpoint); err != nil {
		return Maintenance{}, err
	}
	return Maintenance{Frozen: true, Checkpoint: checkpoint}, nil
}

// AllowWrite stores body only while maintenance is thawed.
func AllowWrite(state Maintenance, dir, name string, body []byte) error {
	if state.Frozen {
		return ErrMaintenanceFrozen
	}
	if name != filepath.Base(name) || name == "" {
		return errors.New("write requires a single file name")
	}
	return os.WriteFile(filepath.Join(dir, name), body, 0600)
}

// RestoreFreezePoint copies the checkpoint back over the directory and stays frozen.
func RestoreFreezePoint(state Maintenance, dir string) (Maintenance, error) {
	if state.Checkpoint == "" {
		return state, errors.New("maintenance has no freeze point")
	}
	if err := copyDirFiles(state.Checkpoint, dir); err != nil {
		return state, err
	}
	state.Frozen = true
	state.BusinessOpen = false
	return state, nil
}

// ThawMaintenance allows writes again and keeps the checkpoint. Business stays closed.
func ThawMaintenance(state Maintenance) Maintenance {
	state.Frozen = false
	state.BusinessOpen = false
	return state
}

// OpenBusiness accepts traffic only when maintenance is thawed and the member
// service gate is already business-ready. It does not start the multi-member app.
func OpenBusiness(state Maintenance, service MemberService) (Maintenance, error) {
	if state.Frozen || !service.BusinessReady {
		state.BusinessOpen = false
		return state, ErrBusinessClosed
	}
	state.BusinessOpen = true
	return state, nil
}

func copyDirFiles(from, to string) error {
	if err := os.MkdirAll(to, 0700); err != nil {
		return err
	}
	if err := writeTree(from, to); err != nil {
		return err
	}
	return removeTreeExtras(to, from)
}

func writeTree(from, to string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		src := filepath.Join(from, entry.Name())
		dst := filepath.Join(to, entry.Name())
		if entry.IsDir() {
			if err := os.MkdirAll(dst, 0700); err != nil {
				return err
			}
			if err := writeTree(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := copyOneFile(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyOneFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	in.Close()
	return errors.Join(copyErr, closeErr)
}

func removeTreeExtras(dir, checkpoint string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		dst := filepath.Join(dir, entry.Name())
		src := filepath.Join(checkpoint, entry.Name())
		info, err := os.Stat(src)
		if err != nil {
			if os.IsNotExist(err) {
				if err := os.RemoveAll(dst); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if entry.IsDir() && info.IsDir() {
			if err := removeTreeExtras(dst, src); err != nil {
				return err
			}
		}
	}
	return nil
}
