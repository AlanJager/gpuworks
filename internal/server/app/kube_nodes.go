package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/slurmsweep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Kubernetes nodes come and go with their cluster: a node scaled away or
// replaced, or one its entry no longer selects (a changed node selector or
// set of node pools), leaves its row offline here for good. The sweep loop
// asks another online agent of the same entry whether such a node still
// exists there and runs one of the entry's agents, and removes the ones that
// do not and run nothing. The server holds no cluster credentials, so the
// question goes through an agent.

const (
	kubeNodeSweepInterval = 5 * time.Minute
	// kubeNodeGoneAfter is how long a node is offline before it is asked
	// about: long enough for an agent pod restart or a node reboot.
	kubeNodeGoneAfter = 15 * time.Minute
)

// clusterNodeLoop runs the sweeps of the nodes that left a cluster: the
// Kubernetes one above, and the Slurm one (package slurmsweep), each with its
// own memory of the nodes it found gone.
func (a *App) clusterNodeLoop(ctx context.Context) {
	t := time.NewTicker(kubeNodeSweepInterval)
	defer t.Stop()
	strikes := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-kubeNodeGoneAfter)
			sweepGoneKubeNodes(ctx, a.log, a.store, a.hub.CallNode, a.hub.Kick, cutoff, strikes)
			a.slurmSweep.Sweep(ctx, cutoff)
		}
	}
}

// nodeCaller is the hub's RPC to a node's agent.
type nodeCaller func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error)

// sweepGoneKubeNodes removes a node only when two sweeps in a row find it
// gone: one answer can catch an agent pod between the deletion and the
// re-creation of a rolling update. strikes holds the nodes the last sweep
// found gone.
func sweepGoneKubeNodes(ctx context.Context, log *slog.Logger, st *store.Store, call nodeCaller, kick func(string), cutoff time.Time, strikes map[string]bool) {
	nodes, err := st.OfflineKubeNodes(cutoff)
	if err != nil {
		log.Warn("kubernetes node sweep failed", "err", err)
		return
	}
	found := map[string]bool{}
	defer func() {
		for uuid := range strikes {
			if !found[uuid] {
				delete(strikes, uuid)
			}
		}
	}()
	for _, node := range nodes {
		if instances, err := st.InstancesForNode(node.ID); err != nil || len(instances) > 0 {
			continue // its instances are the controller's to settle first
		}
		peers, err := st.OnlineKubePeers(node.KubeClusterID, node.ID)
		if err != nil || len(peers) == 0 {
			continue
		}
		var answer proto.KubeNodeExistsResult
		asked := false
		for _, peer := range peers {
			callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			raw, err := call(callCtx, peer.UUID, proto.RPCKubeNodeExists, proto.KubeNodeExistsParams{NodeName: node.KubeNodeName})
			cancel()
			if err == nil && json.Unmarshal(raw, &answer) == nil {
				asked = true
				break
			}
		}
		left := !answer.Exists || (answer.Agent != nil && !*answer.Agent)
		if !asked || !left {
			continue
		}
		found[node.UUID] = true
		if !strikes[node.UUID] {
			strikes[node.UUID] = true
			continue
		}
		delete(strikes, node.UUID)
		if err := st.DeleteNode(node.UUID); err != nil {
			log.Warn("removing a node that left its Kubernetes cluster failed", "node", node.UUID, "err", err)
			continue
		}
		kick(node.UUID)
		log.Info("removed a node that left its Kubernetes cluster", "node", node.UUID, "kube_node", node.KubeNodeName)
	}
}

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
