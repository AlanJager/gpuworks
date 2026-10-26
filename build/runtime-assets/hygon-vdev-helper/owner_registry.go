package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The native API exposes no creation generation. This registry is the current
// owner of each numeric ID, and is protected by the host-wide vdev lock.
type currentOwner struct {
	Token  string `json:"owner_token"`
	Active bool   `json:"active"`
}

func newOwnerToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func ownerPath(dir string, id int) string {
	return filepath.Join(dir, ".gpuworks-owners", fmt.Sprintf("vdev%d.json", id))
}

func readCurrentOwner(dir string, id int) (currentOwner, error) {
	data, err := os.ReadFile(ownerPath(dir, id))
	if err != nil {
		return currentOwner{}, err
	}
	var owner currentOwner
	if err := json.Unmarshal(data, &owner); err != nil || len(owner.Token) != 32 {
		return currentOwner{}, fmt.Errorf("invalid current owner for vdev %d", id)
	}
	return owner, nil
}

func requireCurrentOwner(dir string, id int, token string) (currentOwner, error) {
	if len(token) != 32 {
		return currentOwner{}, fmt.Errorf("vdev %d requires a valid owner token", id)
	}
	owner, err := readCurrentOwner(dir, id)
	if err != nil {
		return currentOwner{}, fmt.Errorf("read current owner of vdev %d: %w", id, err)
	}
	if owner.Token != token {
		return currentOwner{}, fmt.Errorf("vdev %d belongs to a different creation; refusing stale ownership", id)
	}
	return owner, nil
}

func writeCurrentOwner(dir string, id int, owner currentOwner) error {
	path := ownerPath(dir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".owner-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Fence an old active record only when the native device is confirmed absent.
// This runs before a new create, while the same host lock is held.
func fenceAbsentOwners(api deviceAPI, dir string) error {
	entries, err := os.ReadDir(filepath.Join(dir, ".gpuworks-owners"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".owner-") {
			// An interrupted atomic write may leave a temp file behind.
			continue
		}
		var id int
		if _, err := fmt.Sscanf(entry.Name(), "vdev%d.json", &id); err != nil || entry.Name() != fmt.Sprintf("vdev%d.json", id) {
			return fmt.Errorf("invalid vdev owner file %q", entry.Name())
		}
		owner, err := readCurrentOwner(dir, id)
		if err != nil {
			return err
		}
		if !owner.Active {
			continue
		}
		_, exists, err := api.Inspect(id)
		if err != nil {
			return err
		}
		if !exists {
			owner.Active = false
			if err := writeCurrentOwner(dir, id, owner); err != nil {
				return err
			}
		}
	}
	return nil
}
