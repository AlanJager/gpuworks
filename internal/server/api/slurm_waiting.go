package api

import (
	"context"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Why an entry has no node yet (dto.SlurmCluster.Waiting). The reason is read
// from facts the server already has, plus a little it sees as it goes:
//
//   - no_compute_nodes: the cloud's own listing of the cluster says it has no
//     compute nodes (provider.HPCClusterListing.ComputeNodes). Nothing to
//     install the health check's agents on yet. Read live, not remembered.
//   - server_unreachable: the last check or apply the platform ran on the
//     cluster found a host that cannot reach the server's address.
//   - joining: the setup was seen run a moment ago (within slurmJoinWindow)
//     and no node has asked the server yet. The health check reaches the
//     nodes within one to one and a half intervals, so silence is normal.
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
// claims what it did not see: slurmActivity.covers says an entry was created
// after the server began watching, and only such an entry gets not_applied or
// no_node_called.

// slurmActivity is what the server saw of each entry since it began watching.
type slurmActivity struct {
	mu      sync.Mutex
	started time.Time
	now     func() time.Time // time.Now unless a test sets it
	by      map[uint]*slurmSeen
}

// slurmSeen is what was seen of one entry.
type slurmSeen struct {
	// setupAt is when the setup was last seen run (fetched by a person, or
	// applied by the platform); zero when it was not.
	setupAt    time.Time
	nodeCalled bool // a node asked token-status or enroll with the entry's token
	// unreachable are the hosts the last check or apply found unable to reach
	// the server; nil when it found none (or none ran).
	unreachable []provider.SlurmReachHost
}

// begin starts the watch: what happens to an entry created from now on is
// seen from its first minute.
func (s *slurmActivity) begin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = s.clock()
}

// entry returns the record of cluster id, creating it, with mu held.
func (s *slurmActivity) entry(id uint) *slurmSeen {
	if s.by == nil {
		s.by = map[uint]*slurmSeen{}
	}
	e := s.by[id]
	if e == nil {
		e = &slurmSeen{}
		s.by[id] = e
	}
	return e
}

func (s *slurmActivity) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// setupRan notes that the setup was run on the entry's cluster now; a later
// run starts the window again.
func (s *slurmActivity) setupRan(id uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(id).setupAt = s.clock()
}

// setupSeen says the setup was seen run.
func (e slurmSeen) setupSeen() bool { return !e.setupAt.IsZero() }

// joining says the setup was seen run less than slurmJoinWindow before now:
// nodes may still be on their way, so nothing is yet wrong.
func (e slurmSeen) joining(now time.Time) bool {
	return e.setupSeen() && now.Sub(e.setupAt) < slurmJoinWindow
}

// nodeAsked notes that a node asked the server with the entry's token.
func (s *slurmActivity) nodeAsked(id uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(id).nodeCalled = true
}

// reached notes what a check or apply found: the hosts that cannot reach the
// server, none clearing an earlier finding.
func (s *slurmActivity) reached(id uint, reach provider.SlurmReach) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(id)
	e.unreachable = reach.Failed()
}

// forget drops what was seen of a deleted entry.
func (s *slurmActivity) forget(id uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.by, id)
}

// seen is a copy of the entry's record and whether the server has been
// watching since before the entry was created.
func (s *slurmActivity) seen(c store.SlurmCluster) (slurmSeen, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out slurmSeen
	if e := s.by[c.ID]; e != nil {
		out = *e
		out.unreachable = append([]provider.SlurmReachHost(nil), e.unreachable...)
	}
	return out, !c.CreatedAt.Before(s.started)
}

// slurmWaitingReason is why an entry that has no node yet has none, "" when
// nothing known says. computeNodes is how many compute nodes the cloud lists
// in the entry's cluster, nil when it does not say; now is read against the
// window nodes have to join in.
func slurmWaitingReason(seen slurmSeen, covered bool, computeNodes *int, now time.Time) (string, []provider.SlurmReachHost) {
	switch {
	case computeNodes != nil && *computeNodes == 0:
		return "no_compute_nodes", nil
	case len(seen.unreachable) > 0:
		return "server_unreachable", seen.unreachable
	case seen.nodeCalled || !covered:
		return "", nil
	case seen.joining(now):
		return "joining", nil
	case seen.setupSeen():
		return "no_node_called", nil
	}
	return "not_applied", nil
}

// slurmCloudNodes reads how many compute nodes the cloud lists for each
// cluster an account was asked about, once per account and wait; accounts the
// cloud did not answer for read as unknown.
type slurmCloudNodes struct {
	a     *apiHandlers
	ctx   context.Context
	lists map[uint]map[string]*int
}

func (n *slurmCloudNodes) of(c store.SlurmCluster) *int {
	if c.ProviderID == nil || n.a.d.Providers == nil {
		return nil
	}
	byCluster, ok := n.lists[*c.ProviderID]
	if !ok {
		byCluster = map[string]*int{}
		list, err := n.a.d.Providers.CachedHPCClusters(n.ctx, *c.ProviderID, slurmCloudListAge)
		if err == nil && list.Access == "" {
			for _, cl := range list.Clusters {
				byCluster[cl.ID] = cl.ComputeNodes
			}
		}
		if n.lists == nil {
			n.lists = map[uint]map[string]*int{}
		}
		n.lists[*c.ProviderID] = byCluster
	}
	return byCluster[c.ProviderClusterID]
}

// slurmWaiting is the DTO of an entry's waiting reason, nil when it has none
// to give: the entry has a node, is revoked, or nothing known says why.
func slurmWaiting(reason string, hosts []provider.SlurmReachHost) *dto.SlurmWaiting {
	if reason == "" {
		return nil
	}
	out := &dto.SlurmWaiting{Reason: reason}
	for _, h := range hosts {
		out.Hosts = append(out.Hosts, dto.SlurmReachHost{Role: h.Role, Name: h.Name, Status: h.Status, Reason: h.Reason})
	}
	return out
}

// slurmReachDTO is what a check or apply found about the server's address.
func slurmReachDTO(reach provider.SlurmReach) dto.SlurmReach {
	out := dto.SlurmReach{Hosts: make([]dto.SlurmReachHost, 0, len(reach.Hosts)), NodesUnlisted: reach.NodesUnlisted, NodesPermission: reach.NodesPermission}
	for _, h := range reach.Hosts {
		out.Hosts = append(out.Hosts, dto.SlurmReachHost{Role: h.Role, Name: h.Name, Status: h.Status, Reason: h.Reason})
	}
	return out
}
