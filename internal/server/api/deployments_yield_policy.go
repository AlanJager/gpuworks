package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// setYieldPolicy sets how the deployment shares its GPUs with others and the
// parameters that go with it (docs/explanation/service-levels.md). A policy
// other than never, or auto sleep, needs a sleepable deployment; a policy
// other than never a single-node one; and two idle deployments may not share
// a card. It answers the policy as stored, with its derived preset.
func (a *apiHandlers) setYieldPolicy(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.YieldPolicyRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p, v := req.Parse()
	if v != nil {
		writeYieldPolicyViolation(w, v)
		return
	}
	pd, _ := planning.PDOf(dep.Placement)
	if !yieldPolicyEligible(w, p, pd != nil || dep.NNodes > 1, func() *planning.SleepRefusal {
		if code := planning.SleepEligibility(dep); code != "" {
			return &planning.SleepRefusal{Code: code, Params: map[string]any{"engine": dep.Engine}}
		}
		return nil
	}) {
		return
	}
	if p.Yield == servicelevel.Idle {
		other, err := a.idleSharingCards(dep)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if other != "" {
			writeProductError(w, http.StatusConflict, messages.ErrorYieldPolicyIdleShared, "another idle-policy deployment shares this deployment's cards", map[string]any{"deployment": other})
			return
		}
	}
	if err := a.d.Store.SetDeploymentYieldPolicy(dep.ID, p); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.YieldPolicyOf(p))
}

// writeYieldPolicyViolation writes why a yield policy is refused.
func writeYieldPolicyViolation(w http.ResponseWriter, v *servicelevel.Violation) {
	params := map[string]any{"field": v.Field, "reason": v.Reason}
	text := fmt.Sprintf("%s is not a valid value", v.Field)
	switch v.Reason {
	case servicelevel.ReasonRange:
		params["minimum"], params["maximum"] = v.Min, v.Max
		text = fmt.Sprintf("%s is %d to %d", v.Field, v.Min, v.Max)
	case servicelevel.ReasonRequired:
		text = fmt.Sprintf("%s is required", v.Field)
	}
	writeProductError(w, http.StatusBadRequest, messages.ErrorYieldPolicyInvalid, text, params)
}

// yieldPolicyEligible checks the policy against what the deployment is: it
// writes the refusal and returns false when a policy other than never, or
// auto sleep, meets a deployment that cannot sleep, or a policy other than
// never a multi-node or PD one. refusal says why a deployment cannot sleep.
func yieldPolicyEligible(w http.ResponseWriter, p servicelevel.Policy, multiNode bool, refusal func() *planning.SleepRefusal) bool {
	if p.Yield == servicelevel.Never && !p.AutoSleep {
		return true
	}
	if r := refusal(); r != nil {
		writeSleepRefusal(w, http.StatusBadRequest, r)
		return false
	}
	if p.Yield != servicelevel.Never && multiNode {
		writeProductError(w, http.StatusBadRequest, messages.ErrorYieldPolicyShape, "only a single-node deployment shares its cards by yield policy", nil)
		return false
	}
	return true
}

// idleSharingCards names another idle-policy deployment with an instance on
// one of dep's cards, "" when there is none.
func (a *apiHandlers) idleSharingCards(dep *store.Deployment) (string, error) {
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
		if other.ID == dep.ID || other.YieldPolicy != servicelevel.Idle {
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

// createYieldPolicy is the policy a new deployment is stored with: never when
// the request states none. It checks it as setYieldPolicy checks a change,
// writing the refusal. Whether an idle deployment would share a card with
// another idle one is the placement's to keep from happening: it has no cards
// yet.
func createYieldPolicy(w http.ResponseWriter, req dto.DeployRequest, engine string, pd bool) (servicelevel.Policy, bool) {
	if req.YieldPolicy == nil {
		return servicelevel.Policy{Yield: servicelevel.Never}, true
	}
	p, v := req.YieldPolicy.Parse()
	if v != nil {
		writeYieldPolicyViolation(w, v)
		return p, false
	}
	ok := yieldPolicyEligible(w, p, pd || req.NNodes > 1, func() *planning.SleepRefusal {
		if !req.Sleepable {
			return &planning.SleepRefusal{Code: ErrorSleepNotEnabled, Params: map[string]any{"engine": engine}}
		}
		return nil
	})
	return p, ok
}

// requestYield is the yield policy a request states, for the placement
// previews that must not refuse what the create would: never when it states
// none or a malformed one.
func requestYield(stated *dto.YieldPolicyRequest) string {
	if stated == nil {
		return servicelevel.Never
	}
	if p, v := stated.Parse(); v == nil {
		return p.Yield
	}
	return servicelevel.Never
}
