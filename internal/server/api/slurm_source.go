package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// providerHPCClusters lists a cloud account's HPC clusters with the Slurm
// entry already made from each: the sibling of the account's managed
// Kubernetes clusters (GET /providers/{id}/clusters). It is a separate
// listing because the two are separate vendor products with separate
// permissions and shapes, so one failing leaves the other readable. E-HPC is
// asked region by region, which is slow on cold connections, so the page gets
// the last listing at once with when it was read, and refreshing while a
// newer one is read behind it (provider.Service.ShownHPCClusters); fresh=1
// waits for a new read, as after a sync.
func (a *apiHandlers) providerHPCClusters(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	list, listedAt, refreshing, err := a.d.Providers.ShownHPCClusters(r.Context(), id, r.URL.Query().Get("fresh") == "1")
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, provider.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err)
		return
	case err != nil:
		writeErr(w, statusProviderFailed, err)
		return
	}
	entries, err := a.d.Store.SlurmClustersFromProvider(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// One read of the account's instances and this one listing answer every
	// cluster's can_apply; a cluster is not asked about the cloud again.
	targets, err := a.d.Providers.SlurmApplyTargets(id, func() (provider.HPCClusterList, error) { return list, nil })
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.ProviderHPCClusterList{Items: make([]dto.ProviderHPCCluster, 0, len(list.Clusters)), Access: "ok",
		Partial: list.Partial(), FailedRegions: make([]dto.ProviderHPCFailedRegion, 0, len(list.Failed)), Refreshing: refreshing}
	if !listedAt.IsZero() {
		at := listedAt.UTC()
		out.ListedAt = &at
	}
	switch list.Access {
	case provider.HPCAccessPermissionMissing, provider.HPCAccessNotActivated:
		out.Access, out.Permission, out.Code = list.Access, list.Permission, list.Code
	}
	for _, f := range list.Failed {
		out.FailedRegions = append(out.FailedRegions, dto.ProviderHPCFailedRegion{Region: f.Region, Code: f.Code, Message: f.Message, Permission: f.Permission})
	}
	for _, c := range list.Clusters {
		state := provider.HPCStateCode(c.State)
		pc := dto.ProviderHPCCluster{ExternalID: c.ID, Name: c.Name, Region: c.Region, Scheduler: c.Scheduler, SchedulerVersion: c.SchedulerVersion,
			State: state, StateTone: provider.HPCStateTone(state), VendorState: c.State, Running: c.Running}
		// The live listing's count is current; the synced one is as old as the
		// account's last instance sync, so it only answers when the listing
		// carries no count.
		if c.ComputeNodes != nil {
			n := *c.ComputeNodes
			pc.ComputeNodes = &n
		} else if n, ok := targets.GPUNodes(c.ID); ok {
			pc.GPUNodes = &n
		}
		if pc.Name == "" {
			pc.Name = c.ID
		}
		e, hasEntry := entries[c.ID]
		if hasEntry {
			pc.Entry = &dto.SlurmClusterRef{UUID: e.UUID, Name: e.Name}
		}
		pc.Connectable = !hasEntry && c.Running && provider.HPCConnectable(c.Scheduler)
		if !hasEntry {
			// A cluster still being created may not name its scheduler yet:
			// that is its state's to say, not a scheduler that is not Slurm.
			if !provider.HPCConnectable(c.Scheduler) && (c.Scheduler != "" || c.Running) {
				pc.UnsupportedReason = "scheduler_not_slurm"
			} else if !c.Running {
				pc.NotConnectableReason = provider.HPCNotConnectableReason(state)
			}
		}
		if pc.Connectable {
			_, err := targets.For(c.ID)
			pc.CanApply = err == nil
		}
		out.Items = append(out.Items, pc)
	}
	out.Total = len(out.Items)
	writeJSON(w, http.StatusOK, out)
}
