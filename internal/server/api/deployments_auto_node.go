package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// automaticPickAttempts bounds how often a create that named no node picks
// again when the node it picked was taken before the create re-checked it.
const automaticPickAttempts = 3

// createDeployment creates a deployment. An automatic single-node create
// that names no node leaves the pick to the server, so when that pick is
// taken between the evaluation and the create's own re-check (another
// create placing replicas on the same cards a moment earlier), the server
// picks again instead of answering AUTOMATIC_PLACEMENT_STALE: there is no
// client choice to report as stale. A create that names its node keeps the
// 409.
func (a *apiHandlers) createDeployment(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSONBodyError(w, err)
		return
	}
	var probe dto.DeployRequest
	if json.Unmarshal(body, &probe) != nil || !leavesNodeToServer(&probe) {
		r.Body = io.NopCloser(bytes.NewReader(body))
		a.createDeploymentOnce(w, r)
		return
	}
	var out *capturedResponse
	for attempt := 1; ; attempt++ {
		attemptReq := r.Clone(r.Context())
		attemptReq.Body = io.NopCloser(bytes.NewReader(body))
		out = &capturedResponse{header: http.Header{}, status: http.StatusOK}
		a.createDeploymentOnce(out, attemptReq)
		if attempt >= automaticPickAttempts || !pickWentStale(out) {
			break
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		}
		if r.Context().Err() != nil {
			break
		}
	}
	for key, values := range out.header {
		w.Header()[key] = values
	}
	w.WriteHeader(out.status)
	_, _ = w.Write(out.body.Bytes())
}

// afterAutomaticPick, when set, runs right after the server picked a node
// for a create and before the create re-checks it: tests use it to take the
// picked cards in that window, the race a fleet of concurrent creates hits.
var afterAutomaticPick func(nodeUUID string)

// leavesNodeToServer reports whether req is an automatic single-node create
// that names no node.
func leavesNodeToServer(req *dto.DeployRequest) bool {
	return isAutomaticPlacement(req) && req.NNodes <= 1 && placementPD(req) == nil && strings.TrimSpace(req.NodeUUID) == ""
}

// pickWentStale reports whether a create answered AUTOMATIC_PLACEMENT_STALE.
func pickWentStale(out *capturedResponse) bool {
	if out.status != http.StatusConflict {
		return false
	}
	var answer struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(out.body.Bytes(), &answer) == nil && answer.Code == ErrorAutomaticPlacementStale
}

// pickAutomaticNode lets an automatic single-node create name no node: it
// runs the evaluation the deploy form runs before it submits and names the
// node that evaluation picked, so an API client need not call
// /deployments/evaluate first. The create then re-validates the pick like any
// other, so a request that names its evaluated node behaves exactly as before.
// When the evaluation picks no node, the create answers 409
// AUTOMATIC_PLACEMENT_UNAVAILABLE with each candidate's reason, the same
// reasons the evaluation lists.
func (a *apiHandlers) pickAutomaticNode(r *http.Request, req *dto.DeployRequest) error {
	if !leavesNodeToServer(req) {
		return nil
	}
	body, err := json.Marshal(dto.EvaluateRequest{DeployRequest: *req})
	if err != nil {
		return err
	}
	evalReq := r.Clone(r.Context())
	evalReq.Body = io.NopCloser(bytes.NewReader(body))
	evalReq.ContentLength = int64(len(body))
	out := &capturedResponse{header: http.Header{}, status: http.StatusOK}
	a.evaluate(out, evalReq)
	var resp dto.EvaluateResponse
	if out.over || out.status != http.StatusOK || json.Unmarshal(out.body.Bytes(), &resp) != nil {
		// The evaluation refused the request itself; the create's own checks
		// below answer it with their usual error.
		return nil
	}
	if resp.SelectedNode != nil {
		req.NodeUUID = resp.SelectedNode.UUID
		if afterAutomaticPick != nil {
			afterAutomaticPick(req.NodeUUID)
		}
		return nil
	}
	if len(resp.Errors) > 0 {
		return nil // a field error: the create reports it as a 400
	}
	reasons := make([]string, 0, len(resp.Candidates))
	for _, candidate := range resp.Candidates {
		reasons = append(reasons, candidate.Reason.Code)
	}
	return productError(ErrorAutomaticPlacementUnavailable, "no node satisfies the automatic placement", map[string]any{"candidates": len(resp.Candidates), "reasons": reasons})
}
