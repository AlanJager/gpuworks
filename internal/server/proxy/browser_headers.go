package proxy

import "net/http"

// browserContextHeaders describe the page a browser request came from and the
// browser's session with the control plane: the console origin, the console
// URL, and the console's own session cookie. None of them belong upstream.
//
// An engine reached through the platform is not the site the browser talks
// to: the platform has already authenticated the caller and decided the
// request may reach this model, and it forwards the request as a server-side
// client. Origin-aware engine servers (Strata's "allowed_hosts" /
// "cors_origins", WebSocket handshakes in general) treat a console origin
// they were never configured for as a cross-site page and refuse the request,
// and the session cookie would hand the management login to a third-party
// engine process.
var browserContextHeaders = []string{"Origin", "Referer", "Cookie"}

// stripBrowserContext removes the browser-context headers from a request the
// proxy forwards upstream.
func stripBrowserContext(h http.Header) {
	for _, name := range browserContextHeaders {
		h.Del(name)
	}
}
