package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Principal kinds a plugin route's caller has (pkg/extapi, Principal).
const (
	dynamicCallerSession = "session"
	dynamicCallerToken   = "token"
	dynamicCallerPublic  = "public"
)

// serveMatched serves a member or administrator route once its chain has
// authenticated the caller (and checked the token's scope and the role).
func (e *dynamicEndpoint) serveMatched(w http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(identityKey{}).(identity)
	c := DynamicCaller{Kind: dynamicCallerSession, UserID: id.claims.UserID, Username: id.claims.Username, Role: id.claims.Role,
		LocalAdmin: id.claims.LocalAdmin}
	if id.token != nil {
		c.Kind, c.APIKeyID, c.LocalAdmin = dynamicCallerToken, id.token.ID, false
	}
	e.serve(w, r, c)
}

// servePublic serves a public route without authentication, within its
// rate per client address.
func (e *dynamicEndpoint) servePublic(w http.ResponseWriter, r *http.Request) {
	if wait := e.limits.take(matchedFrom(r).route, e.a.loginClientIP(r), time.Now()); wait > 0 {
		retryAfter(w, int((wait+time.Second-1)/time.Second))
		writeProductError(w, http.StatusTooManyRequests, messages.ErrorTooManyRequests, "too many requests to this route; try again later", nil)
		return
	}
	e.serve(w, r, DynamicCaller{Kind: dynamicCallerPublic})
}

// authorizeDynamic decides whether caller may take route's action. Every
// authenticated caller may for now; it sits right after authentication,
// so a later authorization layer keeps a hidden feature's 404 ahead of a
// forbidden action's 403 as it is today.
func authorizeDynamic(DynamicCaller, *DynamicRoute) bool { return true }

// serve takes a matched, authenticated request through the rest of the
// pipeline: authorization, the feature gate, the assistant rule, the body
// cap, the audit line and the deadline, then forwards it.
func (e *dynamicEndpoint) serve(w http.ResponseWriter, r *http.Request, c DynamicCaller) {
	m := matchedFrom(r)
	route := m.route
	if !authorizeDynamic(c, route) {
		writeProductError(w, http.StatusForbidden, messages.ErrorForbidden, "not allowed to call this route", nil)
		return
	}
	if route.Feature != "" {
		access, ok := e.requireFeature(w, r, route)
		if !ok {
			return
		}
		c.Access = access.String()
	}
	if isAssistantRead(r) && !route.AIReadable {
		writeProductError(w, http.StatusNotFound, messages.ErrorNotFound, "the assistant does not read this route", nil)
		return
	}
	if r.ContentLength > route.MaxBody {
		writeProductError(w, http.StatusRequestEntityTooLarge, messages.ErrorRequestTooLarge, "the request body is larger than this route takes", map[string]any{"limit": route.MaxBody})
		return
	}
	status := 0
	if write := !isReadMethod(r.Method); write {
		start := time.Now()
		defer func() { e.audit(r, route, c, &status, start) }()
	}
	// A stream's timeout bounds the wait for its headers (the plugin host
	// holds it to that); its body runs until it goes idle.
	ctx, cancel := context.WithTimeout(r.Context(), route.Timeout)
	if route.Stream {
		cancel()
		ctx, cancel = context.WithCancel(r.Context())
	}
	defer cancel()
	in := r.Body
	if in == nil {
		in = http.NoBody
	}
	body := &cappedBody{r: in, left: route.MaxBody}
	out, err := forwardRequest(ctx, r, m, body, e.a.sessionCookie())
	if err != nil {
		status = http.StatusBadRequest
		writeProductError(w, status, messages.ErrorPluginRoutePathInvalid, "the request path is not in canonical form", nil)
		return
	}
	c.RequestID = middleware.GetReqID(r.Context())
	resp, err := e.ext.Forward(ctx, route, out, c)
	status = e.respond(w, r, route, resp, err, body, cancel)
}

// requireFeature applies route's feature: a read passes while it is full
// or read-only, a write only while it is full (or read-only for a write
// that only removes or relaxes). Hidden answers 404, any other refusal
// 403.
func (e *dynamicEndpoint) requireFeature(w http.ResponseWriter, r *http.Request, route *DynamicRoute) (featuregate.Access, bool) {
	gate := e.a.d.Extensions.Features
	if gate == nil {
		e.a.routingError(http.StatusNotFound)(w, r)
		return featuregate.Hidden, false
	}
	access, refusal := gate.Require(route.Feature, !isReadMethod(r.Method) && !route.WriteWhenReadOnly)
	if refusal != nil {
		status := http.StatusForbidden
		if access == featuregate.Hidden {
			status = http.StatusNotFound
		}
		writeErr(w, status, refusal)
		return access, false
	}
	return access, true
}

func isReadMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// audit records a plugin route's write: who called which route and how it
// ended.
func (e *dynamicEndpoint) audit(r *http.Request, route *DynamicRoute, c DynamicCaller, status *int, start time.Time) {
	log := e.a.d.Log
	if log == nil {
		return
	}
	log.Info("plugin route", "audit", true, "plugin", route.Plugin, "version", route.Version, "action", route.Action,
		"method", r.Method, "route", route.Pattern, "caller", c.Kind, "user", c.Username, "user_id", c.UserID,
		"status", *status, "request_id", middleware.GetReqID(r.Context()), "ms", time.Since(start).Milliseconds())
}

// forwardRequest is the request to hand the plugin: the route path below
// the plugin's prefix (the plugin host adds the serve connection's route
// prefix), the query, the capped body and the headers a client may send
// a plugin.
func forwardRequest(ctx context.Context, r *http.Request, m *matchedRoute, body io.ReadCloser, sessionCookie string) (*http.Request, error) {
	_, path, _ := strings.Cut(m.rest[1:], "/")
	u, err := url.Parse("/" + path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(ctx, r.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	out.ContentLength = r.ContentLength
	if r.ContentLength == 0 {
		out.Body = http.NoBody
	}
	out.Header = forwardHeaders(r, m.route, sessionCookie)
	return out, nil
}

// hopByHop are the headers that describe one connection, never forwarded.
var hopByHop = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Host"}

// forwardHeaders are r's headers without the ones only the server may set
// (Gwx-*, X-Gpuworks-*), the client's credentials (Authorization, and
// Cookie but for a cookies route's own cookies), the connection's own
// headers and a trace context the server does not take; a fresh
// traceparent replaces an invalid or missing one.
func forwardHeaders(r *http.Request, route *DynamicRoute, sessionCookie string) http.Header {
	out := make(http.Header, len(r.Header)+1)
	drop := map[string]bool{}
	for _, v := range r.Header.Values("Connection") {
		for f := range strings.SplitSeq(v, ",") {
			drop[http.CanonicalHeaderKey(strings.TrimSpace(f))] = true
		}
	}
	for k, v := range r.Header {
		lower := strings.ToLower(k)
		switch {
		case strings.HasPrefix(lower, "gwx-"), strings.HasPrefix(lower, "x-gpuworks-"), lower == "authorization", lower == "cookie",
			lower == "traceparent", lower == "tracestate", drop[k], slices.Contains(hopByHop, k):
			continue
		}
		out[k] = slices.Clone(v)
	}
	if tp := r.Header.Get("Traceparent"); validTraceparent(tp) {
		out.Set("Traceparent", tp)
		if ts := r.Header.Get("Tracestate"); ts != "" {
			out.Set("Tracestate", ts)
		}
	} else {
		out.Set("Traceparent", newTraceparent())
	}
	if route.Cookies {
		var kept []string
		for _, c := range r.Cookies() {
			if !coreCookie(c.Name, sessionCookie) && !hasPrefixFold(c.Name, "__Host-") {
				kept = append(kept, c.Name+"="+c.Value)
			}
		}
		if len(kept) > 0 {
			out.Set("Cookie", strings.Join(kept, "; "))
		}
	}
	return out
}

// coreCookie reports a cookie name the core uses or may use: the session
// cookie and every gpuworks-prefixed name.
func coreCookie(name, sessionCookie string) bool {
	return name == sessionCookie || hasPrefixFold(name, "gpuworks")
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

var traceparentPattern = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

func validTraceparent(tp string) bool {
	return traceparentPattern.MatchString(tp) && tp[3:35] != strings.Repeat("0", 32) && tp[36:52] != strings.Repeat("0", 16)
}

func newTraceparent() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "00-" + hex.EncodeToString(b[:16]) + "-" + hex.EncodeToString(b[16:]) + "-01"
}

// errBodyTooLarge ends a forwarded body that outgrew its route's limit.
var errBodyTooLarge = errors.New("request body larger than the route takes")

// cappedBody passes at most left bytes of a request body on, and records
// whether the client sent more.
type cappedBody struct {
	r    io.ReadCloser
	left int64
	over atomic.Bool
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		if n, err := b.r.Read(one[:]); n > 0 {
			b.over.Store(true)
			return 0, errBodyTooLarge
		} else if err != nil {
			return 0, err
		}
		return 0, nil
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.r.Close() }
