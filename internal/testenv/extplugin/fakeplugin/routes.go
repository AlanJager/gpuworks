package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"

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
// it ends, then is recorded.
func routeHandler(html, stallBodies bool, setCookie []string) http.Handler {
	mux := http.NewServeMux()
	for _, r := range manifestRoutes() {
		pattern := r.Method + " " + extapi.PathRoutePrefix + r.Pattern
		if r.Pattern == "/" {
			pattern += "{$}"
		}
		action := r.Action
		mux.Handle(pattern, pluginsdk.RequirePrincipal(action, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if stallBodies && req.ContentLength != 0 {
				<-req.Context().Done()
			} else {
				_, _ = io.Copy(io.Discard, req.Body)
			}
			p := pluginsdk.PrincipalFrom(req)
			line := map[string]any{"route": action, "kind": p.Kind, "user": p.Username, "access": pluginsdk.FeatureAccessFrom(req).String()}
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
