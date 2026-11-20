// Package slurmcluster holds what the server derives and remembers of each
// Slurm cluster entry (ADR 0075), apart from the HTTP handlers that serve
// it: the entry's state and the counts its view shows, the ingestion of the
// node lists its agents report, the one-time codes of the setup URL, and
// what the server saw of the entry's setup. The handlers decode, call this
// and map the answers to DTOs.
package slurmcluster

import (
	"sync"
	"time"
)

// Memory is what the server keeps of each entry in memory only, none of it
// a persisted fact: the digests of the node list it last stored (reports),
// the one-time setup codes (once) and what it saw of the setup (seen). A
// restart forgets it, which each part allows for. Forget drops all of it for
// a deleted entry, so nothing is left under its id.
type Memory struct {
	mu      sync.Mutex
	now     func() time.Time
	started time.Time
	reports map[uint]*listMemory
	once    map[string]onceCode
	seen    map[uint]*seen
}

// New starts the memory: what happens to an entry created from now on is
// seen from its first minute.
func New() *Memory {
	m := &Memory{}
	m.started = m.clock()
	return m
}

// SetClock makes the memory read time from now, and starts watching at the
// time it reads first; tests use it.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
	m.started = m.clockLocked()
}

// SetWatchedSince says the server began watching at t; tests use it to make
// an entry created earlier one the server did not see from its start.
func (m *Memory) SetWatchedSince(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = t
}

func (m *Memory) clock() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clockLocked()
}

func (m *Memory) clockLocked() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now().UTC()
}

// Now is the time the memory reads.
func (m *Memory) Now() time.Time { return m.clock() }

// Forget drops everything kept of the entry cluster: its list digests, its
// one-time codes and what was seen of its setup.
func (m *Memory) Forget(cluster uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reports, cluster)
	delete(m.seen, cluster)
	for code, c := range m.once {
		if c.cluster == cluster {
			delete(m.once, code)
		}
	}
}

// ListChanged forgets the digests of the entry's stored list after one of
// its rows was removed outside a report (a node deleted), so the next report
// stores the list whole again.
func (m *Memory) ListChanged(cluster uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reports, cluster)
}

// holds says whether anything is kept of the entry cluster; tests use it.
func (m *Memory) holds(cluster uint) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reports[cluster] != nil || m.seen[cluster] != nil {
		return true
	}
	for _, c := range m.once {
		if c.cluster == cluster {
			return true
		}
	}
	return false
}
