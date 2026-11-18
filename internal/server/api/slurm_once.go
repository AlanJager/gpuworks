package api

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// When the platform runs an entry's setup on a cloud cluster's manager node,
// the command it hands the cloud's command channel (Cloud Assistant keeps
// invocation records for about 30 days) must not carry the entry's long-lived
// enrollment token. It carries a one-time URL instead: a random code that
// serves the entry's setup script once, within slurmOnceTTL. The script body,
// which the manager downloads and runs, is what carries the token (into the
// root-only token file). The codes live in server memory only: a restart
// forgets them, which costs nothing because an apply waits for its own run.

// slurmOnceTTL is how long a one-time code can fetch the script.
const slurmOnceTTL = 10 * time.Minute

// slurmOnceCodes are the outstanding codes, each for one entry.
type slurmOnceCodes struct {
	mu  sync.Mutex
	now func() time.Time // time.Now unless a test sets it
	by  map[string]slurmOnceCode
}

type slurmOnceCode struct {
	cluster uint
	expires time.Time
}

func (c *slurmOnceCodes) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now().UTC()
}

// issue makes a code that serves the setup script of the entry cluster once.
func (c *slurmOnceCodes) issue(cluster uint) (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	code := hex.EncodeToString(raw[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	for k, v := range c.by {
		if !now.Before(v.expires) {
			delete(c.by, k)
		}
	}
	if c.by == nil {
		c.by = map[string]slurmOnceCode{}
	}
	c.by[code] = slurmOnceCode{cluster: cluster, expires: now.Add(slurmOnceTTL)}
	return code, nil
}

// take spends code: the entry it was issued for, true once and only within
// its lifetime.
func (c *slurmOnceCodes) take(code string) (uint, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.by[code]
	if !ok {
		return 0, false
	}
	delete(c.by, code)
	if !c.clock().Before(v.expires) {
		return 0, false
	}
	return v.cluster, true
}

// discard drops a code that was not spent (the run ended without fetching it).
func (c *slurmOnceCodes) discard(code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.by, code)
}

// forget drops every code of an entry that was deleted.
func (c *slurmOnceCodes) forget(cluster uint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.by {
		if v.cluster == cluster {
			delete(c.by, k)
		}
	}
}
