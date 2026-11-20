package provider

import (
	"context"
	"fmt"
	"sort"
)

// HPCClusterListing is an HPC cluster of a cloud account as the vendor lists
// it (Alibaba Cloud E-HPC), whether or not any of its nodes was synced as an
// instance. Like a managed Kubernetes cluster (Cluster) it is never stored:
// only the Slurm entry made from it is (store.SlurmCluster.ProviderClusterID).
type HPCClusterListing struct {
	HPCCluster
	Region string `json:"region"`
	// State is the vendor's own word for the cluster's state ("running",
	// "creating"...).
	State string `json:"state"`
	// Running: the vendor reports the cluster usable.
	Running bool `json:"running"`
	// ComputeNodes is how many compute nodes the vendor says the cluster has
	// in any state (creating, running, abnormal); nil when the listing does
	// not say, which is not zero.
	ComputeNodes *int `json:"-"`
}

// HPCClusterList is what listing an account's HPC clusters found. Clusters
// are the ones the vendor listed. An account the vendor does not let list
// them has none to show and says why in Access, so that is a quiet notice and
// not a failure; a listing that worked in some regions only is Partial, with
// the regions that did not in Failed.
type HPCClusterList struct {
	Clusters []HPCClusterListing
	// Access is "" when the vendor answered, HPCAccessPermissionMissing when
	// the key may not list clusters (Permission is the action), or
	// HPCAccessNotActivated when the account never activated the product
	// (Code is the vendor's word for it). Clusters is empty for the last two.
	Access     string
	Permission string
	Code       string
	// Failed are the regions that could not be listed while others could.
	Failed []HPCRegionFailure
}

// The Access values of an HPCClusterList.
const (
	HPCAccessPermissionMissing = "permission_missing"
	HPCAccessNotActivated      = "not_activated"
)

// Partial: some region could not be listed, so a cluster of it may be missing.
func (l HPCClusterList) Partial() bool { return len(l.Failed) > 0 }

// HPCRegionFailure is a region whose clusters could not be listed. Code is the
// vendor's error code ("" for a network failure) and Message what it said;
// Permission is set when the cause is a RAM action the key lacks.
type HPCRegionFailure struct {
	Region     string
	Code       string
	Message    string
	Permission string
}

// HPCClusterProvider is implemented by drivers whose accounts hold HPC
// clusters (a Slurm cluster entry can be made from one: ResourceSlurm).
type HPCClusterProvider interface {
	// ListHPCClusters returns the account's HPC clusters in the regions in
	// scope. A region that cannot be listed does not fail the others: see
	// HPCClusterList. An error means nothing could be listed.
	ListHPCClusters(ctx context.Context, creds Credentials) (HPCClusterList, error)
}

// HPCSharedStorage is an HPC cluster provider whose clusters share a
// filesystem across all their nodes at a path it knows: the directory where
// one copy of a model serves every node of the cluster.
type HPCSharedStorage interface {
	HPCSharedModelDir() string
}

// HPCSharedModelDir is the directory the clusters of the provider kind share
// across their nodes for models, "" when the kind does not say.
func (s *Service) HPCSharedModelDir(kind string) string {
	drv, ok := s.driver(kind)
	if !ok {
		return ""
	}
	if shared, ok := drv.(HPCSharedStorage); ok {
		return shared.HPCSharedModelDir()
	}
	return ""
}

func isHPCClusterProvider(drv Driver) bool {
	_, ok := drv.(HPCClusterProvider)
	return ok
}

// ErrNoHPCClusters: the provider kind has no HPC clusters.
var ErrNoHPCClusters = fmt.Errorf("%w: this provider has no HPC clusters", ErrUnsupported)

// HPCClusters lists a provider account's HPC clusters, live, in a stable
// order (region, then ID).
func (s *Service) HPCClusters(ctx context.Context, providerID uint) (HPCClusterList, error) {
	_, drv, creds, err := s.driverCreds(providerID)
	if err != nil {
		return HPCClusterList{}, err
	}
	hp, ok := drv.(HPCClusterProvider)
	if !ok {
		return HPCClusterList{}, ErrNoHPCClusters
	}
	list, err := hp.ListHPCClusters(ctx, creds)
	if err != nil {
		return HPCClusterList{}, err
	}
	sort.SliceStable(list.Clusters, func(i, j int) bool {
		if list.Clusters[i].Region != list.Clusters[j].Region {
			return list.Clusters[i].Region < list.Clusters[j].Region
		}
		return list.Clusters[i].ID < list.Clusters[j].ID
	})
	sort.SliceStable(list.Failed, func(i, j int) bool { return list.Failed[i].Region < list.Failed[j].Region })
	return list, nil
}

// hpcListingOf finds one cluster in a listing.
func hpcListingOf(list []HPCClusterListing, clusterID string) (HPCClusterListing, bool) {
	for _, c := range list {
		if c.ID == clusterID {
			return c, true
		}
	}
	return HPCClusterListing{}, false
}
