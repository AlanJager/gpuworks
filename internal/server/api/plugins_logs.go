package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// PluginLogCursor is where a reader of a plugin's logs stands: the last
// supervision event it read and the stderr offset it reads from next.
type PluginLogCursor struct {
	Event  uint64
	Stderr uint64
}

// PluginLogs are what a plugin logged after a cursor.
type PluginLogs struct {
	// Events are its supervision events, oldest first; EventsTruncated
	// reports that some after the cursor were dropped.
	Events          []dto.PluginEvent
	EventsTruncated bool
	// Stderr is its stderr output from StderrFrom on; StderrTruncated
	// reports that some after the cursor was dropped.
	Stderr          []byte
	StderrFrom      uint64
	StderrTruncated bool
	Next            PluginLogCursor
}

// pluginLogKeepalive is how often an idle logs stream sends a comment
// frame.
var pluginLogKeepalive = 20 * time.Second

// pluginLogs streams a plugin's logs over Server-Sent Events to an
// administrator's login session: first everything kept (the last 256
// supervision events as "state" frames, the last 1 MiB of stderr as one
// "stderr" frame), then each new event and output as it comes. The stream
// ends when the login ends or loses its administrator role, and the
// assistant never reads it (pluginAdmin).
func (a *apiHandlers) pluginLogs(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	logs, changed, err := p.Logs(r.Context(), id, PluginLogCursor{})
	if err != nil {
		writePluginError(w, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errStreamingUnsupported)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Only a change to a login can revoke this one between rechecks.
	var sessions <-chan bus.Event
	if a.d.Bus != nil {
		sub := a.d.Bus.Subscribe(16, "management-session")
		defer sub.Close()
		sessions = sub.C()
	}
	keepalive := time.NewTicker(pluginLogKeepalive)
	defer keepalive.Stop()
	authTicker := time.NewTicker(streamAuthRecheck)
	defer authTicker.Stop()
	for {
		writePluginLogFrames(w, logs)
		fl.Flush()
		next := logs.Next
	wait:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-authTicker.C:
				if !a.managementStreamAllowed(r, true) {
					return
				}
				if changed == nil {
					// Nothing to follow yet: look again for a plugin
					// that has started since.
					break wait
				}
			case <-sessions:
				if !a.managementStreamAllowed(r, true) {
					return
				}
			case <-keepalive.C:
				_, _ = w.Write([]byte(": ping\n\n"))
				fl.Flush()
			case <-changed:
				break wait
			}
		}
		if logs, changed, err = p.Logs(r.Context(), id, next); err != nil {
			// The plugin was uninstalled while the stream was open.
			return
		}
	}
}

// writePluginLogFrames writes logs as SSE frames: one "state" frame per
// event, then one "stderr" frame for the output, if any.
func writePluginLogFrames(w http.ResponseWriter, logs PluginLogs) {
	for i, e := range logs.Events {
		if i == 0 && logs.EventsTruncated {
			e.Truncated = true
		}
		writeSSE(w, "state", e)
	}
	if len(logs.Stderr) > 0 || logs.StderrTruncated {
		writeSSE(w, "stderr", dto.PluginStderr{Offset: logs.StderrFrom, Text: strings.ToValidUTF8(string(logs.Stderr), "�"),
			Truncated: logs.StderrTruncated})
	}
}
