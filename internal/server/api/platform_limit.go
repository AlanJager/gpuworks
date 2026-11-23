package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// PlatformLimiter answers how far the whole server is available right now.
// A separately built server registers one (app.Host.LimitPlatform); the
// community server has none and never limits itself.
//
// It runs on every request, so it answers from a snapshot it swaps
// atomically: one atomic load, no I/O, no lock and no allocation.
type PlatformLimiter interface {
	PlatformLimit() PlatformLimit
}

// PlatformLimit is a PlatformLimiter's answer.
type PlatformLimit struct {
	// Access is featuregate.Full (nothing is limited), ReadOnly (reads and
	// the data plane are served, changes are refused) or Locked (only the
	// recovery surface is served). Hidden is not an answer for a whole
	// server and reads as Locked, so the zero value refuses.
	Access featuregate.Access
	// Notice is the refusal to return instead of the core's own, built
	// when the snapshot changed; nil uses the core's.
	Notice *messages.ProductError
	// Link is the console page where an administrator lifts the limit;
	// the console sends every other page there while Locked. Empty for
	// none.
	Link string
	// Action names what Link does, for the control that opens it; nil
	// leaves the console's own wording.
	Action *messages.ProductError
}

// platformClass is what a limit does to one route. The zero value is the
// strictest class, so a route nobody classified is refused under any limit.
type platformClass uint8

const (
	// platformChange changes the server or brings something new into it:
	// refused while ReadOnly and while Locked.
	platformChange platformClass = iota
	// platformRead only reads: served while ReadOnly.
	platformRead
	// platformServe is the data plane that running work answers: served
	// while ReadOnly, so a limit never interrupts what already runs.
	platformServe
	// platformRecovery is served under any limit: signing in, health,
	// what machines that already joined need, and what an administrator
	// needs to lift the limit or to repair the server.
	platformRecovery
)

// platformRecoveryRoutes are served under any limit, by method and route
// pattern.
var platformRecoveryRoutes = map[string]bool{
	"GET /healthz":             true,
	"GET /api/v1/readyz":       true,
	"GET /api/v1/healthz":      true,
	"POST /api/v1/auth/login":  true,
	"POST /api/v1/auth/logout": true,
	"GET /api/v1/auth/me":      true,
	// The session of a signed-in administrator stays alive, and a password
	// can be changed, while the server is limited.
	"POST /api/v1/auth/activity": true,
	"POST /api/v1/auth/password": true,
	// The sign-in page and the console shell: without them nobody reaches
	// the page that lifts the limit.
	"GET /api/v1/branding":          true,
	"POST /api/v1/updates/prefetch": true,
	"GET /api/v1/console/manifest":  true,
	// Machines that already joined keep their connection and their
	// health check; a limit never interrupts running work.
	"GET /api/v1/agent/connect":    true,
	"POST /api/v1/slurm/gpu-nodes": true,
	"GET /slurm/check.sh":          true,
	"GET /slurm/token-status":      true,
}

// platformRecoveryPrefixes are subtrees served under any limit: the
// installed packages' management (the local administrator's floor), the
// server's own upgrade, and what support asks for.
var platformRecoveryPrefixes = []string{
	"/api/v1/plugins",
	"/api/v1/updates",
	"/api/v1/diagnostics/",
}

// platformChangingReads are GET routes that hand out what brings a new
// machine in, or open an interactive session: refused like a change.
var platformChangingReads = map[string]bool{
	"GET /install.sh":                           true,
	"GET /kube/manifest.yaml":                   true,
	"GET /slurm/setup.sh":                       true,
	"GET /api/v1/kube/clusters/{uuid}/manifest": true,
	"GET /api/v1/slurm/clusters/{uuid}/setup":   true,
	"GET /api/v1/slurm/clusters/{uuid}/script":  true,
	"GET /api/v1/nodes/{uuid}/terminal":         true,
}

// platformReadingPosts are POST routes that only read.
var platformReadingPosts = map[string]bool{
	// MCP serves read-only tools.
	"POST /api/v1/mcp": true,
}

// platformExtensionRoot is where every extension's routes live; the core
// registers nothing under it but the mount that serves installed packages.
const platformExtensionRoot = "/api/v1" + extensionRoot + "/"

// classifyPlatformRoute is the class of a request for method that matched
// the route pattern at path. owner is the extension whose own namespace
// stays served under any limit (the one that registered the limiter). The
// namespace is read from the path only for a route under the extension
// root, where the first segment is the namespace whichever route answers:
// a compiled route, or the packages' mount answering 404 or 405 for it.
// classified is false for a route no rule names: it is refused under any
// limit, and the route inventory test fails until it is classified.
func classifyPlatformRoute(method, pattern, path, owner string) (class platformClass, classified bool) {
	key := method + " " + pattern
	if platformRecoveryRoutes[key] {
		return platformRecovery, true
	}
	for _, prefix := range platformRecoveryPrefixes {
		if pattern == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(pattern, strings.TrimSuffix(prefix, "/")+"/") {
			return platformRecovery, true
		}
	}
	if owner != "" && strings.HasPrefix(pattern, platformExtensionRoot) && strings.HasPrefix(path, platformExtensionRoot+owner+"/") {
		return platformRecovery, true
	}
	if platformChangingReads[key] {
		return platformChange, true
	}
	if platformReadingPosts[key] {
		return platformRead, true
	}
	if pattern == "/v1/*" {
		return platformServe, true
	}
	if !strings.HasPrefix(pattern, "/api/v1/") {
		return platformChange, false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return platformRead, true
	}
	return platformChange, true
}

// platformRefusals are the core's own refusals, built once.
var (
	platformLockedRefusal = messages.NewProductError(messages.ErrorPlatformLocked,
		"this server is locked; only signing in and the page that unlocks it are served", nil)
	platformReadOnlyRefusal = messages.NewProductError(messages.ErrorPlatformReadOnly,
		"this server is read-only; what runs keeps running and can be viewed, changes are refused", nil)
)

// allows reports whether a route of class is served under access.
func (c platformClass) allows(access featuregate.Access) bool {
	switch access {
	case featuregate.Full:
		return true
	case featuregate.ReadOnly:
		return c != platformChange
	}
	return c == platformRecovery
}

// platformRefusal is what a refused request is answered with under limit.
func platformRefusal(limit PlatformLimit) *messages.ProductError {
	if limit.Notice != nil {
		return limit.Notice
	}
	if limit.Access == featuregate.ReadOnly {
		return platformReadOnlyRefusal
	}
	return platformLockedRefusal
}

// platformGuard refuses, before authentication and before any handler, what
// the server's limit does not serve. Without a limiter it adds nothing, and
// under a limiter that answers Full it costs that one call.
//
// It resolves the route the request would reach in routes (the router it is
// installed on), so the class comes from the route's pattern, never from a
// guess at the path. A request no route matches goes on to the router's
// 404 or 405.
func (a *apiHandlers) platformGuard(routes chi.Routes) func(http.Handler) http.Handler {
	limiter := a.d.Extensions.Platform
	if limiter == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	owner := a.d.Extensions.PlatformOwner
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit := limiter.PlatformLimit()
			if limit.Access == featuregate.Full {
				next.ServeHTTP(w, r)
				return
			}
			// The path the router itself routes on.
			path := r.URL.RawPath
			if path == "" {
				path = r.URL.Path
			}
			if path == "" {
				path = "/"
			}
			pattern := routes.Find(chi.NewRouteContext(), r.Method, path)
			if pattern == "" {
				next.ServeHTTP(w, r)
				return
			}
			class, _ := classifyPlatformRoute(r.Method, pattern, path, owner)
			if class.allows(limit.Access) {
				next.ServeHTTP(w, r)
				return
			}
			writeErr(w, http.StatusForbidden, platformRefusal(limit))
		})
	}
}

// consolePlatform is the limit as the console manifest shows it; nil while
// nothing is limited, so the community manifest is unchanged.
func (a *apiHandlers) consolePlatform() *PlatformLimit {
	limiter := a.d.Extensions.Platform
	if limiter == nil {
		return nil
	}
	limit := limiter.PlatformLimit()
	if limit.Access == featuregate.Full {
		return nil
	}
	if limit.Access != featuregate.ReadOnly {
		limit.Access = featuregate.Locked
	}
	if limit.Notice == nil {
		limit.Notice = platformRefusal(limit)
	}
	return &limit
}
