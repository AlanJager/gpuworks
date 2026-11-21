package testconn

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// proxyVariables are the environment variables that decide whether requests
// go through a proxy.
var proxyVariables = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"}

// errProxyConfig stands in for a proxy setting that cannot be used. The
// underlying error quotes the variable's value, which may hold the proxy's
// password, so it is dropped rather than logged.
var errProxyConfig = errors.New("proxy configuration is invalid")

// errProxyRefused is a proxy that answered but would not open a tunnel to the
// endpoint.
var errProxyRefused = errors.New("proxy refused to connect to the endpoint")

// route is how requests to the endpoint leave the server. hop is the first
// address the server itself connects to: the proxy when one applies, the
// endpoint otherwise.
type route struct {
	hop   *url.URL
	proxy *url.URL
	err   error
}

func (rt route) viaProxy() bool { return rt.proxy != nil }

// proxyFor is the runner's proxy decision with the error text removed.
func (r *Runner) proxyFor(req *http.Request) (*url.URL, error) {
	if r.Proxy == nil {
		return nil, nil
	}
	proxy, err := r.Proxy(req)
	if err != nil {
		return nil, errProxyConfig
	}
	return proxy, nil
}

// routeFor resolves the route of the endpoint's requests. With an unusable
// proxy setting the route stays the endpoint itself, so the direct checks
// still say what they can, and proxy_detect reports the setting.
func (r *Runner) routeFor(endpoint *url.URL) route {
	proxy, err := r.proxyFor(&http.Request{URL: endpoint})
	if err != nil || proxy == nil {
		return route{hop: endpoint, err: err}
	}
	return route{hop: proxy, proxy: proxy}
}

func proxyFailure(err error) *Failure {
	return &Failure{Code: messages.ErrorProviderProxyFailed, Hint: HintProxy,
		Detail: map[string]any{"target": "proxy"}, Err: err}
}

// checkProxy names the proxy variables that are set and says whether the
// endpoint's requests go through a proxy. It contacts nothing (dns_resolve
// and tcp_connect reach the proxy), and no variable's value leaves the server.
func (r *Runner) checkProxy(rt route) (map[string]any, error) {
	if rt.err != nil {
		return nil, &Failure{Code: messages.ErrorProviderProxyFailed, Hint: HintProxy, Err: rt.err}
	}
	names := []string{}
	if r.Getenv != nil {
		for _, key := range proxyVariables {
			if strings.TrimSpace(r.Getenv(key)) != "" {
				names = append(names, key)
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 && !rt.viaProxy() {
		return map[string]any{"proxy": "none"}, nil
	}
	return map[string]any{"proxy": "env", "variables": names, "used": rt.viaProxy()}, nil
}
