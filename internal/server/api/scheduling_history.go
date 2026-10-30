package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// schedulingHistory answers GET /scheduling/history: the scheduling events of
// every deployment (or of deployment_id), newest first, cut by the same
// before/limit cursor as the deployment timeline. category selects auto,
// failure and manual entries (comma separated); since and until bound the
// time (RFC 3339). It only reads.
func (a *apiHandlers) schedulingHistory(w http.ResponseWriter, r *http.Request) {
	before, limit, code, detail := eventPage(r)
	if code != "" {
		writeProductError(w, http.StatusBadRequest, code, detail, nil)
		return
	}
	filter := store.SchedulingHistoryFilter{Before: before, Limit: limit}
	query := r.URL.Query()
	invalid := func(field, reason string) {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, field+" "+reason, map[string]any{"field": field})
	}
	if raw := strings.TrimSpace(query.Get("deployment_id")); raw != "" {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || id == 0 {
			invalid("deployment_id", "is a deployment id")
			return
		}
		filter.DeploymentID = uint(id)
	}
	for _, name := range strings.Split(strings.Join(query["category"], ","), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if category := store.SchedulingCategory(name); store.ValidSchedulingCategory(category) {
			filter.Categories = append(filter.Categories, category)
			continue
		}
		invalid("category", "is auto, failure or manual")
		return
	}
	for _, bound := range []struct {
		field string
		into  *time.Time
	}{{"since", &filter.Since}, {"until", &filter.Until}} {
		field, into := bound.field, bound.into
		raw := strings.TrimSpace(query.Get(field))
		if raw == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			invalid(field, "is an RFC 3339 time")
			return
		}
		*into = at
	}
	events, err := a.d.Store.SchedulingHistory(filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ids := make([]uint, 0, len(events))
	seen := map[uint]bool{}
	for _, ev := range events {
		if !seen[ev.DeploymentID] {
			seen[ev.DeploymentID] = true
			ids = append(ids, ev.DeploymentID)
		}
	}
	names, err := a.d.Store.DeploymentNames(ids)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]dto.SchedulingHistoryEntry, 0, len(events))
	for _, ev := range events {
		category, _ := store.SchedulingCategoryOf(ev.Code)
		item := dto.SchedulingHistoryEntry{
			ID: ev.ID, CreatedAt: utcTime(ev.CreatedAt), DeploymentID: ev.DeploymentID, Instance: ev.InstanceUUID,
			Kind: ev.Kind, Code: ev.Code, Category: string(category), Params: map[string]any{},
		}
		if name, ok := names[ev.DeploymentID]; ok {
			item.Deployment, item.DeploymentDeleted = name.Name, name.Deleted
		}
		if ev.Data != "" {
			_ = json.Unmarshal([]byte(ev.Data), &item.Params)
		}
		if item.Params == nil {
			item.Params = map[string]any{}
		}
		if actor, ok := item.Params["actor"].(string); ok {
			item.Actor = actor
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, dto.NewCursorPage(items))
}
