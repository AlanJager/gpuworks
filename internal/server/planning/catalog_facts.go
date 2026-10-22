package planning

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// CatalogKVBytesPerToken is the model's measured KV cost per token for one
// engine, as the catalog recorded it. The catalog row is looked up from the
// repositories the model names when the caller does not already hold it, which
// is the difference between the api (which resolved a spec) and the controller
// (which has a deployment row and a model).
//
// Zero means the catalog measured nothing for this engine, and the breakdown
// then reports no token capacity rather than a derived one.
func CatalogKVBytesPerToken(st *store.Store, catalog *store.InferenceModel, model *store.Model, engine string) int64 {
	if catalog == nil && st != nil && model != nil && strings.TrimSpace(model.SourceID) != "" {
		// The model's own repository first - a catalog entry keyed by it is the
		// most direct answer - then the repositories the catalog declares as its
		// own, which is where every quantized release lives.
		if resolved, err := st.InferenceModelByModelID(model.SourceID); err == nil {
			catalog = resolved
		} else if index, err := st.WeightRepoIndex(inferencecatalog.WeightRepoIndex); err == nil {
			if id, ok := index[inferencecatalog.WeightRepoKey(model.SourceID)]; ok {
				if resolved, err := st.InferenceModelByID(id); err == nil {
					catalog = resolved
				}
			}
		}
	}
	if catalog == nil || strings.TrimSpace(catalog.KVBytesPerToken) == "" {
		return 0
	}
	var byEngine map[string]int64
	if err := json.Unmarshal([]byte(catalog.KVBytesPerToken), &byEngine); err != nil {
		return 0
	}
	return byEngine[engine]
}
