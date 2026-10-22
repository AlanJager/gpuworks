package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Bounds for TerminalSettings. Exported so the API layer validates against the
// same limits the store clamps to, rather than keeping a second copy.
const (
	TerminalIdleTimeoutMinLo = 1
	TerminalIdleTimeoutMinHi = 1440 // 24h; a longer-lived root shell should not be a setting
	TerminalGraceSecLo       = 1
	TerminalGraceSecHi       = 120
	TerminalMaxPerNodeLo     = 1
	TerminalMaxPerNodeHi     = 64
	TerminalMaxGlobalLo      = 1
	TerminalMaxGlobalHi      = 512
)

// TerminalSettings is the operator-configurable web terminal policy.
//
// Enabled is a kill switch: this feature exposes a root shell on every GPU node
// through the browser, so it must be possible to turn off without a redeploy.
type TerminalSettings struct {
	Enabled        bool `json:"enabled"`
	IdleTimeoutMin int  `json:"idle_timeout_min"`
	GraceSec       int  `json:"grace_sec"`
	MaxPerNode     int  `json:"max_per_node"`
	MaxGlobal      int  `json:"max_global"`
}

// DefaultTerminalSettings returns the seeded defaults.
func DefaultTerminalSettings() TerminalSettings {
	return TerminalSettings{
		Enabled:        true,
		IdleTimeoutMin: 15,
		GraceSec:       6,
		MaxPerNode:     4,
		MaxGlobal:      32,
	}
}

// IdleTimeout returns the idle timeout as a duration.
func (t TerminalSettings) IdleTimeout() time.Duration {
	return time.Duration(t.IdleTimeoutMin) * time.Minute
}

// Grace returns the SIGTERM-to-SIGKILL grace period as a duration.
func (t TerminalSettings) Grace() time.Duration {
	return time.Duration(t.GraceSec) * time.Second
}

// ValidateTerminalSettings rejects values outside the supported bounds. Zero is
// never silently accepted: a zero concurrency limit would disable the feature
// while still looking enabled, and a zero timeout would kill every session
// immediately.
func ValidateTerminalSettings(t TerminalSettings) error {
	for _, c := range []struct {
		name      string
		v, lo, hi int
	}{
		{"idle_timeout_min", t.IdleTimeoutMin, TerminalIdleTimeoutMinLo, TerminalIdleTimeoutMinHi},
		{"grace_sec", t.GraceSec, TerminalGraceSecLo, TerminalGraceSecHi},
		{"max_per_node", t.MaxPerNode, TerminalMaxPerNodeLo, TerminalMaxPerNodeHi},
		{"max_global", t.MaxGlobal, TerminalMaxGlobalLo, TerminalMaxGlobalHi},
	} {
		if c.v < c.lo || c.v > c.hi {
			return fmt.Errorf("terminal.%s must be between %d and %d", c.name, c.lo, c.hi)
		}
	}
	if t.MaxGlobal < t.MaxPerNode {
		return fmt.Errorf("terminal.max_global (%d) must be at least terminal.max_per_node (%d)", t.MaxGlobal, t.MaxPerNode)
	}
	return nil
}

// EnsureTerminalSettings seeds defaults when the key is unset.
func (s *Store) EnsureTerminalSettings() error {
	if strings.TrimSpace(s.GetSetting(SettingTerminalPolicy)) != "" {
		return nil
	}
	b, err := json.Marshal(DefaultTerminalSettings())
	if err != nil {
		return err
	}
	return s.SetSetting(SettingTerminalPolicy, string(b))
}

// TerminalSettings returns the current policy, falling back to defaults when the
// key is missing or unparseable. A corrupt setting must not take the feature
// down, and it must not silently unlock a larger limit either, so any field left
// out of the stored JSON keeps its default.
func (s *Store) TerminalSettings() TerminalSettings {
	out := DefaultTerminalSettings()
	raw := strings.TrimSpace(s.GetSetting(SettingTerminalPolicy))
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return DefaultTerminalSettings()
	}
	if err := ValidateTerminalSettings(out); err != nil {
		return DefaultTerminalSettings()
	}
	return out
}

// CreateTerminalSession opens an audit record and returns it with its ID set.
func (s *Store) CreateTerminalSession(sess *TerminalSession) (*TerminalSession, error) {
	if sess.StartedAt.IsZero() {
		sess.StartedAt = time.Now().UTC()
	}
	if err := s.db.Create(sess).Error; err != nil {
		return nil, err
	}
	s.Bus.PublishChange("terminal", uintKey(sess.ID), "started")
	return sess, nil
}

// CloseTerminalSession finalises an audit record with its transfer totals.
//
// Byte counts come from the io.Copy return values at teardown, so accounting
// costs nothing on the data path.
func (s *Store) CloseTerminalSession(id uint, bytesIn, bytesOut int64, reason string) error {
	now := time.Now().UTC()
	err := s.db.Model(&TerminalSession{}).Where("id = ? AND ended_at IS NULL", id).Updates(map[string]any{
		"ended_at":     now,
		"bytes_in":     bytesIn,
		"bytes_out":    bytesOut,
		"close_reason": reason,
	}).Error
	if err != nil {
		return err
	}
	s.Bus.PublishChange("terminal", uintKey(id), "closed")
	return nil
}

// CloseOrphanTerminalSessions finalises records left open by a server that
// stopped mid-session. Terminal sessions are process-scoped: when the server
// exits, every stream dies with it, so a record still marked open on startup
// describes a session that cannot exist. Leaving those rows open would make the
// audit table claim sessions are live indefinitely.
func (s *Store) CloseOrphanTerminalSessions() (int64, error) {
	res := s.db.Model(&TerminalSession{}).Where("ended_at IS NULL").Updates(map[string]any{
		"ended_at":     time.Now().UTC(),
		"close_reason": "server_restart",
	})
	return res.RowsAffected, res.Error
}

// OpenTerminalSessionCount reports how many records are still open. Used by
// tests and diagnostics; live concurrency limits are enforced in memory.
func (s *Store) OpenTerminalSessionCount() (int64, error) {
	var n int64
	err := s.db.Model(&TerminalSession{}).Where("ended_at IS NULL").Count(&n).Error
	return n, err
}

// TerminalSessions returns the most recent audit records, newest first.
func (s *Store) TerminalSessions(limit int) ([]TerminalSession, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []TerminalSession
	err := s.db.Order("started_at desc").Limit(limit).Find(&out).Error
	return out, err
}
