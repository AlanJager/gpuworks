package expose

import (
	"encoding/json"
	"os"
	"sync"
)

// EnabledWatch answers whether the operator has the entry point enabled. The
// server asks while serving requests and the CLI may change the answer at any
// time, so it reads config.json again only when the file changed.
type EnabledWatch struct {
	path string

	mu      sync.Mutex
	seen    os.FileInfo
	enabled bool
}

// WatchEnabled watches this store's desired state.
func (s *Store) WatchEnabled() *EnabledWatch { return &EnabledWatch{path: s.ConfigPath()} }

// Enabled reports desired: enabled. A missing or unreadable configuration is
// not enabled.
func (w *EnabledWatch) Enabled() bool {
	info, err := os.Stat(w.path)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.seen, w.enabled = nil, false
		return false
	}
	// WriteConfig replaces the file by rename, so a rewrite is a different
	// file even when it lands within the same mtime tick.
	if w.seen != nil && os.SameFile(w.seen, info) && w.seen.ModTime().Equal(info.ModTime()) && w.seen.Size() == info.Size() {
		return w.enabled
	}
	w.seen, w.enabled = nil, false
	raw, err := os.ReadFile(w.path)
	if err != nil {
		return false // asked again next time
	}
	var c Config
	if json.Unmarshal(raw, &c) == nil {
		w.enabled = c.Desired == DesiredEnabled
	}
	w.seen = info
	return w.enabled
}
