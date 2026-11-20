package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/slurmsweep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Slurm nodes that left their cluster are swept by slurmsweep; these adapt
// the store and the cloud account to what it asks.

// slurmSweepStore is the store as the Slurm node sweep asks for it.
type slurmSweepStore struct{ *store.Store }

func (s slurmSweepStore) OfflineNodes(cutoff time.Time) ([]slurmsweep.Node, error) {
	nodes, err := s.OfflineSlurmNodes(cutoff)
	out := make([]slurmsweep.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, slurmsweep.Node{ID: n.ID, UUID: n.UUID, ClusterID: n.SlurmClusterID, SlurmNode: n.SlurmNode})
	}
	return out, err
}

func (s slurmSweepStore) OfflineCloudNodes(cutoff time.Time) ([]slurmsweep.Node, error) {
	nodes, err := s.OfflineCloudSlurmNodes(cutoff)
	out := make([]slurmsweep.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, slurmsweep.Node{ID: n.ID, UUID: n.UUID, ClusterID: n.SlurmClusterID, SlurmNode: n.SlurmNode})
	}
	return out, err
}

// slurmSweepListingAge is how old the account's listing of its clusters may
// be when the sweep finds a cluster's region in it: regions do not move.
const slurmSweepListingAge = time.Hour

// slurmSweepCloudWait bounds one cloud listing of a cluster's nodes.
const slurmSweepCloudWait = 30 * time.Second

// slurmSweepCloud asks the cloud account an entry was made from for its
// cluster's compute nodes.
type slurmSweepCloud struct {
	store     *store.Store
	providers *provider.Service
}

func (c slurmSweepCloud) NodeNames(ctx context.Context, clusterID uint) ([]string, error) {
	entry, err := c.store.SlurmClusterByID(clusterID)
	if err != nil {
		return nil, err
	}
	if entry.ProviderID == nil || entry.ProviderClusterID == "" {
		return nil, errors.New("the entry was not made from a cloud cluster")
	}
	ctx, cancel := context.WithTimeout(ctx, slurmSweepCloudWait)
	defer cancel()
	return c.providers.SlurmClusterNodeNames(ctx, *entry.ProviderID, entry.ProviderClusterID, slurmSweepListingAge)
}

// newSlurmSweep is the sweep of the Slurm nodes that left their cluster.
func newSlurmSweep(log *slog.Logger, st *store.Store, providers *provider.Service, kick func(string)) *slurmsweep.Sweeper {
	return slurmsweep.New(log, slurmSweepStore{st}, slurmSweepCloud{store: st, providers: providers}, kick)
}

func (s slurmSweepStore) HasInstances(nodeID uint) (bool, error) {
	instances, err := s.InstancesForNode(nodeID)
	return len(instances) > 0, err
}
