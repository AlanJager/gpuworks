package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/slurmcluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An enrolled agent reports its cluster's nodes every report interval
// (slurmcluster.Memory.Ingest decides what is stored), and the answer hands it
// the entry's current settings, which is how an edit reaches a running agent.

// Bounds of a report.
const (
	slurmReportBodyMax = 4 << 20
	slurmReportNodeMax = 20000
)

// slurmNodeByBearer is the enrolled node whose node token the request carries.
func (a *apiHandlers) slurmNodeByBearer(r *http.Request) (*store.Node, bool, error) {
	token := auth.BearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return nil, false, nil
	}
	node, err := a.d.Store.NodeByTokenHash(store.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return node, node.SlurmClusterID != 0, nil
}

// slurmReportGPUNodes takes an agent's list of its cluster's GPU nodes.
func (a *apiHandlers) slurmReportGPUNodes(w http.ResponseWriter, r *http.Request) {
	node, ok, err := a.slurmNodeByBearer(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeProductError(w, http.StatusUnauthorized, ErrorAuthenticationRequired, "a Slurm node's token is required", nil)
		return
	}
	var req dto.SlurmGPUNodeReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, slurmReportBodyMax)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.Digest, req.StateDigest = strings.TrimSpace(req.Digest), strings.TrimSpace(req.StateDigest)
	if req.Digest == "" || len(req.Digest) > 128 || len(req.StateDigest) > 128 || len(req.Nodes) > slurmReportNodeMax {
		writeFieldInvalid(w, "digest", "a report needs digests of at most 128 bytes and at most 20000 nodes")
		return
	}
	cluster, err := a.d.Store.SlurmClusterByID(node.SlurmClusterID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusUnauthorized, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.SlurmGPUNodeReportResponse{Settings: slurmAgentSettings(*cluster)}
	need, err := a.slurm.Ingest(a.d.Store, cluster.ID, req)
	var bad *slurmcluster.BadNodesError
	switch {
	case errors.As(err, &bad):
		writeFieldInvalid(w, "nodes", bad.Reason)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out.NeedNodes = need
	writeJSON(w, http.StatusOK, out)
}
