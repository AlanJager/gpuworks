package slurmcluster

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// When the platform runs an entry's setup on a cloud cluster's manager node,
// the command it hands the cloud's command channel (Cloud Assistant keeps
// invocation records for about 30 days) must not carry the entry's long-lived
// enrollment token. It carries a one-time URL instead: a random code that
// serves the entry's setup script once, within OnceTTL. The script body,
// which the manager downloads and runs, is what carries the token (into the
// root-only token file). The codes live in server memory only: a restart
// forgets them, which costs nothing because an apply waits for its own run.

// OnceTTL is how long a one-time code can fetch the script.
const OnceTTL = 10 * time.Minute

type onceCode struct {
	cluster uint
	expires time.Time
}

// IssueSetupCode makes a code that serves the setup script of the entry
// cluster once. Expired codes are dropped as new ones are issued.
func (m *Memory) IssueSetupCode(cluster uint) (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	code := hex.EncodeToString(raw[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clockLocked()
	for k, v := range m.once {
		if !now.Before(v.expires) {
			delete(m.once, k)
		}
	}
	if m.once == nil {
		m.once = map[string]onceCode{}
	}
	m.once[code] = onceCode{cluster: cluster, expires: now.Add(OnceTTL)}
	return code, nil
}

// TakeSetupCode spends code: the entry it was issued for, true once and only
// within its lifetime.
func (m *Memory) TakeSetupCode(code string) (uint, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.once[code]
	if !ok {
		return 0, false
	}
	delete(m.once, code)
	if !m.clockLocked().Before(v.expires) {
		return 0, false
	}
	return v.cluster, true
}

// DiscardSetupCode drops a code that was not spent (the run ended without
// fetching it).
func (m *Memory) DiscardSetupCode(code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.once, code)
}
