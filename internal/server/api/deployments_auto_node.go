package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// pickAutomaticNode lets an automatic single-node create name no node: it
// runs the evaluation the deploy form runs before it submits and names the
// node that evaluation picked, so an API client need not call
// /deployments/evaluate first. The create then re-validates the pick like any
// other, so a request that names its evaluated node behaves exactly as before.
// When the evaluation picks no node, the create answers 409
// AUTOMATIC_PLACEMENT_UNAVAILABLE with each candidate's reason, the same
// reasons the evaluation lists.
func (a *apiHandlers) pickAutomaticNode(r *http.Request, req *dto.DeployRequest) error {
	if !isAutomaticPlacement(req) || req.NNodes > 1 || placementPD(req) != nil || strings.TrimSpace(req.NodeUUID) != "" {
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
