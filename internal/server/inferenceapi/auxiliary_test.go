package inferenceapi

import (
	"net/http"
	"testing"
)

func TestAuxiliaryRoutesMapTokenizeToTheEngineRoot(t *testing.T) {
	for path, upstream := range map[string]string{"/v1/tokenize": "/tokenize", "/v1/detokenize": "/detokenize"} {
		route, ok := AuxiliaryRouteFor(path)
		if !ok || route.Path != path || route.Upstream != upstream || route.Method != http.MethodPost {
			t.Fatalf("%s = %+v ok=%v", path, route, ok)
		}
	}
	// Root paths belong to the console; inference paths are not auxiliary.
	for _, path := range []string{"/tokenize", "/detokenize", "/v1/tokenize/", "/v1/chat/completions", "/v1/models", ""} {
		if route, ok := AuxiliaryRouteFor(path); ok {
			t.Fatalf("%q is auxiliary: %+v", path, route)
		}
	}
	routes := AuxiliaryRoutes()
	if len(routes) != 2 {
		t.Fatalf("routes = %+v", routes)
	}
	routes[0].Upstream = "/mutated"
	if route, _ := AuxiliaryRouteFor("/v1/tokenize"); route.Upstream != "/tokenize" {
		t.Fatal("a caller mutated the table")
	}
}
