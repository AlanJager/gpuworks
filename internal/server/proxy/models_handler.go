package proxy

import (
	"encoding/json"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// ModelsHandler serves GET /v1/models listing deployed served-names (OpenAI
// shape), only those of deployments the caller's API key may call.
func (p *Proxy) ModelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, identity, ok := p.authenticate(w, r)
		if !ok {
			return
		}
		deps, _ := p.store.ListDeployments()
		type model struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		}
		out := []model{}
		for _, d := range deps {
			if d.State == store.DeployActive && d.ServedName != "" && identity.Allows(d.ID) {
				out = append(out, model{ID: d.ServedName, Object: "model", OwnedBy: "gpuworks"})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
	}
}
