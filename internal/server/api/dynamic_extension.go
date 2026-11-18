package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// DynamicExtension serves the routes of installed plugin packages under
// /api/v1/x/<plugin id>/ from a route table that changes while the server
// runs (docs/dev/extensions.md, 插件路由). The server's plugin host fills
// it; the API checks every request against the core's rules and passes on
// only what passes. A nil Match mounts nothing.
type DynamicExtension struct {
	// Match finds the route for method and path, the request path below
	// /api/v1/x in canonical form ("/<id><route path>"). It takes no lock
	// and answers from the table current now.
	Match func(method, path string) (*DynamicRoute, DynamicMatch)
	// Routes are every route of the table current now, for the route
	// inventory and the assistant's route list.
	Routes func() []DynamicRoute
	// Forward sends req (path PathRoutePrefix plus the route path, headers
	// already stripped) to route's plugin as caller, signed. A refusal is a
	// *messages.ProductError: PLUGIN_UNAVAILABLE, PLUGIN_BUSY,
	// PLUGIN_TIMEOUT or PLUGIN_ROUTE_FAILED.
	Forward func(ctx context.Context, route *DynamicRoute, req *http.Request, caller DynamicCaller) (*http.Response, error)
	// DecodeError validates the body of a response marked
	// Gwx-Product-Error and returns the product error it carries.
	DecodeError func(body []byte) (*messages.ProductError, error)
	// StreamIdle cuts a stream route's response off when the plugin writes
	// nothing for this long; zero is a minute.
	StreamIdle time.Duration
}

// DynamicMatch says how a request path matched the route table.
type DynamicMatch int

const (
	// DynamicMiss: no route of any method has this path.
	DynamicMiss DynamicMatch = iota
	// DynamicFound: a route has this method and path.
	DynamicFound
	// DynamicMethodNotAllowed: routes have this path, none this method.
	DynamicMethodNotAllowed
)

// DynamicAccess is who may call a plugin route.
type DynamicAccess uint8

const (
	// DynamicAdmin: administrators, by session or platform token (a read
	// token still only reads).
	DynamicAdmin DynamicAccess = iota
	// DynamicAdminSession: administrators signed in with a session.
	DynamicAdminSession
	// DynamicMember: any signed-in user and any platform token; a read
	// token only reads.
	DynamicMember
	// DynamicPublic: anyone, without signing in, rate-limited per client
	// address.
	DynamicPublic
)

func (a DynamicAccess) String() string {
	switch a {
	case DynamicAdminSession:
		return "admin_session"
	case DynamicMember:
		return "member"
	case DynamicPublic:
		return "public"
	}
	return "admin"
}

// DynamicRoute is one route a plugin package declares, with its limits'
// defaults filled in.
type DynamicRoute struct {
	Plugin, Version string
	Method          string
	// Pattern is the declared pattern, relative to /api/v1/x/<Plugin>.
	Pattern string
	Action  string
	Access  DynamicAccess
	// Feature gates the route; empty for an ungated route.
	Feature           featuregate.Key
	WriteWhenReadOnly bool
	AIReadable        bool
	Stream            bool
	HTML              bool
	Cookies           bool
	MaxBody           int64
	// Timeout bounds the request; on a stream route, the wait for the
	// response headers.
	Timeout time.Duration
	// RatePerMinute and RateBurst bound a public route per client
	// address; zero takes the default.
	RatePerMinute, RateBurst int
	// Ref is the plugin host's own handle on the route, for Forward.
	Ref any
}

// prefix is the path every response of the route stays under.
func (r *DynamicRoute) prefix() string { return "/api/v1" + extensionRoot + "/" + r.Plugin + "/" }

// inventoryLine is the route's line in RouteInventory.
func (r *DynamicRoute) inventoryLine() string {
	gate := "[feature=" + string(r.Feature) + "]"
	if r.Feature == "" {
		gate = "[ungated]"
	}
	return r.Method + " /api/v1" + extensionRoot + "/" + r.Plugin + r.Pattern + " [access=" + r.Access.String() + "] [action=" + r.Action + "] " +
		gate + " [x=" + r.Plugin + "@" + r.Version + "]"
}

// DynamicCaller is who calls a plugin route, as the API authenticated it.
type DynamicCaller struct {
	// Kind is session, token or public.
	Kind     string
	UserID   uint
	Username string
	Role     string
	// LocalAdmin is the effective break-glass flag (auth.Claims).
	LocalAdmin bool
	// APIKeyID is the platform token's id for kind token.
	APIKeyID  uint
	RequestID string
	// Access is the route's feature access now (full or read_only);
	// empty for an ungated route.
	Access string
}

// dynamicEndpoint is the one handler under /api/v1/x/* serving plugin
// routes; the route inventory finds the dynamic routes through it.
type dynamicEndpoint struct {
	a   *apiHandlers
	ext *DynamicExtension
	// compiled probes the compiled extensions' routes, so a request in a
	// compiled namespace that chi handed here answers the core 404 or
	// 405 rather than reaching a plugin.
	compiled chi.Router
	names    map[string]bool
	// chains authenticate a matched route by access level, then serve it.
	chains [DynamicPublic + 1]http.Handler
	limits *publicLimiter
}

// mountDynamic registers the plugin route handler after every compiled
// route, so a compiled namespace keeps priority.
func (a *apiHandlers) mountDynamic(r chi.Router) {
	ext := a.d.Extensions.Dynamic
	if ext == nil || ext.Match == nil {
		return
	}
	e := &dynamicEndpoint{a: a, ext: ext, compiled: chi.NewRouter(), names: map[string]bool{}, limits: newPublicLimiter(publicLimiterCapacity)}
	for _, x := range a.d.Extensions.Extensions {
		e.names[x.Name()] = true
	}
	if routes, ok := r.(chi.Routes); ok {
		_ = chi.Walk(routes, func(method, route string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
			if _, ok := h.(*extensionEndpoint); ok {
				e.compiled.Method(method, route, http.NotFoundHandler())
			}
			return nil
		})
	}
	serve := http.HandlerFunc(e.serveMatched)
	e.chains[DynamicAdmin] = a.managementAuth(readTokensRead(a.managementAdmin(serve)))
	e.chains[DynamicAdminSession] = a.managementAuth(sessionOnly(a.managementAdmin(serve)))
	e.chains[DynamicMember] = a.managementAuth(readTokensRead(serve))
	e.chains[DynamicPublic] = http.HandlerFunc(e.servePublic)
	r.Handle(extensionRoot+"/*", e)
	a.dynamic = e
}

type dynamicRouteKey struct{}

func (e *dynamicEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	rest, ok := strings.CutPrefix(path, "/api/v1"+extensionRoot)
	if !ok || !canonicalRoutePath(rest) {
		writeProductError(w, http.StatusBadRequest, messages.ErrorPluginRoutePathInvalid, "the request path is not in canonical form", nil)
		return
	}
	if id, _, _ := strings.Cut(rest[1:], "/"); e.names[id] {
		e.compiledMiss(w, r, extensionRoot+rest)
		return
	}
	route, match := e.ext.Match(r.Method, rest)
	switch match {
	case DynamicFound:
	case DynamicMethodNotAllowed:
		e.a.routingError(http.StatusMethodNotAllowed)(w, r)
		return
	default:
		e.a.routingError(http.StatusNotFound)(w, r)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), dynamicRouteKey{}, &matchedRoute{route: route, rest: rest}))
	e.chains[route.Access].ServeHTTP(w, r)
}

// compiledMiss answers a request in a compiled extension's namespace that
// no compiled route served: the core 405 when another method has the
// path, else the core 404.
func (e *dynamicEndpoint) compiledMiss(w http.ResponseWriter, r *http.Request, path string) {
	for _, m := range [...]string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if m != r.Method && e.compiled.Match(chi.NewRouteContext(), m, path) {
			e.a.routingError(http.StatusMethodNotAllowed)(w, r)
			return
		}
	}
	e.a.routingError(http.StatusNotFound)(w, r)
}

// matchedRoute is the route a request matched and its path below
// /api/v1/x/<id>.
type matchedRoute struct {
	route *DynamicRoute
	rest  string
}

func matchedFrom(r *http.Request) *matchedRoute {
	m, _ := r.Context().Value(dynamicRouteKey{}).(*matchedRoute)
	return m
}

// canonicalRoutePath reports whether path, the raw request path below
// /api/v1/x, is canonical: "/<id>/..." with no empty segment but a
// trailing one, no "." or ".." segment, no backslash and no escaped "/",
// backslash, "." or "%". The route table matches it as it is, and the
// plugin receives the same string.
func canonicalRoutePath(path string) bool {
	if len(path) < 2 || path[0] != '/' || strings.ContainsRune(path, '\\') {
		return false
	}
	for i := 0; i+2 < len(path); i++ {
		if path[i] != '%' {
			continue
		}
		switch strings.ToUpper(path[i+1 : i+3]) {
		case "2F", "5C", "2E", "25":
			return false
		}
	}
	segs := strings.Split(path[1:], "/")
	for i, s := range segs {
		if s == "." || s == ".." || (s == "" && i != len(segs)-1) {
			return false
		}
	}
	return segs[0] != ""
}

// dynamicInventory is the route inventory's lines for the plugin routes
// installed now.
func (e *dynamicEndpoint) inventory() []string {
	if e.ext.Routes == nil {
		return nil
	}
	routes := e.ext.Routes()
	out := make([]string, len(routes))
	for i := range routes {
		out[i] = routes[i].inventoryLine()
	}
	return out
}

// aiRoutes are the GET plugin routes the assistant may read now.
func (e *dynamicEndpoint) aiRoutes() []string {
	if e.ext.Routes == nil {
		return nil
	}
	var out []string
	for _, r := range e.ext.Routes() {
		if r.AIReadable && r.Method == http.MethodGet {
			out = append(out, "/api/v1"+extensionRoot+"/"+r.Plugin+r.Pattern)
		}
	}
	return out
}

// retryAfter sets Retry-After to seconds.
func retryAfter(w http.ResponseWriter, seconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}
