package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// defaultProbeRepo is the repository a connectivity test asks about when the
// operator names none: small, public, on both hubs.
const defaultProbeRepo = "Qwen/Qwen3-0.6B"

// sourceRuleTestTimeout bounds the node's answer: its addresses are asked in
// parallel, 10 s each.
const sourceRuleTestTimeout = 20 * time.Second

// testSourceRule asks one node whether it reaches the addresses a site rule
// being edited offers, the way its downloads would (source.probe): through
// the node's proxy, with the login a download carries. The console asks the
// nodes it tests in parallel, one request each, and shows each answer as it
// arrives. Nothing is stored.
func (a *apiHandlers) testSourceRule(w http.ResponseWriter, r *http.Request) {
	var req dto.SourceRuleTest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	bases, source, err := planning.DraftSiteBases(req.Match, req.Endpoints, req.AllowUpstream)
	if err != nil {
		writePullSettingError(w, err)
		return
	}
	repo := strings.Trim(strings.TrimSpace(req.Repo), "/")
	if repo == "" {
		repo = defaultProbeRepo
	}
	if strings.Count(repo, "/") != 1 || strings.ContainsAny(repo, " ?#") {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "repo is an organization/name repository id", map[string]any{"field": "repo", "detail": "repo is an organization/name repository id"})
		return
	}
	if strings.TrimSpace(req.NodeUUID) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "node_uuid is required", map[string]any{"field": "node_uuid", "detail": "node_uuid is required"})
		return
	}
	node, err := a.d.Store.NodeByUUID(req.NodeUUID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if a.d.CallNode == nil || (a.d.Online != nil && !a.d.Online(node.UUID)) {
		writeProductError(w, http.StatusConflict, ErrorNodeOffline, "node is offline", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sourceRuleTestTimeout)
	defer cancel()
	raw, err := a.d.CallNode(ctx, node.UUID, proto.RPCSourceProbe, proto.SourceProbeParams{Source: source, Repo: repo, Bases: bases})
	var result proto.SourceProbeResult
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	if err != nil {
		detail := err.Error()
		writeProductError(w, http.StatusBadGateway, ErrorSourceProbeFailed, "the node could not run the test (an agent older than this server cannot): "+detail, map[string]any{"detail": detail})
		return
	}
	out := dto.SourceRuleTestResult{Source: source, Repo: repo, NodeUUID: node.UUID, NodeName: nodeDisplayName(node), Outcomes: make([]dto.SourceProbeOutcome, 0, len(result.Outcomes))}
	for _, outcome := range result.Outcomes {
		out.Outcomes = append(out.Outcomes, dto.SourceProbeOutcome{Base: outcome.Base, Status: outcome.Status, Code: outcome.Code, LatencyMS: outcome.LatencyMS, Error: outcome.Error})
	}
	writeJSON(w, http.StatusOK, out)
}
