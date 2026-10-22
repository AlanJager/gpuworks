package app

import (
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/agentbin"
	"gpuworks.ai/gpuworks/internal/server/offlinepkg"
)

// withDL serves embedded (or override-dir) agent binaries and the offline
// package built from them at /dl/ in front of the SPA handler.
func withDL(dir string, offline http.Handler, next http.Handler) http.Handler {
	dl := agentbin.Handler(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offlinepkg.Handles(r.URL.Path) {
			offline.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			dl.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
