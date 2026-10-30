package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// setServiceLevel sets how the deployment shares its GPUs with others
// (docs/explanation/service-levels.md). Any level but resident needs a
// sleepable single-node deployment, and two online deployments may not share
// a card.
func (a *apiHandlers) setServiceLevel(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.ServiceLevel
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !store.ValidServiceLevel(req.Level) {
		writeProductError(w, http.StatusBadRequest, messages.ErrorServiceLevelInvalid, "level is resident, online, shared or offline", map[string]any{"level": req.Level})
		return
	}
	if req.Level != store.ServiceLevelResident {
		if code := planning.SleepEligibility(dep); code != "" {
			writeSleepRefusal(w, http.StatusBadRequest, &planning.SleepRefusal{Code: code, Params: map[string]any{"engine": dep.Engine}})
			return
		}
		if pd, _ := planning.PDOf(dep.Placement); pd != nil || dep.NNodes > 1 {
			writeProductError(w, http.StatusBadRequest, messages.ErrorServiceLevelShape, "only a single-node deployment shares its cards by service level", nil)
			return
		}
	}
	if req.Level == store.ServiceLevelOnline {
		other, err := a.onlineSharingCards(dep)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if other != "" {
			writeProductError(w, http.StatusConflict, messages.ErrorServiceLevelOnlineShared, "another online deployment shares this deployment's cards", map[string]any{"deployment": other})
			return
		}
	}
	if err := a.d.Store.SetDeploymentServiceLevel(dep.ID, req.Level); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// onlineSharingCards names another online deployment with an instance on one
// of dep's cards, "" when there is none.
func (a *apiHandlers) onlineSharingCards(dep *store.Deployment) (string, error) {
	mine, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		return "", err
	}
	cards := planning.InstanceCards(mine)
	if len(cards) == 0 {
		return "", nil
	}
	sharing, err := a.d.Store.SharingDeployments()
	if err != nil {
		return "", err
	}
	for i := range sharing {
		other := &sharing[i]
		if other.ID == dep.ID || other.ServiceLevel != store.ServiceLevelOnline {
			continue
		}
		insts, err := a.d.Store.InstancesForDeployment(other.ID)
		if err != nil {
			return "", err
		}
		for card := range planning.InstanceCards(insts) {
			if cards[card] {
				return other.Name, nil
			}
		}
	}
	return "", nil
}

// createServiceLevel is the level a new deployment is stored with: resident
// when the request names none.
func createServiceLevel(level string) string {
	if level == "" {
		return store.ServiceLevelResident
	}
	return level
}

// createServiceLevelValid checks a new deployment's service level as
// setServiceLevel checks a change, writing the refusal. Whether an online
// deployment would share a card with another online one is the placement's
// to keep from happening: it has no cards yet.
func createServiceLevelValid(w http.ResponseWriter, req dto.DeployRequest, engine string, pd bool) bool {
	level := createServiceLevel(req.ServiceLevel)
	if !store.ValidServiceLevel(level) {
		writeProductError(w, http.StatusBadRequest, messages.ErrorServiceLevelInvalid, "level is resident, online, shared or offline", map[string]any{"level": req.ServiceLevel})
		return false
	}
	if level == store.ServiceLevelResident {
		return true
	}
	if !req.Sleepable {
		writeProductError(w, http.StatusBadRequest, ErrorSleepNotEnabled, sleepRefusalFallback(ErrorSleepNotEnabled), map[string]any{"engine": engine})
		return false
	}
	if pd || req.NNodes > 1 {
		writeProductError(w, http.StatusBadRequest, messages.ErrorServiceLevelShape, "only a single-node deployment shares its cards by service level", nil)
		return false
	}
	return true
}
