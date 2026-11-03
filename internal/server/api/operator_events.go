package api

import (
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// operatorEvent records an operator's order on the deployment's timeline, with
// the user who gave it as params["actor"] when the request names one. The
// platform's own actions (the controller, the automation) are recorded without
// an actor: nobody ordered them.
func (a *apiHandlers) operatorEvent(r *http.Request, deploymentID uint, code string, params map[string]any) {
	if params == nil {
		params = map[string]any{}
	}
	if claims, ok := a.claimsFromRequest(r); ok && claims.Username != "" {
		params["actor"] = claims.Username
	}
	a.d.Store.AddDeploymentEventCode(deploymentID, operatorEventKind(code), code, params)
}

// operatorEventKind is the kind the scheduling history's catalogue writes an
// order with (store.SchedulingEventKind): the orders recorded only for the
// history (a replica count, sleep, wake) stay out of EventKindSchedule, whose
// newest entry is what a deployment's placement status reports, while the
// restarts and retries that were recorded there before stay there. A code the
// catalogue does not list is a schedule entry.
func operatorEventKind(code string) string {
	if kind, ok := store.SchedulingEventKind(code); ok {
		return kind
	}
	return store.EventKindSchedule
}

// sleepOrdered records an operator's sleep order. scope says what the order
// named: the whole deployment, one PD role or one replica; instances are the
// ones the order was stamped on when it named them; wakeOnRequest whether a
// request wakes them (false: the sleep is held).
func (a *apiHandlers) sleepOrdered(r *http.Request, deploymentID uint, level int, mode, scope, role string, instances []string, wakeOnRequest bool) {
	params := map[string]any{"level": level, "mode": mode, "scope": scope, "wake_on_request": wakeOnRequest}
	if role != "" {
		params["role"] = role
	}
	if len(instances) > 0 {
		params["instances"] = instances
	}
	a.operatorEvent(r, deploymentID, store.EventSleepOrdered, params)
}

// wakeOrdered records the instances an operator's wake order started waking;
// an order that woke none (all of them wait for room) leaves no entry here,
// since the wait has its own.
func (a *apiHandlers) wakeOrdered(r *http.Request, deploymentID uint, instances []string) {
	if len(instances) > 0 {
		a.operatorEvent(r, deploymentID, store.EventWakeOrdered, map[string]any{"instances": instances})
	}
}
