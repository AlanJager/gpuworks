package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// PluginProductErrorHeader marks a plugin route's response body as a
// product error (pkg/extapi, HeaderProductError).
const PluginProductErrorHeader = "Gwx-Product-Error"

// pluginRouteCSP is set on every plugin route response: nothing in it may
// run or load anything on the console's origin.
const pluginRouteCSP = "sandbox; default-src 'none'"

const (
	// maxPluginResponse bounds a response body that does not stream.
	maxPluginResponse = 64 << 20
	// maxPluginErrorBody bounds a product error's body.
	maxPluginErrorBody = 64 << 10
	// defaultStreamIdle cuts a stream off that writes nothing this long.
	defaultStreamIdle = time.Minute
)

// passedHeaders are the plugin's response headers the client may see;
// Location and Set-Cookie pass only checked (passLocation, passCookies).
var passedHeaders = []string{"Content-Type", "Content-Length", "Content-Encoding", "Content-Disposition", "Cache-Control", "ETag", "Last-Modified", "Retry-After"}

// respond answers a forwarded request: the call's refusal mapped to its
// status, or the plugin's response with only what may pass. It returns
// the status sent. A response that fails once its headers are sent
// aborts the connection rather than end as a truncated success.
func (e *dynamicEndpoint) respond(w http.ResponseWriter, r *http.Request, route *DynamicRoute, resp *http.Response, err error, body *cappedBody, cancel context.CancelFunc) int {
	if body.over.Load() {
		if resp != nil {
			_ = resp.Body.Close()
		}
		err = errBodyTooLarge
	}
	if err != nil {
		if body.over.Load() {
			writeProductError(w, http.StatusRequestEntityTooLarge, messages.ErrorRequestTooLarge, "the request body is larger than this route takes", map[string]any{"limit": route.MaxBody})
			return http.StatusRequestEntityTooLarge
		}
		return e.callRefused(w, r, err)
	}
	defer resp.Body.Close()
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", pluginRouteCSP)
	if resp.Header.Get(PluginProductErrorHeader) != "" {
		return e.passProductError(w, route, resp)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	if mt, _, _ := mime.ParseMediaType(ct); (mt == "text/html" || mt == "application/xhtml+xml") && !route.HTML {
		return invalidResponse(w, route, "the route does not declare html")
	}
	if !route.Stream && resp.ContentLength > maxPluginResponse {
		return invalidResponse(w, route, "the response is larger than the server passes on")
	}
	for _, k := range passedHeaders {
		if v := resp.Header.Values(k); len(v) > 0 {
			h[k] = v
		}
	}
	h.Set("Content-Type", ct)
	if loc := resp.Header.Get("Location"); loc != "" && locationInside(loc, route.prefix()) {
		h.Set("Location", loc)
	}
	if route.Access == DynamicPublic && route.Cookies {
		e.passCookies(h, route, resp.Header.Values("Set-Cookie"))
	}
	w.WriteHeader(resp.StatusCode)
	if route.Stream {
		e.copyStream(w, resp.Body, cancel)
		return resp.StatusCode
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxPluginResponse+1))
	if err != nil || n > maxPluginResponse || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		panic(http.ErrAbortHandler)
	}
	return resp.StatusCode
}

// callRefused maps a refused call: the plugin was not reached (503, retry
// in 5 s), was busy (503, retry in 1 s), timed out (504), failed after it
// had the request (502, no retry), or is gone (the core 404).
func (e *dynamicEndpoint) callRefused(w http.ResponseWriter, r *http.Request, err error) int {
	status := http.StatusInternalServerError
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		switch pe.Code() {
		case messages.ErrorPluginUnavailable:
			status = http.StatusServiceUnavailable
			retryAfter(w, 5)
		case messages.ErrorPluginBusy:
			status = http.StatusServiceUnavailable
			retryAfter(w, 1)
		case messages.ErrorPluginTimeout:
			status = http.StatusGatewayTimeout
		case messages.ErrorPluginRouteFailed:
			status = http.StatusBadGateway
		case messages.ErrorPluginNotInstalled:
			e.a.routingError(http.StatusNotFound)(w, r)
			return http.StatusNotFound
		}
	}
	writeErr(w, status, err)
	return status
}

// passProductError passes on a plugin's product error when its body is
// one and its status a failure; anything else is refused.
func (e *dynamicEndpoint) passProductError(w http.ResponseWriter, route *DynamicRoute, resp *http.Response) int {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPluginErrorBody+1))
	if err != nil || len(raw) > maxPluginErrorBody || resp.StatusCode < 400 || resp.StatusCode > 599 || e.ext.DecodeError == nil {
		return invalidResponse(w, route, "the product error is not one")
	}
	pe, err := e.ext.DecodeError(raw)
	if err != nil {
		return invalidResponse(w, route, "the product error is malformed: "+err.Error())
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		w.Header().Set("Retry-After", v)
	}
	writeErr(w, resp.StatusCode, pe)
	return resp.StatusCode
}

func invalidResponse(w http.ResponseWriter, route *DynamicRoute, why string) int {
	writeProductError(w, http.StatusBadGateway, messages.ErrorPluginRouteResponseInvalid, "the plugin returned a response the server will not pass on: "+why, map[string]any{"id": route.Plugin})
	return http.StatusBadGateway
}

// locationInside reports whether loc is a path under prefix, the
// route's plugin prefix, in canonical form: no scheme, host or escape.
func locationInside(loc, prefix string) bool {
	u, err := url.Parse(loc)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || strings.HasPrefix(loc, "//") {
		return false
	}
	p := u.EscapedPath()
	return strings.HasPrefix(p, prefix) && canonicalRoutePath(strings.TrimPrefix(p, "/api/v1"+extensionRoot))
}

// passCookies adds the plugin's cookies to h, each held to the route's
// prefix and this host; a core cookie's name or a __Host- or __Secure-
// cookie is dropped. A cookie the core set on h (a renewed session)
// stays.
func (e *dynamicEndpoint) passCookies(h http.Header, route *DynamicRoute, lines []string) {
	for _, line := range lines {
		c, err := http.ParseSetCookie(line)
		if err != nil || coreCookie(c.Name, e.a.sessionCookie()) || hasPrefixFold(c.Name, "__Host-") || hasPrefixFold(c.Name, "__Secure-") {
			continue
		}
		c.Path, c.Domain = route.prefix(), ""
		if v := c.String(); v != "" {
			h.Add("Set-Cookie", v)
		}
	}
}

// copyStream copies a streamed body, flushing every write, and cuts it off
// when the plugin writes nothing for the idle time.
func (e *dynamicEndpoint) copyStream(w http.ResponseWriter, body io.Reader, cancel context.CancelFunc) {
	idle := e.ext.StreamIdle
	if idle <= 0 {
		idle = defaultStreamIdle
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.Flush()
	timer := time.AfterFunc(idle, cancel)
	defer timer.Stop()
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			timer.Reset(idle)
			if _, werr := w.Write(buf[:n]); werr != nil {
				panic(http.ErrAbortHandler)
			}
			_ = rc.Flush()
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}
