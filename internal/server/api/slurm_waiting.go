package api

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/slurmcluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Why an entry has no node yet is slurmcluster.Waiting; the cloud's count of
// the cluster's compute nodes, which it reads first, comes from here.

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
func slurmWaiting(w slurmcluster.Waiting) *dto.SlurmWaiting {
	if w.Reason == "" {
		return nil
	}
	out := &dto.SlurmWaiting{Reason: w.Reason}
	for _, h := range w.Hosts {
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
