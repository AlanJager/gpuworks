package proxy

import (
	"context"
	"fmt"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// An auxiliary route (inferenceapi.AuxiliaryRoute: /v1/tokenize and
// /v1/detokenize) is chosen by the request's model and goes the way any /v1
// request does — the same replica pick, sleep and 503 answers, model rewrite
// and retries — but to the engine path the table names, and, like a probe
// (probe.go), it is not traffic: nothing accounts it, it is not in the
// in-flight table, it leaves no request record and does not move the
// deployment's last request time. A deployment can close the routes
// (store.Deployment.TokenizeDisabled); the route table carries the switch, so
// the check costs no store read.

func (p *Proxy) serveAuxiliary(w http.ResponseWriter, r *http.Request, route inferenceapi.AuxiliaryRoute) {
	if r.Method != route.Method {
		w.Header().Set("Allow", route.Method)
		writeErr(w, http.StatusMethodNotAllowed, fmt.Sprintf("%s only accepts %s", route.Path, route.Method))
		return
	}
	modelName, cleanup, err := extractModel(r, p.maxBodyBytes)
	defer cleanup()
	if err != nil {
		writeErr(w, statusForExtractError(err), err.Error())
		return
	}
	if modelName == "" {
		writeErr(w, http.StatusBadRequest, "request body must specify a model")
		return
	}
	dep, tgt, status, msg, _ := p.selectDeploymentInstanceWithKey(modelName, p.affinityKey(modelName, r))
	if status != 0 {
		writeErr(w, status, msg)
		return
	}
	if dep.TokenizeDisabled {
		p.releaseTarget(tgt)
		writeErr(w, http.StatusNotFound, fmt.Sprintf("deployment %s does not expose %s %s: tokenize is turned off for it", modelName, r.Method, route.Path))
		return
	}
	tgt.upstreamPath = route.Upstream
	r = r.WithContext(context.WithValue(r.Context(), probeKey, &probeTrace{}))
	p.serveSelectedTarget(w, r, modelName, tgt, dep.ID)
}
