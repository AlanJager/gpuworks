package conformance

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// routeCase is how a route rule signs its requests.
type routeCase int

const (
	routeSigned      routeCase = iota // as the server signs it
	routeUnsigned                     // no principal headers at all
	routeWrongKey                     // signed under another key
	routeOtherAction                  // signed for another route's action
)

// routeCallTimeout bounds one route request.
const routeCallTimeout = 10 * time.Second

// pathParam fills every parameter segment of a route pattern.
const pathParam = "conformance"

// checkRoutes returns the rule that sends each declared route one request
// signed as c says: the signed request must not be refused for its
// principal, every other one must be refused with 403.
func (s *suite) checkRoutes(c routeCase) func(*testing.T) {
	return func(t *testing.T) {
		if len(s.m.Routes) == 0 {
			t.Skip("the manifest declares no route")
		}
		h, _, _ := s.initialize(t)
		for _, r := range s.m.Routes {
			t.Run(r.Method+" "+r.Pattern, func(t *testing.T) {
				resp, body := s.callRoute(t, h, r, c)
				refusedForPrincipal := resp.StatusCode == http.StatusForbidden && resp.Header.Get(extapi.HeaderProductError) != ""
				switch {
				case c == routeSigned && refusedForPrincipal:
					t.Errorf("a request signed for %s was refused with 403: %s", r.Action, body)
				case c != routeSigned && resp.StatusCode != http.StatusForbidden:
					t.Errorf("%s was answered with %d, not refused with 403: %s", c.describe(), resp.StatusCode, body)
				}
			})
		}
	}
}

func (c routeCase) describe() string {
	switch c {
	case routeUnsigned:
		return "a request without a principal"
	case routeWrongKey:
		return "a request whose principal was signed under another key"
	case routeOtherAction:
		return "a request signed for another route's action"
	}
	return "a signed request"
}

// callRoute sends r one request signed as c says and returns the answer
// and the start of its body.
func (s *suite) callRoute(t *testing.T, h *plugintest.Harness, r extapi.RouteDecl, c routeCase) (*http.Response, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), routeCallTimeout)
	t.Cleanup(cancel)
	var body io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, routePath(r.Pattern), body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(extapi.HeaderDeadline, strconv.FormatInt(time.Now().Add(routeCallTimeout).UnixMilli(), 10))
	if c != routeUnsigned {
		p := s.principal(h, r)
		if c == routeOtherAction {
			p.RouteAction = s.otherAction(r.Action)
		}
		key := h.PrincipalKey()
		if c == routeWrongKey {
			key = bytes.Repeat([]byte{0x5a}, extapi.PrincipalKeyBytes)
		}
		access := extapi.FeatureAccess{}
		if r.Feature != "" {
			access[r.Feature] = extapi.AccessFull
		}
		hdr, mac, err := extapi.SignPrincipal(key, p, access.String())
		if err != nil {
			t.Fatalf("signing the principal: %v", err)
		}
		req.Header.Set(extapi.HeaderPrincipal, hdr)
		req.Header.Set(extapi.HeaderPrincipalMac, mac)
		req.Header.Set(extapi.HeaderFeatureAccess, access.String())
	}
	resp, err := h.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", r.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp, string(raw)
}

// principal is what the server signs for a request to r: a signed-in
// platform administrator, or nobody for a public route, expiring after
// the route's timeout.
func (s *suite) principal(h *plugintest.Harness, r extapi.RouteDecl) *extapi.Principal {
	p := h.Principal(r.Action, r.Pattern)
	if r.Access == extapi.RoutePublic {
		p = &extapi.Principal{Kind: extapi.PrincipalPublic, RequestID: p.RequestID, RouteAction: r.Action, Route: r.Pattern}
	}
	p.Exp = extapi.PrincipalExpiry(time.Now(), r.TimeoutS)
	return p
}

// otherAction is the action of the next declared route, in manifest order,
// whose action differs from action, so a plugin that accepts any of its
// own actions on every route fails; a package whose routes all share one
// action gets an action no route declares.
func (s *suite) otherAction(action string) string {
	for i, r := range s.m.Routes {
		if r.Action != action {
			continue
		}
		for j := 1; j < len(s.m.Routes); j++ {
			if next := s.m.Routes[(i+j)%len(s.m.Routes)].Action; next != action {
				return next
			}
		}
		break
	}
	declared := func(a string) bool {
		return slices.ContainsFunc(s.m.Routes, func(r extapi.RouteDecl) bool { return r.Action == a })
	}
	other := s.m.ID + ".conformance_other"
	for declared(other) || other == action {
		other += "x"
	}
	return other
}

// routePath is the path the server forwards a request for pattern to, with
// every parameter filled.
func routePath(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") {
			segs[i] = pathParam
		}
	}
	return "http://gwx-plugin" + extapi.PathRoutePrefix + strings.Join(segs, "/")
}
