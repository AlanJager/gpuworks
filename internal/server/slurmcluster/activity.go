package slurmcluster

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/slurmboot"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Why an entry has no node yet (Waiting). The reason is read from facts the
// server already has, plus a little it sees as it goes:
//
//   - no_compute_nodes: the cloud's own listing of the cluster says it has no
//     compute nodes (provider.HPCClusterListing.ComputeNodes). Nothing to
//     install the health check's agents on yet. Read live, not remembered.
//   - server_unreachable: the last check or apply the platform ran on the
//     cluster found a host that cannot reach the server's address.
//   - joining: the setup was seen run a moment ago (within JoinWindow) and no
//     node has asked the server yet. The health check reaches the nodes
//     within one to one and a half intervals, so silence is normal.
//   - no_node_called: the window has passed and no node has asked the server
//     (token-status, enroll) since the setup was seen run (a person fetched
//     the setup command's script, or the platform applied it): the nodes
//     cannot reach the server's address, or the script is not on them.
//   - not_applied: the setup was not seen run.
//
// The last four are what the server saw happen, which no stored column
// holds, and which is only worth knowing while an entry waits for its first
// node. They are kept in memory, not in the database: a new persisted fact
// for a diagnosis that the next run of the same check re-derives is not
// worth a column and a migration. A restart forgets them, so the server never
// claims what it did not see: only an entry created after the server began
// watching gets not_applied or no_node_called.

// The waiting reasons.
const (
	WaitingNoComputeNodes    = "no_compute_nodes"
	WaitingServerUnreachable = "server_unreachable"
	WaitingJoining           = "joining"
	WaitingNoNodeCalled      = "no_node_called"
	WaitingNotApplied        = "not_applied"
)

// JoinWindow is how long after the setup was seen run an entry may have no
// node without that meaning anything: the health check runs on each node
// every slurmboot.DefaultIntervalSeconds and the install is spread over half
// of one, so nodes join within one to one and a half intervals; a further
// half interval is allowed for a check that has just run. While it lasts the
// entry waits as joining and is not stalled; after it, no node having called
// is a finding.
const JoinWindow = 2 * time.Duration(slurmboot.DefaultIntervalSeconds) * time.Second

// seen is what was seen of one entry.
type seen struct {
	// setupAt is when the setup was last seen run (fetched by a person, or
	// applied by the platform); zero when it was not.
	setupAt    time.Time
	nodeCalled bool // a node asked token-status or enroll with the entry's token
	// unreachable are the hosts the last check or apply found unable to reach
	// the server; nil when it found none (or none ran).
	unreachable []provider.SlurmReachHost
}

// entryLocked returns the record of cluster, creating it, with mu held.
func (m *Memory) entryLocked(cluster uint) *seen {
	if m.seen == nil {
		m.seen = map[uint]*seen{}
	}
	e := m.seen[cluster]
	if e == nil {
		e = &seen{}
		m.seen[cluster] = e
	}
	return e
}

// SetupRan notes that the setup was run on the entry's cluster now; a later
// run starts the window again.
func (m *Memory) SetupRan(cluster uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entryLocked(cluster).setupAt = m.clockLocked()
}

// NodeAsked notes that a node asked the server with the entry's token.
func (m *Memory) NodeAsked(cluster uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entryLocked(cluster).nodeCalled = true
}

// Reached notes what a check or apply found: the hosts that cannot reach the
// server, none clearing an earlier finding.
func (m *Memory) Reached(cluster uint, reach provider.SlurmReach) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entryLocked(cluster).unreachable = reach.Failed()
}

// Waiting is why an entry with no node yet has none.
type Waiting struct {
	// Reason is one of the Waiting* codes, "" when nothing known says.
	Reason string
	// Hosts are the hosts that cannot reach the server, for
	// WaitingServerUnreachable.
	Hosts []provider.SlurmReachHost
	// Joining: the setup was seen run within JoinWindow, so the entry is not
	// stalled however old it is.
	Joining bool
}

// Waiting says why the entry c, which has no node, has none. computeNodes is
// how many compute nodes the cloud lists in its cluster, nil when it does
// not say.
func (m *Memory) Waiting(c store.SlurmCluster, computeNodes *int) Waiting {
	m.mu.Lock()
	var e seen
	if got := m.seen[c.ID]; got != nil {
		e = *got
		e.unreachable = append([]provider.SlurmReachHost(nil), got.unreachable...)
	}
	covered := !c.CreatedAt.Before(m.started)
	now := m.clockLocked()
	m.mu.Unlock()
	joining := !e.setupAt.IsZero() && now.Sub(e.setupAt) < JoinWindow
	out := Waiting{Joining: covered && joining}
	switch {
	case computeNodes != nil && *computeNodes == 0:
		out.Reason = WaitingNoComputeNodes
	case len(e.unreachable) > 0:
		out.Reason, out.Hosts = WaitingServerUnreachable, e.unreachable
	case e.nodeCalled || !covered:
	case joining:
		out.Reason = WaitingJoining
	case !e.setupAt.IsZero():
		out.Reason = WaitingNoNodeCalled
	default:
		out.Reason = WaitingNotApplied
	}
	return out
}
