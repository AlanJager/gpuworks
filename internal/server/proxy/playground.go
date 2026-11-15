package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// PlaygroundHandler routes deployed models through the normal internal data
// plane and namespaced external models through encrypted AI endpoint configs.
// Callers must mount it below management JWT authentication.
func (p *Proxy) PlaygroundHandler(box *auth.Secretbox) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The management API hands over the signed-in user's claims; the
		// request record attributes the request to them.
		caller := uxrecord.Caller{Source: uxrecord.SourcePlayground, ReceivedMS: time.Now().UnixMilli()}
		claims, signedIn := auth.ClaimsFrom(r.Context())
		if signedIn {
			caller.UserID = claims.UserID
		}
		// Any class may be named here because only an administrator may bind
		// a key to any class; anyone else runs at the default.
		if !signedIn || claims.Role != "admin" {
			r.Header.Del(priorityClassHeader)
		}
		r = r.WithContext(uxrecord.WithCaller(r.Context(), caller))
		modelName, cleanup, err := extractModel(r, p.maxBodyBytes)
		defer cleanup()
		if err != nil {
			writeErr(w, statusForExtractError(err), err.Error())
			return
		}
		if strings.HasPrefix(modelName, "external:") {
			p.serveExternal(w, r, modelName, box)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v1/playground")
		// A class that does not exist is refused before a deployment is
		// chosen or woken, as on /v1.
		r, ok := p.decidePriority(w, r)
		if !ok {
			return
		}
		// A pinned refusal for sleep is the same event the load balancing path
		// accounts as asleep_503, not a generic 5xx.
		deployment, tgt, pinned, status, message, asleep := p.pinnedTarget(r, modelName)
		if !pinned {
			affinity := p.affinityKey(modelName, r)
			deployment, tgt, status, message, asleep = p.selectForRequest(r.Context(), modelName, affinity)
			if status != 0 && asleep {
				deployment, tgt, status, message, asleep = p.holdForWake(r.Context(), w, modelName, affinity, deployment)
			}
		}
		if status != 0 {
			p.acct.RecordImmediate(deploymentIDOrZero(deployment), status, asleep)
			writeErr(w, status, message)
			return
		}
		contract := inferenceapi.ResolveDeploymentContract(p.store, deployment)
		if !inferenceapi.AllowsContractRequest(contract, r.Method, r.URL.Path) {
			p.acct.RecordImmediate(deployment.ID, http.StatusNotFound, false)
			writeErr(w, http.StatusNotFound, fmt.Sprintf("deployment %s does not expose %s %s", modelName, r.Method, r.URL.Path))
			return
		}
		p.serveSelectedTarget(w, r, modelName, tgt, deployment.ID)
	}
}

// deploymentIDOrZero returns dep.ID, or 0 when dep is nil (a model that never
// resolved to any deployment at all — accounting attributes that under
// deployment 0, per the proxy metrics contract).
func deploymentIDOrZero(dep *store.Deployment) uint {
	if dep == nil {
		return 0
	}
	return dep.ID
}

func (p *Proxy) serveExternal(w http.ResponseWriter, r *http.Request, selected string, box *auth.Secretbox) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/playground/v1")
	if tail != "/chat/completions" {
		writeErr(w, http.StatusNotFound, "external playground models only support chat completions")
		return
	}
	parts := strings.SplitN(selected, ":", 3)
	if len(parts) != 3 {
		writeErr(w, http.StatusBadRequest, "invalid external playground model")
		return
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid external playground model")
		return
	}
	configs, err := p.store.ListAIConfigs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var config *store.AIConfig
	for i := range configs {
		if uint64(configs[i].ID) == id {
			config = &configs[i]
			break
		}
	}
	if config == nil {
		writeErr(w, http.StatusNotFound, "external playground model no longer exists")
		return
	}
	key, err := store.DecryptAIKey(box, config.APIKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "decrypt external endpoint key: "+err.Error())
		return
	}
	base, err := url.Parse(strings.TrimRight(config.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		writeErr(w, http.StatusBadRequest, "external endpoint has an invalid base URL")
		return
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "external playground requests must be JSON: "+err.Error())
		return
	}
	body["model"] = config.Model
	encoded, err := json.Marshal(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.ContentLength = int64(len(encoded))
	rp := &httputil.ReverseProxy{
		Director: func(out *http.Request) {
			out.URL.Scheme = base.Scheme
			out.URL.Host = base.Host
			out.URL.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(tail, "/")
			out.Host = base.Host
			stripBrowserContext(out.Header)
			// The class is the gateway's own; an outside provider has no use for it.
			out.Header.Del(priorityClassHeader)
			out.Header.Set("Authorization", "Bearer "+key)
			out.Header.Set("Content-Type", "application/json")
		},
		FlushInterval:  -1,
		ModifyResponse: boundUpstreamError,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			p.log.Warn("external playground upstream error", "err", err)
			writeErr(w, http.StatusBadGateway, "external upstream error: "+err.Error())
		},
	}
	rp.ServeHTTP(w, r)
}
