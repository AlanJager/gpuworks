package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
)

// railDTOs describes a network's rails on a node from its inventory.
func railDTOs(inv *proto.NetInventory, networkType string, hcas []string) []dto.NetworkRail {
	out := []dto.NetworkRail{}
	for _, r := range netplan.Rails(inv, networkType, hcas) {
		out = append(out, railDTO(r))
	}
	return out
}

func railDTO(r netplan.Rail) dto.NetworkRail {
	return dto.NetworkRail{Name: r.Name, IP: r.IP(), NetDev: r.NetDev, LinkLayer: r.LinkLayer, State: r.State, SpeedGbps: r.SpeedGbps, Gap: r.Gap()}
}

// endpointSources says whether the rails and bootstrap of a network that uses
// rails are inferred or pinned on a node; both empty for other networks.
func endpointSources(r netplan.Resolved) (hcas, bootstrap string) {
	if !netplan.UsesRails(r.Network.Type) {
		return "", ""
	}
	return pinSource(r.HCAsPinned), pinSource(r.BootstrapPinned)
}

func pinSource(pinned bool) string {
	if pinned {
		return "manual"
	}
	return "auto"
}

// attachNetworkBatch attaches a network to several nodes; each node succeeds
// or fails on its own.
func (a *apiHandlers) attachNetworkBatch(w http.ResponseWriter, r *http.Request) {
	var req dto.NetworkAttachBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	networkID := parseUint(chi.URLParam(r, "id"))
	if len(req.NodeUUIDs) == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorNetworkAttachmentTargetInvalid, "node_uuids is required", nil)
		return
	}
	results, err := a.networks().AttachNodes(networkID, req.NodeUUIDs)
	if err != nil {
		writeNetworkErr(w, err)
		return
	}
	out := dto.NetworkAttachBatchResult{Results: []dto.NetworkAttachBatchItem{}}
	for _, res := range results {
		item := dto.NetworkAttachBatchItem{NodeUUID: res.NodeUUID, Status: res.Status}
		if res.Err != nil {
			item.Code, item.Params, item.Error = ErrorNetworkAttachmentTargetInvalid, map[string]any{"node": res.NodeUUID}, res.Err.Error()
		}
		out.Results = append(out.Results, item)
	}
	a.syncNodesIfOnline()
	st, err := a.loadNetworkState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, n := range st.Networks {
		if n.ID == networkID {
			out.Network = st.networkDetail(n, a.latestNetworkTest(n.ID))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// editNetworkHCAs adds, removes or resets the rails nodes use on an RDMA
// network.
func (a *apiHandlers) editNetworkHCAs(w http.ResponseWriter, r *http.Request) {
	var req dto.NetworkHCARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	networkID := parseUint(chi.URLParam(r, "id"))
	err := a.networks().EditHCAs(networkID, network.HCAEdit{Op: req.Op, HCA: req.HCA, NodeUUID: req.NodeUUID})
	var last *network.LastRailError
	switch {
	case err == nil:
		a.writeNetworkDetail(w, http.StatusOK, networkID)
	case errors.As(err, &last):
		writeProductError(w, http.StatusConflict, ErrorNetworkHCALast, last.Error(), map[string]any{"node": last.Node})
	case errors.Is(err, network.ErrNotRDMA):
		writeProductError(w, http.StatusBadRequest, ErrorNetworkHCANotRDMA, err.Error(), nil)
	case errors.Is(err, network.ErrNodeNotOnNetwork):
		writeProductError(w, http.StatusBadRequest, ErrorNetworkHCANodeInvalid, err.Error(), map[string]any{"node": req.NodeUUID})
	case errors.Is(err, network.ErrHCANotOnNode), errors.Is(err, network.ErrHCANotUsed):
		writeProductError(w, http.StatusBadRequest, ErrorNetworkHCAInvalid, err.Error(), map[string]any{"hca": req.HCA})
	default:
		writeNetworkErr(w, err)
	}
}
