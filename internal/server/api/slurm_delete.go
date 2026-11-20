package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// deleteSlurmCluster answers DELETE /slurm/clusters/{uuid}. An entry with
// nodes is kept (SLURM_CLUSTER_HAS_NODES): the nodes' agents belong to the
// cluster's health check, and deleting the nodes of a cluster that still runs
// it only brings them back. After the agents were uninstalled
// (docs/how-to/connect-slurm-nodes.md) the nodes are offline for good, and
// with_offline_nodes=true deletes the entry together with them, provided none
// is online and none has instances (SLURM_CLUSTER_NODES_ONLINE,
// SLURM_CLUSTER_NODES_HAVE_INSTANCES); the entry's DTO says beforehand.
func (a *apiHandlers) deleteSlurmCluster(w http.ResponseWriter, r *http.Request) {
	withNodes := false
	if values, present := r.URL.Query()["with_offline_nodes"]; present {
		var err error
		if len(values) != 1 {
			slurmInvalid(w, "with_offline_nodes", "must be a single boolean")
			return
		}
		if withNodes, err = strconv.ParseBool(values[0]); err != nil {
			slurmInvalid(w, "with_offline_nodes", "must be a single boolean")
			return
		}
	}
	uuid := chi.URLParam(r, "uuid")
	// What the server remembered of the entry's list goes with it.
	gone, lookup := a.d.Store.SlurmClusterByUUID(uuid)
	if errors.Is(lookup, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, lookup)
		return
	}
	if lookup != nil {
		writeErr(w, http.StatusInternalServerError, lookup)
		return
	}
	if withNodes && !a.deleteSlurmNodes(w, *gone) {
		return
	}
	switch err := a.d.Store.DeleteSlurmCluster(uuid); {
	case err == nil:
		a.slurm.Forget(gone.ID)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrSlurmClusterHasNodes):
		writeProductError(w, http.StatusConflict, ErrorSlurmClusterHasNodes, "remove the cluster's nodes first", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// deleteSlurmNodes deletes the nodes of the entry c, answering the request
// itself and returning false when some cannot go: an agent is online, or a
// node has instances. Nothing is deleted then. Otherwise the entry is revoked
// first, then the nodes are deleted.
func (a *apiHandlers) deleteSlurmNodes(w http.ResponseWriter, c store.SlurmCluster) bool {
	blockers, err := a.d.Store.SlurmClusterDeleteBlockers(c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	switch b := blockers[c.ID]; {
	case b.Online > 0:
		writeProductError(w, http.StatusConflict, messages.ErrorSlurmClusterNodesOnline,
			"agents of the cluster are still connected: uninstall them first", map[string]any{"count": b.Online})
		return false
	case b.Instances > 0:
		writeProductError(w, http.StatusConflict, messages.ErrorSlurmClusterNodesHaveInstances,
			"some nodes of the cluster have instances: stop their deployments first", map[string]any{"count": b.Instances})
		return false
	}
	// Revoke before anything is deleted: a node deleted while the token still
	// works is enrolled again by the next health check run, and the entry
	// then has nodes it cannot be deleted with. The entry stays revoked if a
	// deletion below fails; revoked entries only stop taking new nodes.
	if err := a.d.Store.RevokeSlurmCluster(c.UUID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return false
	}
	nodes, err := a.d.Store.SlurmClusterNodes(c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	for _, n := range nodes {
		if err := a.d.Store.DeleteNode(n.UUID); err != nil && !errors.Is(err, store.ErrNotFound) {
			a.writeNodeDeleteError(w, err)
			return false
		}
		if a.d.CardHealth != nil {
			a.d.CardHealth.ForgetNode(n.UUID, n.ID)
		}
		if a.d.KickNode != nil {
			a.d.KickNode(n.UUID)
		}
	}
	if a.d.Log != nil {
		a.d.Log.Info("slurm cluster nodes deleted with their entry", "cluster", c.Name, "nodes", len(nodes))
	}
	return true
}
