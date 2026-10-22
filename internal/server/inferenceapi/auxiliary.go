package inferenceapi

import "net/http"

// An auxiliary route is a gateway path that is not inference: the engine
// answers it from its tokenizer alone, without scheduling anything on the
// GPU. The gateway routes it by the request's model like any /v1 request,
// but forwards it to the engine path the engine actually serves and does
// not count it as traffic. The table is code-owned: it is not part of a
// template's contract and not a discovery route (those are GET-only).
type AuxiliaryRoute struct {
	// Path is the gateway path a client calls.
	Path string
	// Upstream is the engine path the request is forwarded to. vLLM serves
	// tokenize only at the root; SGLang serves it at the root and under /v1.
	Upstream string
	// Method is the one method the route takes.
	Method string
}

var auxiliaryRoutes = []AuxiliaryRoute{
	{Path: "/v1/tokenize", Upstream: "/tokenize", Method: http.MethodPost},
	{Path: "/v1/detokenize", Upstream: "/detokenize", Method: http.MethodPost},
}

// AuxiliaryRouteFor returns the auxiliary route a gateway path names.
func AuxiliaryRouteFor(path string) (AuxiliaryRoute, bool) {
	for _, route := range auxiliaryRoutes {
		if route.Path == path {
			return route, true
		}
	}
	return AuxiliaryRoute{}, false
}

// AuxiliaryRoutes lists every auxiliary route, in a copy the caller may keep.
func AuxiliaryRoutes() []AuxiliaryRoute {
	return append([]AuxiliaryRoute(nil), auxiliaryRoutes...)
}
