package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ErrAnalyticsUnavailable answers the analytics routes of a server built
// without the service (a harness).
var ErrAnalyticsUnavailable = errors.New("operations analytics is not available")

// Analytics is operations analytics as the API sees it: the data retention
// setting, applied the moment it is saved, and what it keeps.
type Analytics interface {
	Retention() dto.DataRetentionView
	// SetRetention validates and applies a new setting; an invalid value is
	// a *store.DataRetentionError.
	SetRetention(dto.DataRetention) (dto.DataRetentionView, error)
	RetentionStatus(ctx context.Context) (dto.DataRetentionStatus, error)
}

func (a *apiHandlers) getDataRetention(w http.ResponseWriter, _ *http.Request) {
	if a.d.Analytics == nil {
		writeErr(w, http.StatusNotFound, ErrAnalyticsUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, a.d.Analytics.Retention())
}

func (a *apiHandlers) putDataRetention(w http.ResponseWriter, r *http.Request) {
	if a.d.Analytics == nil {
		writeErr(w, http.StatusNotFound, ErrAnalyticsUnavailable)
		return
	}
	// The setting is replaced whole: a value left out would silently become
	// zero or off, so every one must be there and nothing else.
	var raw map[string]json.RawMessage
	if !decodeJSONBody(w, r, &raw) {
		return
	}
	known := dataRetentionFields()
	for name := range raw {
		if !known[name] {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "unknown field "+name, map[string]any{"field": name, "detail": "unknown field"})
			return
		}
	}
	for name := range known {
		if _, ok := raw[name]; !ok {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, name+" is required", map[string]any{"field": name, "detail": "required"})
			return
		}
	}
	var req dto.DataRetention
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &req); err != nil {
		field := ""
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			field = typeErr.Field
		}
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "a value has the wrong type", map[string]any{"field": field, "detail": "wrong type"})
		return
	}
	view, err := a.d.Analytics.SetRetention(req)
	var invalid *store.DataRetentionError
	switch {
	case errors.As(err, &invalid):
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, invalid.Error(), map[string]any{"field": invalid.Field, "detail": invalid.Detail})
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, view)
	}
}

func (a *apiHandlers) getDataRetentionStatus(w http.ResponseWriter, r *http.Request) {
	if a.d.Analytics == nil {
		writeErr(w, http.StatusNotFound, ErrAnalyticsUnavailable)
		return
	}
	status, err := a.d.Analytics.RetentionStatus(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// dataRetentionFields are dto.DataRetention's JSON names.
func dataRetentionFields() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(dto.DataRetention{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		out[name] = true
	}
	return out
}
