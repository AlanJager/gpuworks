package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const upgradeFile = "upgrade.json"

// Upgrade is the marker of an upgrade in progress, <id>/upgrade.json: it
// is written before the running version stops and removed once the new
// version passed its health gate or the upgrade was rolled back. A server
// that finds one at boot was stopped in the middle of the upgrade and
// rolls back to From, restoring Snapshot, before it starts anything.
type Upgrade struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Snapshot names the snapshot of From's data the upgrade takes (it
	// may not exist yet: the marker is written first).
	Snapshot string    `json:"snapshot"`
	Started  time.Time `json:"started"`
}

// WriteUpgrade records that package id is being upgraded.
func (d Dir) WriteUpgrade(id string, u Upgrade) error {
	raw, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(d.Package(id), upgradeFile), append(raw, '\n'), 0o600)
}

// ReadUpgrade reads package id's upgrade marker; ok is false without one.
func (d Dir) ReadUpgrade(id string) (u Upgrade, ok bool, err error) {
	raw, err := os.ReadFile(filepath.Join(d.Package(id), upgradeFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Upgrade{}, false, nil
	}
	if err != nil {
		return Upgrade{}, false, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil {
		return Upgrade{}, false, fmt.Errorf("read %s upgrade marker: %w", id, err)
	}
	return u, true, nil
}

// RemoveUpgrade removes package id's upgrade marker, if any.
func (d Dir) RemoveUpgrade(id string) error {
	err := os.Remove(filepath.Join(d.Package(id), upgradeFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(d.Package(id))
}

// Upgrading returns the ids that have an upgrade marker, sorted.
func (d Dir) Upgrading() ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(d.Root, "*", upgradeFile))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, p := range paths {
		id := filepath.Base(filepath.Dir(p))
		if !strings.HasPrefix(id, ".") {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
