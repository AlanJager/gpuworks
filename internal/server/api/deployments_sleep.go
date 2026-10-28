// deployments_sleep.go carries the sleep and wake orders for a deployment.
// The policy behind them (who may sleep, what a sleeper still occupies, whether
// a wake fits) lives in internal/server/sleep and reaches here through
// planning; these handlers only translate verdicts into HTTP.

package api

import (
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// sleepDeployment sets the desired sleep level (?level=1|2, default 1) and the
// pause mode (?mode=wait|abort, default wait). The level and mode are stamped
// into each live instance's desired state, which the controller pushes to the
// agent. ?role= orders only a PD deployment's instances of that role; the
// deployment as a whole is then not under the order.
func (a *apiHandlers) sleepDeployment(w http.ResponseWriter, r *http.Request) {
	defer planning.LockLifecycle()()
	level, mode, ok := sleepOrderParams(w, r)
	if !ok {
		return
	}
	dep, ok := a.sleepEligibleDeployment(w, r)
	if !ok {
		return
	}
	role := r.URL.Query().Get("role")
	if role == "" {
		if level == proto.SleepLevelL1 {
			insts, err := a.d.Store.InstancesForDeployment(dep.ID)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if refusal := planning.L1HostMemoryRefusal(a.d.Store, dep, planning.Sleepers(insts), a.hostMemory); refusal != nil {
				writeSleepRefusal(w, http.StatusConflict, refusal)
				return
			}
		}
		if err := a.d.Store.OrderDeploymentSleep(dep.ID, level, mode); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, dto.SleepOrder{Status: "sleeping", Level: level, Mode: mode})
		return
	}
	named, ok := a.roleSleepTargets(w, dep, role, true)
	if !ok {
		return
	}
	if level == proto.SleepLevelL1 {
		if refusal := planning.L1HostMemoryRefusal(a.d.Store, dep, named, a.hostMemory); refusal != nil {
			writeSleepRefusal(w, http.StatusConflict, refusal)
			return
		}
	}
	if err := a.d.Store.OrderInstancesSleep(dep.ID, uuidsOf(named), level, mode); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.SleepOrder{Status: "sleeping", Level: level, Mode: mode, Role: role, Instances: uuidsOf(named)})
}

// sleepOrderParams reads ?level= and ?mode=, writing the response when they
// are invalid.
func sleepOrderParams(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	level, mode, code := parseSleepOrder(r.URL.Query().Get("level"), r.URL.Query().Get("mode"))
	switch code {
	case ErrorSleepInvalidLevel:
		writeProductError(w, http.StatusBadRequest, code, "sleep level must be 1 or 2", map[string]any{"level": r.URL.Query().Get("level")})
		return 0, "", false
	case ErrorSleepInvalidMode:
		writeProductError(w, http.StatusBadRequest, code, "sleep mode must be wait or abort", map[string]any{"mode": r.URL.Query().Get("mode")})
		return 0, "", false
	}
	return level, mode, true
}

// parseSleepOrder validates protocol values at the HTTP boundary. Sleep levels
// and pause modes are shared wire vocabulary, so their values live in proto;
// this package owns the API error codes and fallback text.
func parseSleepOrder(rawLevel, rawMode string) (level int, mode string, code string) {
	switch rawLevel {
	case "", "1":
		level = proto.SleepLevelL1
	case "2":
		level = proto.SleepLevelL2
	default:
		return 0, "", ErrorSleepInvalidLevel
	}
	switch rawMode {
	case "":
		mode = proto.SleepModeWait
	case proto.SleepModeWait, proto.SleepModeAbort:
		mode = rawMode
	default:
		return 0, "", ErrorSleepInvalidMode
	}
	return level, mode, ""
}

// wakeDeployment wakes a sleeping deployment once its GPUs can take the
// instances back. A card an awake co-tenant has since claimed refuses the wake
// with the shortfall, rather than letting the engine find out by running out of
// memory. ?role= wakes only a PD deployment's instances of that role.
// ?partial=true wakes the sleepers that fit and lists the rest with their
// shortfall instead of refusing them all. Any wake withdraws a deployment-wide
// sleep order.
func (a *apiHandlers) wakeDeployment(w http.ResponseWriter, r *http.Request) {
	defer planning.LockLifecycle()()
	partial := false
	if raw := r.URL.Query().Get("partial"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		partial = parsed
	}
	dep, ok := a.sleepEligibleDeployment(w, r)
	if !ok {
		return
	}
	role := r.URL.Query().Get("role")
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	named := planning.Sleepers(insts)
	if role != "" {
		if named, ok = a.roleSleepTargets(w, dep, role, false); !ok {
			return
		}
	}
	match, err := a.deploymentUnitMatch(dep, insts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	how, ok := wakeWaitParams(w, r)
	if !ok {
		return
	}
	if how.wait || how.yield {
		a.wakeAndWait(w, dep, match, named, role == "", how)
		return
	}
	woken, refused, err := planning.OrderWake(a.d.Store, dep, match, named, partial, role == "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(woken) == 0 && len(refused) > 0 {
		writeWakeRefusal(w, refused)
		return
	}
	result := dto.WakeResult{Status: "waking"}
	if partial {
		result.Woken = uuidsOf(woken)
		for _, refusal := range refused {
			result.Refused = append(result.Refused, wakeRefusalDTO(refusal))
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// sleepEligibleDeployment loads the deployment a sleep or wake order names and
// refuses the order when the deployment cannot sleep at all. It writes the
// response on refusal.
func (a *apiHandlers) sleepEligibleDeployment(w http.ResponseWriter, r *http.Request) (*store.Deployment, bool) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, false
	}
	if code := planning.SleepEligibility(dep); code != "" {
		writeSleepRefusal(w, http.StatusBadRequest, &planning.SleepRefusal{Code: code, Params: map[string]any{"engine": dep.Engine}})
		return nil, false
	}
	return dep, true
}

// roleSleepTargets is a PD deployment's instances of a role that a sleep
// (sleeping) or wake order by role names, refused as planning.RoleSleepRefusal
// says. It writes the response on refusal.
func (a *apiHandlers) roleSleepTargets(w http.ResponseWriter, dep *store.Deployment, role string, sleeping bool) ([]store.Instance, bool) {
	if refusal := planning.RoleSleepRefusal(dep, role, sleeping); refusal != nil {
		writeSleepRefusal(w, http.StatusBadRequest, refusal)
		return nil, false
	}
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, false
	}
	return planning.RoleInstances(insts, role), true
}

// writeSleepRefusal answers a refused sleep or wake order with its code and
// the English fallback; the UI renders the code.
func writeSleepRefusal(w http.ResponseWriter, status int, refusal *planning.SleepRefusal) {
	writeProductError(w, status, refusal.Code, sleepRefusalFallback(refusal.Code), refusal.Params)
}

// sleepRefusalFallback is the English wire text for a refused sleep order.
func sleepRefusalFallback(code string) string {
	switch code {
	case ErrorSleepNotEnabled:
		return "sleep mode was not enabled for this deployment"
	case ErrorSleepEngineUnsupported:
		return "the deployment's engine does not support sleep mode"
	case ErrorInstanceNotServing:
		return "the instance fills no replica: it failed, is draining or is being removed"
	case ErrorSleepRoleCannotEmpty:
		return "the role cannot be left without an awake instance; sleep the whole deployment instead"
	case ErrorSleepHostMemoryInsufficient:
		return "the node's host memory cannot hold the weights an L1 sleep moves there; sleep at L2 instead"
	case messages.ErrorPDNotPD:
		return "deployment is not PD disaggregated"
	case messages.ErrorPDRoleUnknown:
		return "the role is not a role of this deployment"
	}
	return "sleep order refused"
}

func appendUnique(values []string, more ...string) []string {
	for _, v := range more {
		if !slices.Contains(values, v) {
			values = append(values, v)
		}
	}
	return values
}

// hostMemory is a node's host memory as its agent last reported it (within
// two minutes).
func (a *apiHandlers) hostMemory(nodeUUID string) (available, total int64, ok bool) {
	if a.d.MetricsLatest == nil {
		return 0, 0, false
	}
	since := time.Now().UTC().Add(-2 * time.Minute).UnixMilli()
	return planning.HostMemoryFromMetrics(func(metric, node string) (float64, bool) {
		rows, err := a.d.MetricsLatest(metric, map[string]string{"node": node}, since)
		if err != nil || len(rows) == 0 {
			return 0, false
		}
		return rows[0].Value, true
	})(nodeUUID)
}
