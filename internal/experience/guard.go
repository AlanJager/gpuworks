package experience

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The environment is for showing the product, so the guard in front of its
// console lets a visitor change only what lives in its own database:
// deployments of the preloaded models, models registered from the offline
// hub, evaluations, API keys and the like. Anything that would act on the
// host the environment runs on, reach a real cloud, network, registry or
// storage, or change the console itself is refused with
// EXPERIENCE_UNSUPPORTED.
//
// The guard judges a request by the route the console's router matches it
// to, never by its own reading of the path. Writes are refused unless listed
// in allowedWrites. Reads are open except the ones the AI assistant may not
// read either (aihelper.DeniedRoutes: terminals, file browsing, public
// registries and the like) and the few in refusedReads; a read route the
// platform marks sensitive tomorrow is refused here without anyone editing
// this file, and openReads lets the console's own pages through.

// allowedWrites are the mutating routes a visitor may call, as
// "METHOD <route pattern>" exactly as the console's router names it
// (TestGuardPatternsNameConsoleRoutes).
var allowedWrites = map[string]bool{
	"POST /api/v1/auth/login":         true,
	"POST /api/v1/auth/logout":        true,
	"POST /api/v1/auth/activity":      true,
	"POST /api/v1/notices/read":       true,
	"POST /api/v1/apikeys":            true,
	"DELETE /api/v1/apikeys/{prefix}": true,

	"PATCH /api/v1/nodes/{uuid}/name":     true,
	"PUT /api/v1/nodes/{uuid}/labels":     true,
	"PUT /api/v1/nodes/{uuid}/scheduling": true,
	"POST /api/v1/nodes/{uuid}/evict":     true,

	"POST /api/v1/models":                        true,
	"POST /api/v1/models/quick-register":         true,
	"DELETE /api/v1/models/{id}":                 true,
	"PATCH /api/v1/models/{id}":                  true,
	"PATCH /api/v1/models/{id}/inference-config": true,
	"POST /api/v1/models/{id}/metadata/refresh":  true,
	"POST /api/v1/inference-templates/clone":     true,
	"POST /api/v1/inference-templates/evaluate":  true,

	"POST /api/v1/deployments/preflight":            true,
	"POST /api/v1/deployments":                      true, // preloaded models only (deployable)
	"PUT /api/v1/deployments/{id}":                  true,
	"DELETE /api/v1/deployments/{id}":               true,
	"POST /api/v1/deployments/{id}/clone":           true,
	"POST /api/v1/deployments/{id}/stop":            true,
	"POST /api/v1/deployments/{id}/start":           true,
	"POST /api/v1/deployments/{id}/restart":         true,
	"POST /api/v1/deployments/{id}/units/retry":     true,
	"POST /api/v1/deployments/{id}/units/suggest":   true,
	"PUT /api/v1/deployments/{id}/requirements":     true,
	"PUT /api/v1/deployments/{id}/routing":          true,
	"PUT /api/v1/deployments/{id}/health-check":     true,
	"PUT /api/v1/deployments/{id}/tokenize":         true,
	"PUT /api/v1/deployments/{id}/dgpu":             true,
	"PUT /api/v1/deployments/{id}/pd":               true,
	"PUT /api/v1/deployments/{id}/replicas":         true,
	"PUT /api/v1/deployments/{id}/node-replacement": true,
	"PUT /api/v1/deployments/{id}/placement-mode":   true,
	"POST /api/v1/instances/{uuid}/restart":         true,

	"POST /api/v1/evaluate": true,
	// Evaluations of the environment's own deployments only (evaluable):
	// an external endpoint would make the console a load generator.
	"POST /api/v1/benchmarks":             true,
	"DELETE /api/v1/benchmarks/{id}":      true,
	"POST /api/v1/benchmarks/{id}/cancel": true,

	"POST /api/v1/playground/v1/*": true,
}

// openReads are the reads the AI assistant may not make (for its own
// reasons: a stream, an image, a secret it must not repeat) that the
// console's pages need and that stay inside the environment
// (TestGuardOpenReadsAreAssistantDenied keeps each one a denied route).
var openReads = map[string]string{
	"/api/v1/agent/connect":                       "the simulated nodes' connections",
	"/api/v1/providers/{id}":                      "the seeded accounts' edit form",
	"/api/v1/ai/configs":                          "the assistant settings page",
	"/api/v1/apikeys":                             "the API key page",
	"/api/v1/experience":                          "answers 404 inside the environment",
	"/api/v1/deployments/{id}/draft":              "the deploy form",
	"/api/v1/branding":                            "the console's logo",
	"/api/v1/platform-tokens":                     "the platform token page",
	"/api/v1/events":                              "the console's live updates",
	"/api/v1/instances/{uuid}/logs":               "the simulated engines' logs",
	"/api/v1/inference-templates/export/{digest}": "a template archive built from the environment's own data",
	"/api/v1/models/{id}/logo":                    "model logos",
	"/api/v1/playground/openapi.json":             "the playground's description of the environment's own endpoints",
	"/api/v1/pull/registry-credentials":           "answers that the setting moved",
}

// refusedReads are reads the assistant may make that reach the host or
// leave the environment.
var refusedReads = map[string]string{
	"/api/v1/nodes/{uuid}/terminal/settings": "the host terminal's settings",
	"/api/v1/updates":                        "asks the update channel",
	"/api/v1/server/listen":                  "the host's listen addresses",
	"/install.sh":                            "installs an agent on a real host",
}

// maxWriteBody bounds a visitor's write to the console; the environment
// lives in the server's data directory and a visitor must not fill its disk.
// Chat through the gateway and the playground is bounded by the gateway.
const maxWriteBody = 1 << 20

// guard refuses a visitor's unsupported operations. Requests carrying the
// environment's own secret (its seeding, client.go) pass.
type guard struct {
	secret string

	mu        sync.RWMutex
	preloaded map[uint]bool // model ids a deployment may use
	models    string        // their names, for the message
}

func newGuard(secret string) *guard { return &guard{secret: secret} }

// allowModels sets the models a deployment may use.
func (g *guard) allowModels(ids map[uint]bool, names []string) {
	g.mu.Lock()
	g.preloaded, g.models = ids, strings.Join(names, ", ")
	g.mu.Unlock()
}

// wrap guards next, the console's router, whose routes it judges requests
// by. A handler that is not a router has no routes to judge by, and every
// visitor's request is refused.
func (g *guard) wrap(next http.Handler) http.Handler {
	routes, _ := next.(routeFinder)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.internal(r) {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Del(internalHeader)
		// An escaped path (%2F) is routed on its raw form; the console never
		// needs one.
		if routes == nil || r.URL.RawPath != "" || !permitted(routes, r) {
			refuse(w, messages.ErrorExperienceUnsupported, "the experience environment is for showing the product and does not support this operation", nil)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !isChat(r.URL.Path) {
			r.Body = http.MaxBytesReader(w, r.Body, maxWriteBody)
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/deployments" && !g.deployable(w, r) {
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/benchmarks" && !evaluable(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// permitted reports whether a visitor may make request r, judged by the
// route the console's router matches it to.
func permitted(routes routeFinder, r *http.Request) bool {
	if isGateway(r.URL.Path) {
		return true // the OpenAI gateway, served beside the router, behind API keys
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		pattern := routes.Find(chi.NewRouteContext(), http.MethodGet, r.URL.Path)
		return pattern == "" || readable(pattern) // "": the console's pages and assets
	}
	pattern := routes.Find(chi.NewRouteContext(), r.Method, r.URL.Path)
	return pattern != "" && allowedWrites[r.Method+" "+pattern]
}

// routeFinder names the route a method and path match (chi.Routes.Find).
type routeFinder interface {
	Find(rctx *chi.Context, method, path string) string
}

// readable reports whether a visitor may read the route pattern.
func readable(pattern string) bool {
	if _, refused := refusedReads[pattern]; refused {
		return false
	}
	if _, open := openReads[pattern]; open {
		return true
	}
	_, denied := assistantDenied[pattern]
	return !denied
}

// assistantDenied are the GET routes the AI assistant may not read.
var assistantDenied = aihelper.DeniedRoutes()

func isGateway(path string) bool { return path == "/v1" || strings.HasPrefix(path, "/v1/") }

func isChat(path string) bool {
	return isGateway(path) || strings.HasPrefix(path, "/api/v1/playground/")
}

// evaluable lets an evaluation through only against a deployment of the
// environment, with the load sent from the console: a node's load would
// leave from the real host, and an external endpoint is anything.
func evaluable(w http.ResponseWriter, r *http.Request) bool {
	raw, ok := readBody(w, r)
	if !ok {
		return false
	}
	var body struct {
		TargetKind string `json:"target_kind"`
		LoadOrigin string `json:"load_origin"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.TargetKind == "instance" && (body.LoadOrigin == "" || body.LoadOrigin == "server") {
		return true
	}
	refuse(w, messages.ErrorExperienceUnsupported, "the experience environment evaluates its own deployments, with the load sent from the console", nil)
	return false
}

// readBody reads the request body and puts it back for the next handler.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		refuse(w, messages.ErrorExperienceUnsupported, "the request body is too large for the experience environment", nil)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return raw, true
}

func (g *guard) internal(r *http.Request) bool {
	got := r.Header.Get(internalHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(g.secret)) == 1
}

// deployable lets a deployment through only for a preloaded model: every
// other model has no copy on any node, and the environment downloads none.
func (g *guard) deployable(w http.ResponseWriter, r *http.Request) bool {
	raw, ok := readBody(w, r)
	if !ok {
		return false
	}
	var body struct {
		ModelID uint `json:"model_id"`
	}
	_ = json.Unmarshal(raw, &body)
	g.mu.RLock()
	ok, models := g.preloaded[body.ModelID], g.models
	g.mu.RUnlock()
	if !ok {
		refuse(w, messages.ErrorExperienceModelNotPreloaded, "the experience environment deploys only its preloaded models: "+models, map[string]any{"models": models})
	}
	return ok
}

// refuse answers 403 with a product error, in the console's own error shape.
func refuse(w http.ResponseWriter, code, fallback string, params map[string]any) {
	api.WriteError(w, http.StatusForbidden, messages.NewProductError(code, fallback, params))
}
