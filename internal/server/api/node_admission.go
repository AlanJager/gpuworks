package api

import (
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// writeNodeEnrollmentRefused answers err with 403 and the guard's product
// error when an enrollment guard refused the node (store.AddNodeEnrollmentGuard),
// and reports whether it did.
func writeNodeEnrollmentRefused(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, store.ErrNodeEnrollmentRefused) {
		return false
	}
	writeErr(w, http.StatusForbidden, err)
	return true
}

// nodeEnrollmentOpen asks the enrollment guards whether a node could join
// before the caller spends anything on one (renting a machine, installing
// over SSH, applying a cluster's manifest), and answers the request itself
// when not. It is advisory: the insert asks again in its own transaction.
func (a *apiHandlers) nodeEnrollmentOpen(w http.ResponseWriter) bool {
	if a.d.Store == nil {
		return true
	}
	err := a.d.Store.CheckNodeEnrollment()
	if err == nil {
		return true
	}
	if !writeNodeEnrollmentRefused(w, err) {
		writeErr(w, http.StatusInternalServerError, err)
	}
	return false
}

// kubeEnrollRefusalParams are KUBE_ENROLL_REFUSED's params for a node an
// enrollment guard refused: reason enrollment_refused, the guard's code as
// refusal_code, and the guard's own params beside them.
func kubeEnrollRefusalParams(err error) map[string]any {
	params := map[string]any{}
	var refusal *messages.ProductError
	if errors.As(err, &refusal) {
		for key, value := range refusal.Params() {
			params[key] = value
		}
		params["refusal_code"] = refusal.Code()
	}
	params["reason"] = "enrollment_refused"
	return params
}
