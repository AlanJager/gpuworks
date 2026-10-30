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
// other than never needs a sleepable, single-node deployment. It answers the
// policy as stored, naming the other idle-policy deployments on the same
// cards when the policy is idle: the two may share, but neither gives way
// to the other while busy.
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
	if err := a.d.Store.SetDeploymentYieldPolicy(dep.ID, p); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.YieldPolicyOf(p)
	if p.Yield == servicelevel.Idle {
		if out.IdleNeighbours, err = a.idleNeighbours(dep); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
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
// writes the refusal and returns false when a policy other than never meets a
// deployment that cannot sleep, or a multi-node or PD one. refusal says why a
// deployment cannot sleep.
func yieldPolicyEligible(w http.ResponseWriter, p servicelevel.Policy, multiNode bool, refusal func() *planning.SleepRefusal) bool {
	if p.Yield == servicelevel.Never {
		return true
	}
	if r := refusal(); r != nil {
		writeSleepRefusal(w, http.StatusBadRequest, r)
		return false
	}
	if multiNode {
		writeProductError(w, http.StatusBadRequest, messages.ErrorYieldPolicyShape, "only a single-node deployment shares its cards by yield policy", nil)
		return false
	}
	return true
}

// idleNeighbours names the other idle-policy deployments with an instance on
// one of dep's cards.
func (a *apiHandlers) idleNeighbours(dep *store.Deployment) ([]string, error) {
	mine, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		return nil, err
	}
	cards := planning.InstanceCards(mine)
	if len(cards) == 0 {
		return nil, nil
	}
	sharing, err := a.d.Store.SharingDeployments()
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range sharing {
		other := &sharing[i]
		if other.ID == dep.ID || !servicelevel.IdleNeighbours(servicelevel.Idle, other.YieldPolicy) {
			continue
		}
		insts, err := a.d.Store.InstancesForDeployment(other.ID)
		if err != nil {
			return nil, err
		}
		for card := range planning.InstanceCards(insts) {
			if cards[card] {
				out = append(out, other.Name)
				break
			}
		}
	}
	return out, nil
}

// createYieldPolicy is the policy a new deployment is stored with: never when
// the request states none. It checks it as setYieldPolicy checks a change,
// writing the refusal.
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

// yieldPolicyDictionary answers the yield policies with their defaults,
// which parameters apply and the parameters' ranges, for a client's editor.
func (a *apiHandlers) yieldPolicyDictionary(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dto.YieldPolicyDictionaryOf())
}
