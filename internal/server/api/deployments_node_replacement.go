package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// Bounds of a deployment's node replacement wait, in seconds; 0 never moves
// its replicas.
const (
	nodeReplaceMinSec = 60
	nodeReplaceMaxSec = 7 * 24 * 3600
)

// setNodeReplacement sets how long a node running one of the deployment's
// replicas may stay offline before the replica moves to another node.
func (a *apiHandlers) setNodeReplacement(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.NodeReplacement
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.AfterSec != 0 && (req.AfterSec < nodeReplaceMinSec || req.AfterSec > nodeReplaceMaxSec) {
		writeProductError(w, http.StatusBadRequest, ErrorNodeReplacementInvalid, fmt.Sprintf("after_sec is 0 (never) or %d to %d", nodeReplaceMinSec, nodeReplaceMaxSec), map[string]any{"minimum": nodeReplaceMinSec, "maximum": nodeReplaceMaxSec})
		return
	}
	if err := a.d.Store.SetDeploymentNodeReplaceAfter(dep.ID, req.AfterSec); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}
