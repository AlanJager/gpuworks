package app

import (
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/installscript"
)

// installHandler serves the install script.
func (a *App) installHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript")
		_, _ = w.Write([]byte(installscript.Script))
	}
}
