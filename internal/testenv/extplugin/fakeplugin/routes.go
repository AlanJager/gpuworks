package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// routeHandler serves every route the installed manifest declares, each
// behind RequirePrincipal with the action of the route it matched itself,
// so a request the server signed for another route is refused. Each call
// it serves is appended to results.jsonl ({"route": action, "kind",
// "user", "access"}) and answered with the same as JSON, or as text/html
// with html; setCookie adds a Set-Cookie line to every answer. With
// stallBodies it never reads a request body: a call with one waits until
// it ends, then is recorded. check is how the principal is checked; any
// other value than checkFull breaks the rule on purpose.
func routeHandler(html, stallBodies bool, setCookie []string, check string) http.Handler {
	mux := http.NewServeMux()
	for _, r := range manifestRoutes() {
		pattern := r.Method + " " + extapi.PathRoutePrefix + r.Pattern
		if r.Pattern == "/" {
			pattern += "{$}"
		}
		action := r.Action
		mux.Handle(pattern, guard(check, action, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if stallBodies && req.ContentLength != 0 {
				<-req.Context().Done()
			} else {
				_, _ = io.Copy(io.Discard, req.Body)
			}
			line := map[string]any{"route": action}
			if p := pluginsdk.PrincipalFrom(req); p != nil {
				line["kind"], line["user"], line["access"] = p.Kind, p.Username, pluginsdk.FeatureAccessFrom(req).String()
			}
			appendResult(line)
			for _, c := range setCookie {
				w.Header().Add("Set-Cookie", c)
			}
			if html {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, "<p>"+action+"</p>")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(line)
		})))
	}
	return mux
}

// manifestRoutes are the routes of the manifest above the executable.
func manifestRoutes() []extapi.RouteDecl {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	for dir := filepath.Dir(exe); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			continue
		}
		var m struct {
			Routes []extapi.RouteDecl `json:"routes"`
		}
		_ = json.Unmarshal(raw, &m)
		return m.Routes
	}
	return nil
}

// How a route checks its principal.
const (
	checkFull    = "full"     // RequirePrincipal: the MAC and the route action
	checkSkip    = "skip"     // --skip-principal-check: nothing at all
	checkMACOnly = "mac_only" // --ignore-route-action: the MAC, not the action
)

// guard wraps a route's handler with the principal check check names.
func guard(check, action string, h http.Handler) http.Handler {
	switch check {
	case checkSkip:
		return h
	case checkMACOnly:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := pluginsdk.PluginFrom(r.Context()).Request().PrincipalKey
			if _, _, err := extapi.VerifyPrincipal(key, r.Header.Get(extapi.HeaderPrincipal),
				r.Header.Get(extapi.HeaderPrincipalMac), r.Header.Get(extapi.HeaderFeatureAccess), time.Now()); err != nil {
				w.Header().Set(extapi.HeaderProductError, "1")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	return pluginsdk.RequirePrincipal(action, h)
}
