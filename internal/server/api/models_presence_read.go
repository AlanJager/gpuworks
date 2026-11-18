package api

import (
	"net/http"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
)

// modelPresence answers the visible page's rapidly changing file counters.
// Structural events continue to invalidate the ordinary paged model list.
func (a *apiHandlers) modelPresence(w http.ResponseWriter, r *http.Request) {
	raw, ok := listParam(r.URL.Query(), "ids")
	parts := strings.Split(raw, ",")
	if !ok || len(parts) > listMaxPageSize {
		writeErr(w, http.StatusBadRequest, listPageError("ids", raw))
		return
	}
	ids, seen := []uint{}, map[uint]bool{}
	for _, part := range parts {
		value, err := strconv.ParseUint(part, 10, 63)
		if err != nil || value == 0 {
			writeErr(w, http.StatusBadRequest, listPageError("ids", raw))
			return
		}
		id := uint(value)
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	files, storages, nodes, err := a.d.Store.ModelPresenceFacts(ids)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	lookup := presenceLookup{PreparationReads: deployment.NewPreparationReads(a.d.Preparation), files: files, storages: storages, nodes: nodes}
	out := make(map[uint]dto.ModelPresenceSummary, len(files))
	for id := range files {
		var model dto.Model
		lookup.fill(&model, id)
		out[id] = modelPresenceSummary(model)
	}
	writeJSON(w, http.StatusOK, out)
}
