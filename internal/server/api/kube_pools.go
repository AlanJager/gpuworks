package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An entry made from a cloud cluster holds a set of that cluster's GPU node
// pools, all of one vendor for now: the DaemonSet runs on the nodes of those
// pools and nowhere else (docs/adr/0062-apply-to-cloud-clusters.md).

// kubeEntryView is what the DTO of one entry needs beyond its row.
type kubeEntryView struct {
	nodes      int
	admin      bool
	source     *dto.KubeClusterSource
	privateAPI bool
	poolIDs    []string
}

// kubeEntryViews gathers the views of the given entries in one pass: node
// counts, sources, the pools they hold and the cloud's word on private API
// servers.
func (a *apiHandlers) kubeEntryViews(ctx context.Context, rows []store.KubeCluster, admin bool) (map[uint]kubeEntryView, error) {
	counts, err := a.d.Store.KubeClusterNodeCounts()
	if err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(rows))
	for _, c := range rows {
		ids = append(ids, c.ID)
	}
	pools, err := a.d.Store.KubeClusterPools(ids...)
	if err != nil {
		return nil, err
	}
	sources := a.kubeClusterSources(rows)
	private := a.kubePrivateAPIClusters(ctx, rows)
	out := make(map[uint]kubeEntryView, len(rows))
	for _, c := range rows {
		out[c.ID] = kubeEntryView{nodes: counts[c.ID], admin: admin, source: sources[c.ID], privateAPI: private[c.ID], poolIDs: kubePoolIDs(pools[c.ID])}
	}
	return out, nil
}

func kubePoolIDs(pools []store.KubeClusterPool) []string {
	out := make([]string, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.PoolID)
	}
	return out
}

// kubeClusterParams are the manifest parameters of a stored entry, with the
// node pools it holds.
func (a *apiHandlers) kubeClusterParams(c store.KubeCluster) (kubemanifest.Params, error) {
	pools, err := a.d.Store.KubeClusterPools(c.ID)
	if err != nil {
		return kubemanifest.Params{}, err
	}
	return kubeManifestParams(c, pools[c.ID])
}

// kubePoolSelection resolves the node pools a request names against the cloud
// account's live listing, answering the request itself and returning false
// when they cannot be held: none, unknown to the cluster, or of other vendors
// than the entry's. entryVendor is the vendor of an entry that exists (its
// pools may not change it); empty for a new entry, whose vendor is then the
// pools'.
func (a *apiHandlers) kubePoolSelection(w http.ResponseWriter, r *http.Request, providerID uint, clusterID string, ids []string, entryVendor string) (pools []store.KubeClusterPool, vendor string, ok bool) {
	wanted := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			wanted = append(wanted, id)
		}
	}
	if len(wanted) == 0 {
		kubeInvalid(w, "provider_pool_ids", "select at least one node pool of the cluster")
		return nil, "", false
	}
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return nil, "", false
	}
	clusters, err := a.d.Providers.Clusters(r.Context(), providerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		kubeInvalid(w, "provider_id", "no such cloud account")
		return nil, "", false
	case errors.Is(err, provider.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err)
		return nil, "", false
	case err != nil:
		writeErr(w, statusProviderFailed, err)
		return nil, "", false
	}
	var found *provider.Cluster
	for i := range clusters {
		if clusters[i].ExternalID == clusterID {
			found = &clusters[i]
		}
	}
	if found == nil {
		kubeInvalid(w, "provider_cluster_id", "the cloud account has no such cluster")
		return nil, "", false
	}
	byID := map[string]provider.ClusterPool{}
	for _, p := range found.GPUPools {
		byID[p.ID] = p
	}
	vendors := map[string]bool{}
	for _, id := range wanted {
		p, known := byID[id]
		if !known {
			kubeInvalid(w, "provider_pool_ids", "node pool "+id+" is not a GPU node pool of the cluster")
			return nil, "", false
		}
		if p.LabelKey == "" {
			kubeInvalid(w, "provider_pool_ids", "the cloud gave no label for node pool "+id)
			return nil, "", false
		}
		vendors[p.Vendor] = true
		pools = append(pools, store.KubeClusterPool{PoolID: p.ID, LabelKey: p.LabelKey})
	}
	if entryVendor != "" {
		vendors[entryVendor] = true
	}
	if len(vendors) > 1 {
		names := make([]string, 0, len(vendors))
		for v := range vendors {
			names = append(names, v)
		}
		sort.Strings(names)
		writeProductError(w, http.StatusBadRequest, ErrorKubeClusterMixedVendors,
			"the node pools of one entry must all be of one vendor, got "+strings.Join(names, ", "), map[string]any{"vendors": strings.Join(names, ", ")})
		return nil, "", false
	}
	for v := range vendors {
		vendor = v
	}
	return pools, vendor, true
}

// setKubeClusterPools replaces the node pools an entry made from a cloud
// cluster holds. The cluster itself changes only when the manifest is applied
// again (POST …/apply, or kubectl).
func (a *apiHandlers) setKubeClusterPools(w http.ResponseWriter, r *http.Request) {
	var req dto.KubeClusterPoolsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	c, err := a.d.Store.KubeClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if c.ProviderID == nil {
		writeProductError(w, http.StatusConflict, ErrorKubeClusterNoSource, "the cluster was not connected from a cloud account; its nodes are chosen by its node selector", nil)
		return
	}
	pools, _, ok := a.kubePoolSelection(w, r, *c.ProviderID, c.ProviderClusterID, req.ProviderPoolIDs, c.Vendor)
	if !ok {
		return
	}
	updated, err := a.d.Store.SetKubeClusterPools(c.UUID, pools)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if a.d.Log != nil {
		a.d.Log.Info("kubernetes cluster pools changed", "cluster", updated.Name, "pools", len(pools))
	}
	views, err := a.kubeEntryViews(r.Context(), []store.KubeCluster{*updated}, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, kubeClusterDTO(*updated, views[updated.ID]))
}
