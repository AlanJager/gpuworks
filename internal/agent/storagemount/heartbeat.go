package storagemount

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Heartbeat is the content of one control plane's heartbeat file on a shared export.
type Heartbeat struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Peer is another control plane seen through its heartbeat file.
type Peer struct {
	ID, Name, Version string
	LastSeen          time.Duration // how long before our own heartbeat it was refreshed
	Fresh             bool          // LastSeen <= the freshness window
}

var heartbeatIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func heartbeatDir(root string) string { return filepath.Join(root, MetaDirName, ControlPlanesDirName) }

func checkHeartbeatID(id string) error {
	if !heartbeatIDRe.MatchString(id) {
		return fmt.Errorf("invalid control plane id %q", id)
	}
	return nil
}

// WriteHeartbeat writes <root>/.gpuworks/control-planes/<id>.json through a temp file and rename
// and returns the file's modification time as stamped by the filesystem. These calls touch a
// shared path, so callers should run them through Manager.Do.
func WriteHeartbeat(root string, hb Heartbeat) (time.Time, error) {
	if err := checkHeartbeatID(hb.ID); err != nil {
		return time.Time{}, err
	}
	b, err := json.Marshal(hb)
	if err != nil {
		return time.Time{}, err
	}
	path := filepath.Join(heartbeatDir(root), hb.ID+".json")
	if err := writeFileAtomic(path, b, 0o644); err != nil {
		return time.Time{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// ReadPeers lists the heartbeat files of the other control planes. "Now" is the modification time
// of selfID's own file, i.e. the clock of the file server, never the local clock, so skew between
// this node and the server does not matter. A peer refreshed within freshFor of that is Fresh.
// It fails when selfID's own file is missing (write it first). Malformed files, temp files and
// files with other names are ignored. Peers are sorted by ID.
func ReadPeers(root, selfID string, freshFor time.Duration) ([]Peer, error) {
	if err := checkHeartbeatID(selfID); err != nil {
		return nil, err
	}
	dir := heartbeatDir(root)
	fi, err := os.Stat(filepath.Join(dir, selfID+".json"))
	if err != nil {
		return nil, fmt.Errorf("own heartbeat: %w", err)
	}
	now := fi.ModTime()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var peers []Peer
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || id == selfID || checkHeartbeatID(id) != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var hb Heartbeat
		if json.Unmarshal(b, &hb) != nil || hb.ID != id {
			continue
		}
		age := now.Sub(info.ModTime())
		if age < 0 {
			age = 0
		}
		peers = append(peers, Peer{ID: id, Name: hb.Name, Version: hb.Version, LastSeen: age, Fresh: age <= freshFor})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	return peers, nil
}

// RemoveHeartbeat deletes this control plane's heartbeat file; a missing file is not an error.
func RemoveHeartbeat(root, id string) error {
	if err := checkHeartbeatID(id); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(heartbeatDir(root), id+".json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
