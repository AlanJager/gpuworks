package api

import (
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func (a *apiHandlers) routingPolicies(w http.ResponseWriter, _ *http.Request) {
	options := make([]dto.RoutingPolicyOption, 0, len(deploy.RoutingPolicyOptions()))
	for _, option := range deploy.RoutingPolicyOptions() {
		options = append(options, dto.RoutingPolicyOption{Value: option.Value, Label: option.Label, Description: option.Description})
	}
	writeJSON(w, http.StatusOK, dto.NewList(options))
}
