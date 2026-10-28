package experience

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The environment is for showing the product, so the guard in front of its
// console lets a visitor change only what lives in its own database:
// deployments of the preloaded models, models registered from the offline
// hub, evaluations, API keys and the like. Anything that would act on the
// host the environment runs on, reach a real cloud, network, registry or
// storage, or change the console itself is refused with
// EXPERIENCE_UNSUPPORTED; reading stays open, except where reading itself
// reaches the host (a terminal, file browsing).

// allowedWrites are the mutating routes a visitor may call, as
// "METHOD /api/v1/<pattern>", where * matches one path segment and a
// trailing /** any rest.
var allowedWrites = []string{
	"POST /api/v1/auth/login",
	"POST /api/v1/auth/logout",
	"POST /api/v1/auth/activity",
	"POST /api/v1/notices/read",
	"POST /api/v1/apikeys",
	"DELETE /api/v1/apikeys/*",

	"PATCH /api/v1/nodes/*/name",
	"PUT /api/v1/nodes/*/labels",
	"PUT /api/v1/nodes/*/scheduling",
	"POST /api/v1/nodes/*/evict",

	"POST /api/v1/models",
	"POST /api/v1/models/quick-register",
	"DELETE /api/v1/models/*",
	"PATCH /api/v1/models/*/inference-config",
	"POST /api/v1/models/*/metadata/refresh",

	"POST /api/v1/inference-templates/clone",
	"POST /api/v1/inference-templates/evaluate",

	"POST /api/v1/deployments/preflight",
	"POST /api/v1/deployments",
	"PUT /api/v1/deployments/*",
	"DELETE /api/v1/deployments/*",
	"POST /api/v1/deployments/*/clone",
	"POST /api/v1/deployments/*/stop",
	"POST /api/v1/deployments/*/start",
	"POST /api/v1/deployments/*/restart",
	"POST /api/v1/deployments/*/units/retry",
	"POST /api/v1/deployments/*/units/suggest",
	"PUT /api/v1/deployments/*/requirements",
	"PUT /api/v1/deployments/*/routing",
	"PUT /api/v1/deployments/*/health-check",
	"PUT /api/v1/deployments/*/tokenize",
	"PUT /api/v1/deployments/*/dgpu",
	"PUT /api/v1/deployments/*/pd",
	"PUT /api/v1/deployments/*/replicas",
	"PUT /api/v1/deployments/*/node-replacement",
	"PUT /api/v1/deployments/*/placement-mode",
	"POST /api/v1/instances/*/restart",

	"POST /api/v1/evaluate",
	// Evaluations of the environment's own deployments only (evaluable):
	// an external endpoint would make the console a load generator.
	"POST /api/v1/benchmarks",
	"DELETE /api/v1/benchmarks/*",
	"POST /api/v1/benchmarks/*/cancel",

	"POST /api/v1/playground/v1/**",
	"POST /api/v1/mcp",
}

// refusedReads are reads that reach the host or leave the environment.
var refusedReads = []string{
	"GET /api/v1/nodes/*/fs",
	"GET /api/v1/nodes/*/terminal",
	"GET /api/v1/nodes/*/terminal/settings",
	"GET /api/v1/terminal/sessions",
	"GET /api/v1/nodes/*/ssh",
	"GET /api/v1/ssh-key",
	"GET /api/v1/storages/*/files",
	"GET /api/v1/storages/*/files/download",
	"GET /api/v1/updates",
	"GET /api/v1/server/listen",
	"GET /install.sh",
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

func (g *guard) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.internal(r) {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Del(internalHeader)
		// An escaped path (%2F) is matched by the router on its raw form, which
		// the patterns below do not see; the console never needs one.
		if r.URL.RawPath != "" || !g.permitted(r) {
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

func isChat(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/api/v1/playground/")
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

func (g *guard) permitted(r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return !matchesAny(refusedReads, http.MethodGet, path)
	}
	if path == "/v1" || strings.HasPrefix(path, "/v1/") || path == "/api/v1/agent/connect" {
		return true // the gateway (API keys) and the simulated nodes' connections
	}
	return matchesAny(allowedWrites, r.Method, path)
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

func refuse(w http.ResponseWriter, code, fallback string, params map[string]any) {
	body := map[string]any{"error": fallback, "code": code}
	if len(params) > 0 {
		body["params"] = params
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(body)
}

func matchesAny(patterns []string, method, path string) bool {
	for _, p := range patterns {
		m, pattern, _ := strings.Cut(p, " ")
		if m == method && matchPath(pattern, path) {
			return true
		}
	}
	return false
}

func matchPath(pattern, path string) bool {
	ps, xs := strings.Split(pattern, "/"), strings.Split(path, "/")
	for i, seg := range ps {
		if seg == "**" {
			return len(xs) > i
		}
		if i >= len(xs) || (seg != "*" && seg != xs[i]) || (seg == "*" && xs[i] == "") {
			return false
		}
	}
	return len(ps) == len(xs)
}
