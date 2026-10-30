package proxy

import (
	"encoding/json"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// ModelsHandler serves GET /v1/models listing deployed served-names (OpenAI shape).
func (p *Proxy) ModelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := p.authenticate(w, r); !ok {
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
			if d.State == store.DeployActive && d.ServedName != "" {
				out = append(out, model{ID: d.ServedName, Object: "model", OwnedBy: "gpuworks"})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
	}
}
