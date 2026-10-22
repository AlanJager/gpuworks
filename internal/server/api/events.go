package api

import (
	"encoding/json"
	"net/http"
	"time"
)

// events streams change notifications to the UI over Server-Sent Events.
// The UI re-fetches affected resources on each event (events carry no payload),
// which keeps the stream tiny and the client logic idempotent.
func (a *apiHandlers) events(w http.ResponseWriter, r *http.Request) {
	if !a.managementStreamAllowed(r, false) {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := a.d.Bus.Subscribe(128) // all topics
	defer sub.Close()

	// Prime the client so it does an initial fetch on connect.
	writeSSE(w, "node", map[string]string{"reason": "connected"})
	fl.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	authTicker := time.NewTicker(time.Second)
	defer authTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-authTicker.C:
			if !a.managementStreamAllowed(r, false) {
				return
			}
		case ev := <-sub.C():
			if !a.managementStreamAllowed(r, false) {
				return
			}
			if ev.Topic == "management-session" {
				continue
			}
			writeSSE(w, ev.Topic, map[string]string{"key": ev.Key, "reason": ev.Reason})
			fl.Flush()
		case <-ticker.C:
			// Keepalive comment frame.
			_, _ = w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	_, _ = w.Write([]byte("event: " + event + "\ndata: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))
}
