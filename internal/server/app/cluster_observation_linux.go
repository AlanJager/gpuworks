//go:build linux && dqlite

package app

import (
	"context"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// observeCluster takes the status report's observation for the running
// server. It skips the report's business probe: whoever asks is being served
// by this member already.
func observeCluster(ctx context.Context, cfg Config, timeout time.Duration) (ClusterObservation, error) {
	// A server that never initialised or joined a cluster is a single
	// server, whatever its build.
	member, err := selfinstall.ClusterMember(cfg.DataDir)
	if err != nil {
		return ClusterObservation{}, err
	}
	if !member {
		return ClusterObservation{ObservedAt: time.Now()}, nil
	}
	report, err := observeClusterStatus(ctx, cfg, "", timeout)
	if err != nil {
		return ClusterObservation{}, err
	}
	if err := ctx.Err(); err != nil {
		return ClusterObservation{}, err
	}
	return report.observation(), nil
}

// observation is the report as values. Every verdict comes from the method
// that words the corresponding report line.
func (s clusterStatus) observation() ClusterObservation {
	o := ClusterObservation{
		Clustered: true, ObservedAt: s.observedAt, ProbeTimeout: s.probeTimeout,
		LeaderID: s.leaderID, Voters: s.voters,
		FaultTolerance: s.faultToleranceVerdict(),
		Forming:        s.forming(s.observedAt),
		Shortfall:      s.shortfall(s.observedAt),
	}
	peers := make(map[uint64]statusPeer, len(s.peers))
	for _, peer := range s.peers {
		peers[peer.id] = peer
	}
	for _, member := range s.members {
		m := ClusterObservedMember{
			ID: member.id, Address: member.address, Self: member.id == s.id,
			Role: member.role, Live: member.source == "live raft",
			Database: "not observed", Listener: "not observed",
		}
		if m.Self {
			m.Database, m.Listener = localListener(s.database), localListener(strings.TrimPrefix(s.enrollment, s.enrollmentListen+": "))
		} else if peer, ok := peers[member.id]; ok {
			m.Database, m.Listener = peer.database, peer.enrollment
		}
		o.Members = append(o.Members, m)
	}
	sort.SliceStable(o.Members, func(i, j int) bool { return o.Members[i].Address < o.Members[j].Address })
	return o
}

// localListener turns the report's wording for this member's own listeners
// into the wording it uses for a peer's.
func localListener(state string) string {
	switch state {
	case "listening":
		return "open"
	case "not listening":
		return "connection refused"
	case "timed out", "unreachable":
		return state
	default:
		return "not observed"
	}
}
