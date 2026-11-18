package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A path that is not canonical is refused before any route is matched or
// any caller authenticated, so the server and the plugin's router can
// never read one path two ways.
func TestDynamicRoutesRefuseNonCanonicalPaths(t *testing.T) {
	rig := newDynamicRig(t, []DynamicRoute{dynRoute("GET", "/pub/{x}", DynamicPublic), dynRoute("GET", "/admin", DynamicAdmin), dynRoute("GET", "/b", DynamicPublic)})
	for _, path := range []string{
		"/api/v1/x/acme_fake/pub%2F..%2Fadmin",
		"/api/v1/x/acme_fake/pub%2f..%2fadmin",
		"/api/v1/x/acme_fake/a/../b",
		"/api/v1/x/acme_fake/./b",
		"/api/v1/x/acme_fake//b",
		"/api/v1/x/acme_fake/pub/%2e%2e",
		"/api/v1/x/acme_fake/pub/a%5cb",
		"/api/v1/x/acme_fake/pub/a\\b",
		"/api/v1/x/acme_fake/pub/%252F",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path, req.URL.RawPath = path, path
		if unescaped, err := pathUnescape(path); err == nil {
			req.URL.Path = unescaped
		}
		rec := serve(rig.h, req)
		if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != `{"code":"PLUGIN_ROUTE_PATH_INVALID","error":"the request path is not in canonical form"}` {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
	// A path that names no plugin is no plugin's path: the core's answer
	// for an unknown path (401 unauthenticated, else 404), as with nothing
	// installed.
	for _, path := range []string{"/api/v1/x", "/api/v1/x/", "/api/v1/x//acme_fake/b"} {
		if rec := rig.do("", http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s = %d %s", path, rec.Code, rec.Body)
		}
		if rec := rig.do(rig.session, http.MethodGet, path, ""); rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != `{"code":"NOT_FOUND","error":"Not Found"}` {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
	if rig.dyn.calls.Load() != 0 {
		t.Fatalf("a non-canonical path reached the plugin %d times", rig.dyn.calls.Load())
	}
	// A canonical path with an ordinary escape matches and reaches the
	// plugin as it was sent.
	rec := rig.do("", http.MethodGet, "/api/v1/x/acme_fake/pub/a%20b?q=1", "")
	if rec.Code != http.StatusOK || rig.dyn.last.Load().path != "/pub/a%20b?q=1" {
		t.Fatalf("escaped segment = %d %s, forwarded %q", rec.Code, rec.Body, rig.dyn.last.Load().path)
	}
}

func pathUnescape(p string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, "http://h"+p, nil)
	if err != nil {
		return "", err
	}
	return req.URL.Path, nil
}

// The plugin hears none of the client's credentials or of the headers
// only the server sets, and gets a trace context.
func TestDynamicRoutesStripInboundHeaders(t *testing.T) {
	rig := newDynamicRig(t, []DynamicRoute{dynRoute("GET", "/items", DynamicMember), dynRoute("GET", "/sso", DynamicPublic, func(r *DynamicRoute) { r.Cookies = true })})
	req := httptestRequest(http.MethodGet, "/api/v1/x/acme_fake/items", rig.member)
	for k, v := range map[string]string{"Gwx-Principal": "forged", "Gwx-Principal-Mac": "forged", "Gwx-Feature-Access": "acme.backup=full",
		"X-Gpuworks-Usage-Error": "x", "Cookie": "gpuworks_session=abc; theme=dark", "Traceparent": "bogus", "Connection": "X-Secret", "X-Secret": "1", "X-Kept": "yes",
		"X-Forwarded-For": "10.9.9.9", "X-Real-Ip": "10.9.9.9", "Forwarded": "for=10.9.9.9"} {
		req.Header.Set(k, v)
	}
	if rec := serve(rig.h, req); rec.Code != http.StatusOK {
		t.Fatalf("member read = %d %s", rec.Code, rec.Body)
	}
	h := rig.dyn.last.Load().header
	for _, k := range []string{"Authorization", "Gwx-Principal", "Gwx-Principal-Mac", "Gwx-Feature-Access", "X-Gpuworks-Usage-Error", "Cookie", "X-Secret",
		"X-Forwarded-For", "X-Real-Ip", "Forwarded"} {
		if h.Get(k) != "" {
			t.Errorf("the plugin got %s: %q", k, h.Get(k))
		}
	}
	if h.Get("X-Kept") != "yes" || !validTraceparent(h.Get("Traceparent")) {
		t.Fatalf("forwarded headers %v", h)
	}

	// A cookies route gets the client's own cookies, never the core's or a
	// __Host- one; a valid trace context passes as it is.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/x/acme_fake/sso", nil)
	req.Header.Set("Cookie", "gpuworks_session=abc; GPUWORKS_X=1; __Host-sid=2; saml_relay=r1")
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req.Header.Set("Traceparent", tp)
	if rec := serve(rig.h, req); rec.Code != http.StatusOK {
		t.Fatalf("cookies route = %d %s", rec.Code, rec.Body)
	}
	if h := rig.dyn.last.Load().header; h.Get("Cookie") != "saml_relay=r1" || h.Get("Traceparent") != tp {
		t.Fatalf("cookies route headers %v", h)
	}
}

// What a plugin answers reaches the client only through the allowlist,
// always sandboxed and never sniffed; HTML only from a route that says it
// sends HTML.
func TestDynamicRoutesFilterResponses(t *testing.T) {
	rig := newDynamicRig(t, []DynamicRoute{
		dynRoute("GET", "/page", DynamicAdmin),
		dynRoute("GET", "/html", DynamicAdmin, func(r *DynamicRoute) { r.HTML = true }),
		dynRoute("GET", "/blob", DynamicAdmin),
		dynRoute("GET", "/text", DynamicAdmin),
		dynRoute("GET", "/list", DynamicAdmin),
		dynRoute("GET", "/comma", DynamicAdmin),
	})
	rig.dyn.serve(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		switch r.URL.Path {
		case "/page", "/html":
			h.Set("Content-Type", "text/html; charset=utf-8")
		case "/blob":
			h["Content-Type"] = nil
		case "/text":
			h.Set("Content-Type", "Text/Plain;charset=utf-8")
		case "/list":
			h.Set("Content-Type", "text/plain, text/html")
		case "/comma":
			h.Set("Content-Type", "text/html,")
		}
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Content-Security-Policy", "default-src *")
		h.Set("X-Powered-By", "acme")
		h.Set("Cache-Control", "no-store")
		h.Set("Location", "https://elsewhere.example/")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<b>hi</b>")
	})
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/x/acme_fake/page", "")
	if rec.Code != http.StatusBadGateway || strings.TrimSpace(rec.Body.String()) !=
		`{"code":"PLUGIN_ROUTE_RESPONSE_INVALID","error":"the plugin returned a response the server will not pass on: the route does not declare html","params":{"id":"acme_fake"}}` {
		t.Fatalf("undeclared html = %d %s", rec.Code, rec.Body)
	}
	// A Content-Type that is not one media type could read as HTML in a
	// browser, which takes the last entry of a list.
	for _, path := range []string{"/list", "/comma"} {
		rec = rig.do(rig.session, http.MethodGet, "/api/v1/x/acme_fake"+path, "")
		if rec.Code != http.StatusBadGateway || strings.TrimSpace(rec.Body.String()) !=
			`{"code":"PLUGIN_ROUTE_RESPONSE_INVALID","error":"the plugin returned a response the server will not pass on: the content type is malformed","params":{"id":"acme_fake"}}` {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
	for path, ct := range map[string]string{"/html": "text/html; charset=utf-8", "/blob": "application/octet-stream", "/text": "text/plain; charset=utf-8"} {
		rec = rig.do(rig.session, http.MethodGet, "/api/v1/x/acme_fake"+path, "")
		h := rec.Header()
		if rec.Code != http.StatusOK || rec.Body.String() != "<b>hi</b>" || h.Get("Content-Type") != ct || h.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s = %d %q %v", path, rec.Code, rec.Body, h)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox; default-src 'none'" ||
			h.Get("Access-Control-Allow-Origin") != "" || h.Get("X-Powered-By") != "" || h.Get("Location") != "" {
			t.Fatalf("%s headers %v", path, h)
		}
	}
}

// A redirect stays inside the plugin's own prefix, or loses its Location.
func TestLocationStaysInsideThePluginPrefix(t *testing.T) {
	prefix := "/api/v1/x/acme_fake/"
	for loc, want := range map[string]bool{
		"/api/v1/x/acme_fake/jobs/7":              true,
		"/api/v1/x/acme_fake/":                    true,
		"/api/v1/x/acme_other/jobs":               false,
		"/api/v1/x/acme_fake/../nodes":            false,
		"/api/v1/x/acme_fake/%2e%2e/a":            false,
		"//elsewhere.example/api/v1/x/acme_fake/": false,
		"https://elsewhere.example/":              false,
		"/api/v1/nodes":                           false,
		"jobs/7":                                  false,
	} {
		if got := locationInside(loc, prefix); got != want {
			t.Errorf("locationInside(%q) = %t", loc, got)
		}
	}
}

// Only a public cookies route passes cookies, each held to the plugin's
// prefix and host; core names and prefixed cookies are dropped. On other
// routes every plugin cookie is dropped, and the session the core renewed
// still reaches the client.
func TestDynamicRoutesFilterCookies(t *testing.T) {
	rig := newDynamicRig(t, []DynamicRoute{
		dynRoute("POST", "/acs", DynamicPublic, func(r *DynamicRoute) { r.Cookies = true }),
		dynRoute("POST", "/items", DynamicMember),
	})
	rig.dyn.serve(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range []string{"gpuworks_session=stolen; Path=/", "__Host-x=1; Path=/; Secure", "__Secure-y=1; Secure", "sso=ok; Path=/; Domain=example.com; HttpOnly"} {
			w.Header().Add("Set-Cookie", c)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rec := rig.do("", http.MethodPost, "/api/v1/x/acme_fake/acs", "")
	if got := rec.Header().Values("Set-Cookie"); rec.Code != http.StatusNoContent || len(got) != 1 || got[0] != "sso=ok; Path=/api/v1/x/acme_fake/; HttpOnly" {
		t.Fatalf("public cookies route = %d, cookies %q", rec.Code, got)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/x/acme_fake/items", nil)
	req.AddCookie(&http.Cookie{Name: managementCookie, Value: rig.sessionCookie})
	rec = serve(rig.h, req)
	got := rec.Header().Values("Set-Cookie")
	if rec.Code != http.StatusNoContent || len(got) != 1 || !strings.HasPrefix(got[0], managementCookie+"="+rig.sessionCookie+";") {
		t.Fatalf("member route = %d %s, cookies %q", rec.Code, rec.Body, got)
	}
}
