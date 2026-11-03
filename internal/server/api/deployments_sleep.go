// deployments_sleep.go carries the sleep and wake orders for a deployment.
// Lifecycle and wake admission live in internal/server/sleep and planning;
// selected-card sleep capability policy lives in planning. These handlers
// translate verdicts into HTTP and issue the resulting store orders.

package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
// deployment as a whole is then not under the order. ?wake_on_request= says
// whether a request wakes what the order puts to sleep, by default
// planning.OperatorSleepWakesOnRequest; a sleep a request wakes names the
// awake replicas rather than putting the deployment as a whole under the
// order, which only a held sleep does.
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
	wakes, ok := wakeOnRequestParam(w, r, dep)
	if !ok {
		return
	}
	role := r.URL.Query().Get("role")
	if role == "" {
		insts, err := a.d.Store.InstancesForDeployment(dep.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if a.refuseUnsupportedSleepTargets(w, dep.Engine, insts) {
			return
		}
		if !a.orderSleep(w, dep, planning.SleepOrder{Pref: planning.SleepPreference(level), Mode: mode, Held: !wakes}, writeErr500) {
			return
		}
		a.sleepOrdered(r, dep.ID, level, mode, "deployment", "", nil, wakes)
		writeJSON(w, http.StatusOK, dto.SleepOrder{Status: "sleeping", Level: level, Mode: mode, WakeOnRequest: wakes})
		return
	}
	named, ok := a.roleSleepTargets(w, dep, role, true)
	if !ok {
		return
	}
	if a.refuseUnsupportedSleepTargets(w, dep.Engine, named) {
		return
	}
	if !a.orderSleep(w, dep, planning.SleepOrder{Pref: planning.SleepPreference(level), Mode: mode, Held: !wakes, Instances: uuidsOf(named)}, writeErr500) {
		return
	}
	a.sleepOrdered(r, dep.ID, level, mode, "role", role, uuidsOf(named), wakes)
	writeJSON(w, http.StatusOK, dto.SleepOrder{Status: "sleeping", Level: level, Mode: mode, Role: role, Instances: uuidsOf(named), WakeOnRequest: wakes})
}

// wakeOnRequestParam reads ?wake_on_request=true|false, whether a request
// wakes what an operator's sleep puts to sleep; absent, it is the
// deployment's default (planning.OperatorSleepWakesOnRequest). A deployment
// that cannot sleep was refused before. It writes the response when invalid.
func wakeOnRequestParam(w http.ResponseWriter, r *http.Request, dep *store.Deployment) (bool, bool) {
	raw := r.URL.Query().Get("wake_on_request")
	if raw == "" {
		return planning.OperatorSleepWakesOnRequest(dep), true
	}
	wakes, err := strconv.ParseBool(raw)
	if err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorSleepInvalidWakeOnRequest, "wake_on_request must be true or false", map[string]any{"wake_on_request": raw})
		return false, false
	}
	return wakes, true
}

// orderSleep writes an operator's sleep order (planning.OrderSleep), which
// names its level explicitly: a refusal is 409 (the deployment's sleep
// eligibility was checked before, as a 400), a store failure goes to
// storeErr. It writes the response unless the order was written.
func (a *apiHandlers) orderSleep(w http.ResponseWriter, dep *store.Deployment, order planning.SleepOrder, storeErr func(http.ResponseWriter, error)) bool {
	_, refusal, err := planning.OrderSleep(a.d.Store, dep, order, a.hostMemory)
	if err != nil {
		storeErr(w, err)
		return false
	}
	if refusal != nil {
		writeSleepRefusal(w, http.StatusConflict, refusal)
		return false
	}
	return true
}

func writeErr500(w http.ResponseWriter, err error) { writeErr(w, http.StatusInternalServerError, err) }

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
		a.wakeAndWait(w, r, dep, match, named, role == "", how)
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
	a.wakeOrdered(r, dep.ID, uuidsOf(woken))
	result := dto.WakeResult{Status: "waking"}
	if partial {
		result.Woken = uuidsOf(woken)
		for _, refusal := range refused {
			result.Refused = append(result.Refused, wakeRefusalDTO(refusal))
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// refuseUnsupportedSleepTargets translates the shared planning decision to
// HTTP. New sleep commands use this check; wake admission deliberately does not.
func (a *apiHandlers) refuseUnsupportedSleepTargets(w http.ResponseWriter, engine string, insts []store.Instance) bool {
	refusal, err := planning.UnsupportedSleepTarget(engine, insts, a.d.Store)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return true
	}
	if refusal == nil {
		return false
	}
	writeSleepRefusal(w, http.StatusBadRequest, refusal)
	return true
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

// writeSleepDeclarationError preserves sleep refusal text and target capability
// errors when a deployment asks to enable sleep.
func writeSleepDeclarationError(w http.ResponseWriter, err error) {
	var product *messages.ProductError
	if errors.As(err, &product) {
		fallback := product.Error()
		if strings.HasPrefix(product.Code(), "SLEEP_") {
			fallback = sleepRefusalFallback(product.Code())
		}
		writeProductError(w, http.StatusBadRequest, product.Code(), fallback, product.Params())
	} else {
		writeErr(w, http.StatusBadRequest, err)
	}
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
	case messages.ErrorAcceleratorFeatureUnsupported:
		return "the selected accelerator does not support sleep mode"
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

// hostMemory is a node's host memory as its agent recently reported it
// (planning.RecentHostMemory).
func (a *apiHandlers) hostMemory(nodeUUID string) (available, total int64, ok bool) {
	host := HostMemory(a.d.MetricsLatest)
	if host == nil {
		return 0, 0, false
	}
	return host(nodeUUID)
}

// HostMemory is planning.RecentHostMemory over latest, the metrics store's
// newest samples (Deps.MetricsLatest): the one reader of host memory for an
// operator's L1 sleep and the automation's. Nil latest knows nothing.
func HostMemory(latest func(metric string, matchers map[string]string, sinceMS int64, asOfMS ...int64) ([]MetricLatest, error)) planning.HostMemory {
	if latest == nil {
		return nil
	}
	return planning.RecentHostMemory(func(metric, node string, sinceMS int64) (float64, bool) {
		rows, err := latest(metric, map[string]string{"node": node}, sinceMS)
		if err != nil || len(rows) == 0 {
			return 0, false
		}
		return rows[0].Value, true
	}, time.Now)
}

// releaseSleepHold lets requests wake the deployment's held sleepers without
// waking them (store.ReleaseSleepHolds): an operator's or a schedule's sleep
// becomes one a request ends, as the platform's own yields are. It answers the
// instances released, none when none was held.
func (a *apiHandlers) releaseSleepHold(w http.ResponseWriter, r *http.Request) {
	defer planning.LockLifecycle()()
	dep, ok := a.sleepEligibleDeployment(w, r)
	if !ok {
		return
	}
	released, err := a.d.Store.ReleaseSleepHolds(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(released) > 0 {
		a.operatorEvent(r, dep.ID, store.EventSleepHoldReleased, map[string]any{"instances": released})
	}
	if released == nil {
		released = []string{}
	}
	writeJSON(w, http.StatusOK, dto.SleepHoldRelease{Released: released})
}
