package api

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func (a *apiHandlers) engines(w http.ResponseWriter, r *http.Request) {
	out := make([]deploy.Profile, 0, len(deploy.EngineNames()))
	for _, name := range deploy.EngineNames() {
		if p, err := deploy.ProfileForEngine(name); err == nil {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}
