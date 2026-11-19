// Package slurmsweep removes the nodes of Slurm cluster entries that left
// their cluster. The server's app package only runs it on a timer.
package slurmsweep

import (
	"context"
	"log/slog"
	"time"
)

// A Slurm node leaves its cluster when the cluster scales in: its agent goes
// with the machine and the node's row stays offline for good. The enrolled
// agents of an entry report the cluster's GPU nodes (sinfo), so a node that
// has been offline for a while and is no longer among them has left; an entry
// that has reported no GPU node condemns nothing (store.OfflineSlurmNodes).
// That list cannot tell when the node that left was the last one with an
// agent: nothing reports a newer list. An entry made from a cloud account's
// cluster has a second source, the cloud's own list of the cluster's compute
// nodes, so its offline nodes are also asked about there (Cloud), and one the
// cloud no longer has has left.
//
// Like the Kubernetes sweep, a node is removed only when two sweeps in a row
// find it gone, and only when it runs nothing: its instances are the
// controller's to settle first.

// Node is a node of a Slurm entry that has been offline for a while.
type Node struct {
	ID        uint
	UUID      string
	ClusterID uint   // its entry's
	SlurmNode string // its name in Slurm
}

// Store is what the sweep reads and removes. The package asks for these
// things in its own terms so that it does not depend on the store's tables;
// the app adapts the store to it.
type Store interface {
	// OfflineNodes are the offline nodes of Slurm entries that were last
	// seen before cutoff and that their cluster no longer lists.
	OfflineNodes(cutoff time.Time) ([]Node, error)
	// OfflineCloudNodes are the offline nodes of entries made from a cloud
	// account's cluster that were last seen before cutoff, listed or not.
	OfflineCloudNodes(cutoff time.Time) ([]Node, error)
	// HasInstances: instances still belong to the node.
	HasInstances(nodeID uint) (bool, error)
	// DeleteNode removes the node, and takes it off its entry's list.
	DeleteNode(uuid string) error
}

// Cloud lists the compute nodes the cloud has in the cluster an entry was
// made from. An error means the list is not known whole; then none of the
// entry's nodes is found gone by it.
type Cloud interface {
	NodeNames(ctx context.Context, clusterID uint) ([]string, error)
}

// Sweeper removes the nodes of Slurm entries that left their cluster. It
// remembers the nodes its last sweep found gone.
type Sweeper struct {
	log     *slog.Logger
	store   Store
	cloud   Cloud // nil asks no cloud
	kick    func(nodeUUID string)
	strikes map[string]bool
}

// New is a Sweeper that asks cloud about the nodes of entries made from a
// cloud cluster (nil asks none) and disconnects a removed node's agent with
// kick.
func New(log *slog.Logger, st Store, cloud Cloud, kick func(nodeUUID string)) *Sweeper {
	return &Sweeper{log: log, store: st, cloud: cloud, kick: kick, strikes: map[string]bool{}}
}

// Sweep removes the nodes that have been offline since before cutoff, are no
// longer among their cluster's GPU nodes or no longer in the cloud's cluster,
// and were found gone by the sweep before too. It is called from one
// goroutine.
func (s *Sweeper) Sweep(ctx context.Context, cutoff time.Time) {
	nodes, err := s.store.OfflineNodes(cutoff)
	if err != nil {
		s.log.Warn("slurm node sweep failed", "err", err)
		return
	}
	nodes = append(nodes, s.goneFromCloud(ctx, cutoff, nodes)...)
	found := map[string]bool{}
	defer func() {
		for uuid := range s.strikes {
			if !found[uuid] {
				delete(s.strikes, uuid)
			}
		}
	}()
	for _, node := range nodes {
		if has, err := s.store.HasInstances(node.ID); err != nil || has {
			continue
		}
		found[node.UUID] = true
		if !s.strikes[node.UUID] {
			s.strikes[node.UUID] = true
			continue
		}
		delete(s.strikes, node.UUID)
		if err := s.store.DeleteNode(node.UUID); err != nil {
			s.log.Warn("removing a node that left its Slurm cluster failed", "node", node.UUID, "err", err)
			continue
		}
		s.kick(node.UUID)
		s.log.Info("removed a node that left its Slurm cluster", "node", node.UUID, "slurm_node", node.SlurmNode)
	}
}

// goneFromCloud are the offline nodes of cloud entries the cloud no longer
// has, leaving out those already found gone from their list. The cloud is
// asked once per entry that has such a node, and only then.
func (s *Sweeper) goneFromCloud(ctx context.Context, cutoff time.Time, already []Node) []Node {
	if s.cloud == nil {
		return nil
	}
	nodes, err := s.store.OfflineCloudNodes(cutoff)
	if err != nil {
		s.log.Warn("slurm node sweep could not read the nodes of cloud clusters", "err", err)
		return nil
	}
	seen := make(map[string]bool, len(already))
	for _, n := range already {
		seen[n.UUID] = true
	}
	lists := map[uint]map[string]bool{} // nil: the cloud's list is not known
	var out []Node
	for _, n := range nodes {
		if seen[n.UUID] || n.SlurmNode == "" {
			continue
		}
		names, asked := lists[n.ClusterID]
		if !asked {
			listed, err := s.cloud.NodeNames(ctx, n.ClusterID)
			if err != nil {
				s.log.Warn("slurm node sweep could not list the cloud cluster's nodes", "cluster", n.ClusterID, "err", err)
			} else {
				names = make(map[string]bool, len(listed))
				for _, name := range listed {
					names[name] = true
				}
			}
			lists[n.ClusterID] = names
		}
		if names != nil && !names[n.SlurmNode] {
			out = append(out, n)
		}
	}
	return out
}
