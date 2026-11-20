package api

import (
	"errors"
	"net/http"
)

// errBusinessNotReady is all a probe learns from a server that is not ready.
var errBusinessNotReady = errors.New("business service is not ready")

// ready is anonymous but remains inside the App's ordinary business gates.
// A fixed response exposes no settings, identities, counts, or storage errors.
func (a *apiHandlers) ready(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.d.Store == nil || a.d.AuthSigner == nil || a.d.Store.CheckBusinessReads(r.Context()) != nil {
		writeErr(w, http.StatusServiceUnavailable, errBusinessNotReady)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
