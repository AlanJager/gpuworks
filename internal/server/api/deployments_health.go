package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment's health check (internal/server/healthcheck) is a setting of
// the deployment: on or off and how long a check waits. Changing it restarts
// nothing; the next round of checks follows it.

func healthThresholdError(seconds int) error {
	return productError(ErrorFieldInvalid, "health_check_threshold_sec is out of range", map[string]any{
		"field":  "health_check_threshold_sec",
		"detail": fmt.Sprintf("health_check_threshold_sec must be between %d and %d seconds (got %d)", store.HealthCheckThresholdSecLo, store.HealthCheckThresholdSecHi, seconds),
	})
}

func healthUncheckableError() error {
	return productError(ErrorFieldInvalid, "the model serves neither chat nor embeddings", map[string]any{
		"field":  "health_check",
		"detail": "the health check sends a chat or an embedding request; this model serves neither",
	})
}

// validateHealthCheckThreshold gives evaluation and create the same rule for
// a requested threshold; 0 asks for the default.
func validateHealthCheckThreshold(req *dto.DeployRequest) error {
	if req.HealthCheckThresholdSec == 0 || store.ValidHealthCheckThresholdSec(req.HealthCheckThresholdSec) {
		return nil
	}
	return healthThresholdError(req.HealthCheckThresholdSec)
}

// healthCheckable reports whether a deployment of model can be checked: it
// serves chat or embeddings.
func healthCheckable(model *store.Model) bool {
	if model == nil {
		return false
	}
	_, ok := inferenceapi.ModelContract(model).CheckOperation()
	return ok
}

// createHealthCheck is the health check a new deployment starts with: as
// the request says, and on when it says nothing and the model serves chat
// or embeddings.
func createHealthCheck(req *dto.DeployRequest, model *store.Model) (bool, int, error) {
	threshold := req.HealthCheckThresholdSec
	if threshold == 0 {
		threshold = store.DefaultHealthCheckThresholdSec
	}
	checkable := healthCheckable(model)
	if req.HealthCheck == nil {
		return checkable, threshold, nil
	}
	if *req.HealthCheck && !checkable {
		return false, threshold, healthUncheckableError()
	}
	return *req.HealthCheck, threshold, nil
}

// setHealthCheck answers PUT /deployments/{id}/health-check.
func (a *apiHandlers) setHealthCheck(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	dep, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var body dto.HealthCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	threshold := dep.HealthCheckThresholdSec
	if body.ThresholdSec != 0 {
		if !store.ValidHealthCheckThresholdSec(body.ThresholdSec) {
			writeErr(w, http.StatusBadRequest, healthThresholdError(body.ThresholdSec))
			return
		}
		threshold = body.ThresholdSec
	}
	if body.Enabled {
		model, _ := a.d.Store.GetModel(dep.ModelID)
		if !healthCheckable(model) {
			writeErr(w, http.StatusBadRequest, healthUncheckableError())
			return
		}
	}
	if err := a.d.Store.SetDeploymentHealthCheck(dep.ID, body.Enabled, threshold); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dep.HealthCheck, dep.HealthCheckThresholdSec = body.Enabled, threshold
	writeJSON(w, http.StatusOK, a.deploymentDTO(dep, true))
}

// deploymentHealth is a deployment's health check on the wire.
func (a *apiHandlers) deploymentHealth(d *store.Deployment) dto.DeploymentHealth {
	if a.d.Monitor != nil {
		return a.d.Monitor.Health(d.ID, d.HealthCheck, d.HealthCheckThresholdSec)
	}
	status := "off"
	if d.HealthCheck {
		status = "pending"
	}
	return dto.DeploymentHealth{Status: status, ThresholdSec: d.HealthCheckThresholdSec, Recent: []dto.HealthProbe{}}
}
