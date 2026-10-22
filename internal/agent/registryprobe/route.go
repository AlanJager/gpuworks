package registryprobe

import (
	"net/http"
	"net/url"
	"strings"

	"gpuworks.ai/gpuworks/pkg/httpproxy"
)

// HostResult is what the prober knows about one registry host.
type HostResult struct {
	Host      string
	Reachable bool
	LatencyMS int64
	Cooled    bool
}

// Route is the network path a probe takes: it has to be the path the pull
// takes, or the ranking measures a link the image never uses. Key tells two
// routes apart for the result cache and is never logged (it may hold proxy
// credentials).
type Route struct {
	Key   string
	Proxy func(*http.Request) (*url.URL, error)
}

// DirectRoute connects without any proxy.
var DirectRoute = Route{Key: "direct", Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}

// RouteFor is the route through an explicit HTTP(S) proxy that skips the
// hosts listed in noProxy. With no proxy at all it is the route a Go program
// takes from its environment.
func RouteFor(httpProxy, httpsProxy, noProxy string) Route {
	httpProxy, httpsProxy = strings.TrimSpace(httpProxy), strings.TrimSpace(httpsProxy)
	if httpProxy == "" && httpsProxy == "" {
		return Route{Key: "env", Proxy: http.ProxyFromEnvironment}
	}
	return Route{
		Key: "proxy|" + httpProxy + "|" + httpsProxy + "|" + noProxy,
		Proxy: func(req *http.Request) (*url.URL, error) {
			return httpproxy.Proxy(req, httpproxy.Config{HTTPProxy: httpProxy, HTTPSProxy: httpsProxy, NoProxy: noProxy}, false)
		},
	}
}

func defaultTransport(route Route) http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = route.Proxy
	tr.DisableKeepAlives = true
	return tr
}
