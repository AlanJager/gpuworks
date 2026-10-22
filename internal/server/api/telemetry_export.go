package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// TelemetryService is the export of the gateway's request traces to an
// OpenTelemetry collector (ADR 0046): its stored setting and the running
// exporter. The app implements it over the store and internal/server/traceexport.
type TelemetryService interface {
	// Export is the setting, header values withheld, with the export's status.
	Export() dto.TelemetryExport
	// UpdateExport validates and stores the whole setting and applies it at
	// once; a refused one is a *TelemetryError.
	UpdateExport(update dto.TelemetryExportUpdate) (dto.TelemetryExport, error)
	// TestExport sends one test span with a draft setting; a draft that is
	// not valid is a *TelemetryError, a collector that refuses is a result.
	TestExport(ctx context.Context, update dto.TelemetryExportUpdate) (dto.TelemetryExportTest, error)
}

// ErrTelemetryUnavailable answers the telemetry routes of a server built
// without the service (a harness).
var ErrTelemetryUnavailable = errors.New("telemetry export is not available")

// TelemetryError is a refused telemetry setting, carried to the client as a
// product error.
type TelemetryError struct {
	Code     string
	Fallback string
	Params   map[string]any
}

func (e *TelemetryError) Error() string { return e.Fallback }

// getTelemetryExport serves GET /api/v1/telemetry/export. Any signed-in user
// may read it: the request view needs the trace link, and no secret is in it.
func (a *apiHandlers) getTelemetryExport(w http.ResponseWriter, r *http.Request) {
	if a.d.Telemetry == nil {
		writeErr(w, http.StatusNotFound, ErrTelemetryUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, a.d.Telemetry.Export())
}

// putTelemetryExport serves PUT /api/v1/telemetry/export (administrators).
func (a *apiHandlers) putTelemetryExport(w http.ResponseWriter, r *http.Request) {
	update, ok := a.telemetryUpdateBody(w, r)
	if !ok {
		return
	}
	out, err := a.d.Telemetry.UpdateExport(update)
	if err != nil {
		writeTelemetryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// testTelemetryExport serves POST /api/v1/telemetry/export/test
// (administrators): one span to the draft setting's collector. The answer is
// 200 either way; ok says whether the collector took it.
func (a *apiHandlers) testTelemetryExport(w http.ResponseWriter, r *http.Request) {
	update, ok := a.telemetryUpdateBody(w, r)
	if !ok {
		return
	}
	out, err := a.d.Telemetry.TestExport(r.Context(), update)
	if err != nil {
		writeTelemetryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) telemetryUpdateBody(w http.ResponseWriter, r *http.Request) (dto.TelemetryExportUpdate, bool) {
	var update dto.TelemetryExportUpdate
	if a.d.Telemetry == nil {
		writeErr(w, http.StatusNotFound, ErrTelemetryUnavailable)
		return update, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&update); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return update, false
	}
	return update, true
}

func writeTelemetryError(w http.ResponseWriter, err error) {
	var te *TelemetryError
	if errors.As(err, &te) {
		writeProductError(w, http.StatusBadRequest, te.Code, te.Fallback, te.Params)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}
